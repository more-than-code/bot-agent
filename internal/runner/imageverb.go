package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

// handleImage runs one v1.4 verb. Refusals are {"error": code}. Program
// verbs keep the v1.3 ok/reason object. Nothing in in is sent to the Engine
// as a path, query, header, or body.
func handleImage(ctx context.Context, verb string, in []byte, sys Sys) []byte {
	if ctx.Err() != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	if len(in) > images.MaxFrame {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID,
	})
	if !pol.ImagesEnabled() {
		return imgErr(sys, programs.ReasonPolicy)
	}
	switch verb {
	case images.VerbList:
		return doImageList(ctx, in, sys)
	case images.VerbPull:
		return doPull(ctx, in, pol, sys)
	case images.VerbDeploy:
		return doDeploy(ctx, in, pol, sys)
	case images.VerbRollback:
		return doRollback(ctx, in, pol, sys)
	case images.VerbRemove:
		return doRemove(ctx, in, pol, sys)
	case images.VerbStart:
		return doStartCtr(ctx, in, pol, sys)
	case images.VerbStop:
		return doStopCtr(ctx, in, pol, sys)
	case images.VerbRestart:
		return doRestartCtr(ctx, in, pol, sys)
	case images.VerbStatus:
		return doStatusCtr(ctx, in, pol, sys)
	case images.VerbLogs:
		return doLogsCtr(ctx, in, pol, sys)
	default:
		return imgErr(sys, programs.ReasonVerb)
	}
}

func imgErr(sys Sys, reason string) []byte {
	reason = programs.FixedReason(reason)
	if sys.Logf != nil {
		sys.Logf("bot-agent-runner: refuse %s", reason)
	}
	return images.EncodeError(reason)
}

func engReason(err error) string {
	switch {
	case errors.Is(err, errImage):
		return programs.ReasonImage
	case errors.Is(err, errBadSpec):
		return programs.ReasonBadSpec
	default:
		return programs.ReasonDocker
	}
}

func checkProject(pol policy.Policy, project string) string {
	if !images.ValidProject(project) {
		return programs.ReasonBadSpec
	}
	for _, p := range pol.Projects {
		if p == project {
			return ""
		}
	}
	return programs.ReasonPolicy
}

func dockerSock(sys Sys) string {
	if sys.DockerSock != "" {
		return sys.DockerSock
	}
	return "/var/run/docker.sock"
}

func machineIDPath(sys Sys) string {
	if sys.MachineID != "" {
		return sys.MachineID
	}
	return "/etc/machine-id"
}

// readHost hashes the machine-id file. It never opens the agent credential.
func readHost(sys Sys) (string, string) {
	f, err := os.Open(machineIDPath(sys))
	if err != nil {
		return "", programs.ReasonDocker
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(b) > 256 {
		return "", programs.ReasonDocker
	}
	id, ok := images.HostID(string(b))
	if !ok {
		return "", programs.ReasonDocker
	}
	return id, ""
}

// entryLockCap bounds root-owned per-entry flock files on the /run tmpfs.
// Status and logs do not create one. Mutations do. The contract path stays
// one file per name. 512 is above max_containers (200) and still a fixed
// set. A further new name is policy_denied.
const entryLockCap = 512

var errLockCap = errors.New("entry lock cap")

// lockEntry holds the per-entry flock for the rest of the verb. The path is
// built only from a project and entry that already passed validation.
// A name that would create a file past entryLockCap returns errLockCap.
// Unlock does not remove the file: another process may still have it open.
func lockEntry(root, project, entry string) (func(), error) {
	rel, ok := images.LockPath(project, entry)
	if !ok {
		return nil, errors.New("lock")
	}
	path := joinRoot(root, strings.TrimPrefix(rel, "/"))
	unlockGate, err := lockFile(joinRoot(root, filepath.Join("run", "bot-agent", "image-lock-gate.lock")))
	if err != nil {
		return nil, err
	}
	f, err := openEntryLock(path)
	unlockGate()
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// openEntryLock runs while the gate lock is held. An existing file is reused
// and does not count against the cap.
func openEntryLock(path string) (*os.File, error) {
	if _, err := os.Stat(path); err == nil {
		return os.OpenFile(path, os.O_RDWR, 0o600)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	n, err := entryLockCount(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if n >= entryLockCap {
		return nil, errLockCap
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

func entryLockCount(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ent := range ents {
		name := ent.Name()
		if name == "image-count.lock" || name == "image-lock-gate.lock" {
			continue
		}
		if strings.HasPrefix(name, "image-") && strings.HasSuffix(name, ".lock") {
			n++
		}
	}
	return n, nil
}

// lockCount serialises deploys that consume max_containers. It is acquired
// before the per-entry lock and held until create returns.
func lockCount(root string) (func(), error) {
	return lockFile(joinRoot(root, filepath.Join("run", "bot-agent", "image-count.lock")))
}

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func withEntry(ctx context.Context, sys Sys, project, entry string, fn func(engine, string) []byte) []byte {
	if err := ensureRuntime(sys.Root); err != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	unlock, err := lockEntry(sys.Root, project, entry)
	if errors.Is(err, errLockCap) {
		return imgErr(sys, programs.ReasonPolicy)
	}
	if err != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	defer unlock()
	if ctx.Err() != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	host, code := readHost(sys)
	if code != "" {
		return imgErr(sys, code)
	}
	return fn(newEngine(dockerSock(sys)), host)
}

// withRead is status and logs. Those verbs do not create the per-entry lock.
func withRead(ctx context.Context, sys Sys, fn func(engine, string) []byte) []byte {
	if err := ensureRuntime(sys.Root); err != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	if ctx.Err() != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	host, code := readHost(sys)
	if code != "" {
		return imgErr(sys, code)
	}
	return fn(newEngine(dockerSock(sys)), host)
}

type slot struct {
	found  bool
	id     string
	state  string
	image  string
	labels map[string]string
}

func doImageList(ctx context.Context, in []byte, sys Sys) []byte {
	if images.Decode(in, &images.ListRequest{}) != nil {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	rows, err := newEngine(dockerSock(sys)).List(ctx)
	if err != nil {
		return imgErr(sys, engReason(err))
	}
	return images.Encode(projectRows(rows))
}

func doPull(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	var req images.PullRequest
	if images.Decode(in, &req) != nil {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if code := checkProject(pol, req.Project); code != "" {
		return imgErr(sys, code)
	}
	ref, err := images.Parse(req.Ref)
	if err != nil {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if !images.Match(ref, pol.Images) {
		return imgErr(sys, programs.ReasonPolicy)
	}
	if req.RegistryAuth != nil && badCredential(req.RegistryAuth) {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	digest, id, err := newEngine(dockerSock(sys)).Pull(ctx, ref, req.RegistryAuth)
	if err != nil {
		return imgErr(sys, engReason(err))
	}
	return images.Encode(images.PullResponse{Digest: digest, ImageID: id})
}

func doDeploy(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	var req images.DeployRequest
	if images.Decode(in, &req) != nil {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if code := checkProject(pol, req.Project); code != "" {
		return imgErr(sys, code)
	}
	if !images.ValidEntry(req.Entry) || !images.ValidDigest(req.Digest) || !images.ValidSpecHash(req.SpecHash) {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if req.Spec.Validate() != "" || images.ResolvedEnv(req.Env) != "" || images.CheckResolved(req.Spec.Files, req.Files) != "" {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if code := precheckDeploy(req, pol); code != "" {
		return imgErr(sys, code)
	}
	if err := ensureRuntime(sys.Root); err != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	unlock, err := lockCount(sys.Root)
	if err != nil {
		return imgErr(sys, programs.ReasonDocker)
	}
	defer unlock()
	return withEntry(ctx, sys, req.Project, req.Entry, func(eng engine, host string) []byte {
		body, code := buildCreate(req.Project, req.Entry, req.Digest, req.SpecHash, host, req.Spec, req.Env, req.Files, pol.Images, pol.AllowPublicPorts)
		if code != "" {
			return imgErr(sys, code)
		}
		n, cur, prev, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		if req.Spec.Kind == "job" {
			return deployJob(ctx, sys, eng, req, host, body, n, cur, pol.MaxContainers)
		}
		return deployService(ctx, sys, eng, req, host, body, n, cur, prev, pol.MaxContainers)
	})
}

// precheckDeploy is the part of create that does not need the host id:
// allowlist, digest pin, and public ports. It runs before the flock and
// before any Engine call.
func precheckDeploy(req images.DeployRequest, pol policy.Policy) string {
	ref, err := images.Parse(req.Spec.Image)
	if err != nil {
		return programs.ReasonBadSpec
	}
	pinned, err := images.Parse(ref.Name + "@" + req.Digest)
	if err != nil || !images.Match(ref, pol.Images) || !images.Match(pinned, pol.Images) {
		return programs.ReasonPolicy
	}
	for _, p := range req.Spec.Ports {
		if p.Public && !pol.AllowPublicPorts {
			return programs.ReasonPolicy
		}
	}
	return ""
}

func deployService(ctx context.Context, sys Sys, eng engine, req images.DeployRequest, host string, body []byte, n int, cur, prev slot, max int) []byte {
	if !fits(n, prev.found, max) {
		return imgErr(sys, programs.ReasonPolicy)
	}
	curName, prevName, newName, ok := slotNames(req.Project, req.Entry)
	if !ok {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	st, code := openEntryStore(sys, len(req.Files) > 0)
	if code != "" {
		return imgErr(sys, code)
	}
	defer closeStore(st)
	// Label check already ran. A bad slot stops here, before any delete
	// and before any container is stopped. Leftover cleanup is only of
	// slots that passed that check.
	if st != nil {
		if err := st.preflight(curName, prevName, newName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if err := st.removeIfSafe(newName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if !cur.found {
			if err := st.removeIfSafe(curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
		if !prev.found {
			if err := st.removeIfSafe(prevName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
		if len(req.Files) > 0 {
			if err := st.writeSlot(newName, req.Files); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
	}
	if err := stopRemove(ctx, eng, prev); err != nil {
		return imgErr(sys, engReason(err))
	}
	if st != nil {
		if err := st.removeIfSafe(prevName); err != nil {
			return imgErr(sys, storeCode(err))
		}
	}
	if err := park(ctx, eng, cur, prevName); err != nil {
		return imgErr(sys, engReason(err))
	}
	if st != nil {
		if err := st.rename(curName, prevName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if len(req.Files) > 0 {
			if err := st.rename(newName, curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
	}
	if code := ensureNet(ctx, eng, req.Project, host); code != "" {
		return imgErr(sys, code)
	}
	if code := ensureVols(ctx, eng, req.Project, host, req.Spec.Volumes); code != "" {
		return imgErr(sys, code)
	}
	out, code := createStart(ctx, eng, req.Project, req.Entry, body)
	if code != "" {
		return imgErr(sys, code)
	}
	return out
}

func deployJob(ctx context.Context, sys Sys, eng engine, req images.DeployRequest, host string, body []byte, n int, cur slot, max int) []byte {
	// A job does not remove -prev. A foreign -prev was already a conflict.
	if !fits(n, cur.found, max) {
		return imgErr(sys, programs.ReasonPolicy)
	}
	curName, _, newName, ok := slotNames(req.Project, req.Entry)
	if !ok {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	st, code := openEntryStore(sys, len(req.Files) > 0)
	if code != "" {
		return imgErr(sys, code)
	}
	defer closeStore(st)
	if st != nil {
		if err := st.preflight(curName, newName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if err := st.removeIfSafe(newName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if !cur.found {
			if err := st.removeIfSafe(curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
	}
	if err := stopRemove(ctx, eng, cur); err != nil {
		return imgErr(sys, engReason(err))
	}
	if st != nil {
		if err := st.removeIfSafe(curName); err != nil {
			return imgErr(sys, storeCode(err))
		}
		if len(req.Files) > 0 {
			if err := st.writeSlot(curName, req.Files); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
	}
	if code := ensureNet(ctx, eng, req.Project, host); code != "" {
		return imgErr(sys, code)
	}
	if code := ensureVols(ctx, eng, req.Project, host, req.Spec.Volumes); code != "" {
		return imgErr(sys, code)
	}
	out, code := createStart(ctx, eng, req.Project, req.Entry, body)
	if code != "" {
		return imgErr(sys, code)
	}
	return out
}

func slotNames(project, entry string) (cur, prev, fresh string, ok bool) {
	cur, ok1 := images.ContainerName(project, entry)
	prev, ok2 := images.PrevName(project, entry)
	fresh, ok3 := images.NewSlot(project, entry)
	return cur, prev, fresh, ok1 && ok2 && ok3
}

func doRollback(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	req, code := decodeEntry(in, pol)
	if code != "" {
		return imgErr(sys, code)
	}
	return withEntry(ctx, sys, req.Project, req.Entry, func(eng engine, host string) []byte {
		_, cur, prev, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		if !prev.found {
			return imgErr(sys, programs.ReasonConflict)
		}
		if code := allowPinned(prev.image, pol.Images); code != "" {
			return imgErr(sys, code)
		}
		curName, prevName, _, ok := slotNames(req.Project, req.Entry)
		if !ok {
			return imgErr(sys, programs.ReasonBadSpec)
		}
		st, code := openEntryStore(sys, false)
		if code != "" {
			return imgErr(sys, code)
		}
		defer closeStore(st)
		// A bad slot refuses the rollback before the current container is removed.
		if st != nil {
			if err := st.preflight(curName, prevName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
		// Remove the current container before its slot. A failed remove
		// leaves that container on the slot it was started with.
		if err := stopRemove(ctx, eng, cur); err != nil {
			return imgErr(sys, engReason(err))
		}
		// The current slot is removed only once this call has removed the
		// container, or when there was no container and the slot is an
		// orphan. stopRemove has done one of those two.
		var moved bool
		if st != nil {
			if err := st.removeIfSafe(curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
			var err error
			moved, err = st.renameMoved(prevName, curName)
			if err != nil {
				if moved {
					_ = st.rename(curName, prevName)
				}
				return imgErr(sys, storeCode(err))
			}
		}
		if err := eng.Rename(ctx, prev.id, curName); err != nil {
			// The -prev container is still -prev. Put its slot back
			// beside it. The current container is already gone, and so
			// is its slot.
			if moved {
				if rerr := st.rename(curName, prevName); rerr != nil {
					return imgErr(sys, storeCode(rerr))
				}
			}
			return imgErr(sys, engReason(err))
		}
		if err := eng.Start(ctx, prev.id); err != nil {
			// Both names are the current name, and the container did not
			// start. Put the container back on -prev first, then the slot,
			// so a failed start ends like a failed container rename. Moving
			// only the slot would leave the container on the current name.
			// If the container rename back fails, the slot stays where it
			// is: both are still the current name.
			if rerr := eng.Rename(ctx, prev.id, prevName); rerr != nil {
				return imgErr(sys, engReason(rerr))
			}
			if moved {
				if rerr := st.rename(curName, prevName); rerr != nil {
					return imgErr(sys, storeCode(rerr))
				}
			}
			return imgErr(sys, engReason(err))
		}
		return images.Encode(images.IDResponse{ContainerID: prev.id})
	})
}

func doRemove(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	req, code := decodeEntry(in, pol)
	if code != "" {
		return imgErr(sys, code)
	}
	return withEntry(ctx, sys, req.Project, req.Entry, func(eng engine, host string) []byte {
		_, cur, _, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		curName, _, _, ok := slotNames(req.Project, req.Entry)
		if !ok {
			return imgErr(sys, programs.ReasonBadSpec)
		}
		st, code := openEntryStore(sys, false)
		if code != "" {
			return imgErr(sys, code)
		}
		defer closeStore(st)
		if st != nil {
			if err := st.preflight(curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
		if !cur.found {
			if st != nil {
				if err := st.removeIfSafe(curName); err != nil {
					return imgErr(sys, storeCode(err))
				}
			}
			return encodeAction(missingStatus())
		}
		if err := stopRemove(ctx, eng, cur); err != nil {
			return imgErr(sys, engReason(err))
		}
		if st != nil {
			if err := st.removeIfSafe(curName); err != nil {
				return imgErr(sys, storeCode(err))
			}
		}
		return encodeAction(missingStatus())
	})
}

func doStartCtr(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	return mutateOne(ctx, in, pol, sys, func(ctx context.Context, eng engine, cur slot) ([]byte, string) {
		if !cur.found {
			return encodeAction(missingStatus()), ""
		}
		if err := eng.Start(ctx, cur.id); err != nil {
			return nil, engReason(err)
		}
		return nil, ""
	})
}

func doStopCtr(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	return mutateOne(ctx, in, pol, sys, func(ctx context.Context, eng engine, cur slot) ([]byte, string) {
		if !cur.found {
			return encodeAction(missingStatus()), ""
		}
		if err := stopRunning(ctx, eng, cur); err != nil {
			return nil, engReason(err)
		}
		return nil, ""
	})
}

func doRestartCtr(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	return mutateOne(ctx, in, pol, sys, func(ctx context.Context, eng engine, cur slot) ([]byte, string) {
		if !cur.found {
			return encodeAction(missingStatus()), ""
		}
		if err := stopRunning(ctx, eng, cur); err != nil {
			return nil, engReason(err)
		}
		if err := eng.Start(ctx, cur.id); err != nil {
			return nil, engReason(err)
		}
		return nil, ""
	})
}

func doStatusCtr(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	req, code := decodeEntry(in, pol)
	if code != "" {
		return imgErr(sys, code)
	}
	return withRead(ctx, sys, func(eng engine, host string) []byte {
		_, cur, _, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		if !cur.found {
			return images.Encode(images.StatusResult{Status: missingStatus(), SpecHash: "", FilesHash: ""})
		}
		st, hash, filesHash, code := inspectStatus(ctx, eng, cur.id)
		if code != "" {
			return imgErr(sys, code)
		}
		return images.Encode(images.StatusResult{Status: st, SpecHash: hash, FilesHash: filesHash})
	})
}

func doLogsCtr(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	var req images.LogsRequest
	if images.Decode(in, &req) != nil || !images.ValidEntry(req.Entry) || req.Lines < 1 || req.Lines > 2000 || !images.ValidRedact(req.Redact) {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	if code := checkProject(pol, req.Project); code != "" {
		return imgErr(sys, code)
	}
	name, ok := images.ContainerName(req.Project, req.Entry)
	if !ok {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	return withRead(ctx, sys, func(eng engine, host string) []byte {
		_, cur, _, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		if !cur.found {
			return images.Encode(images.LogsResponse{Container: name, Lines: []string{}, Truncated: false})
		}
		raw, cutRead, err := eng.Logs(ctx, cur.id, req.Lines)
		if err != nil {
			return imgErr(sys, engReason(err))
		}
		text, cutDemux := demux(raw)
		ex := redact.NewExact(req.Redact...)
		lines, cutJournal := redact.JournalTask(splitLines(text), ex)
		ex.Clear()
		if lines == nil {
			lines = []string{}
		}
		return images.Encode(images.LogsResponse{Container: name, Lines: lines, Truncated: cutRead || cutDemux || cutJournal})
	})
}

// mutateOne is start, stop, and restart: label-check both names, run op,
// then project the container. A missing name is state "missing", not conflict.
type slotOp func(context.Context, engine, slot) ([]byte, string)

func mutateOne(ctx context.Context, in []byte, pol policy.Policy, sys Sys, op slotOp) []byte {
	req, code := decodeEntry(in, pol)
	if code != "" {
		return imgErr(sys, code)
	}
	return withEntry(ctx, sys, req.Project, req.Entry, func(eng engine, host string) []byte {
		_, cur, _, code := loadPair(ctx, eng, req.Project, req.Entry, host)
		if code != "" {
			return imgErr(sys, code)
		}
		if out, code := op(ctx, eng, cur); code != "" {
			return imgErr(sys, code)
		} else if out != nil {
			return out
		}
		if !cur.found {
			return encodeAction(missingStatus())
		}
		st, _, _, code := inspectStatus(ctx, eng, cur.id)
		if code != "" {
			return imgErr(sys, code)
		}
		return encodeAction(st)
	})
}

func decodeEntry(in []byte, pol policy.Policy) (images.EntryRequest, string) {
	var req images.EntryRequest
	if images.Decode(in, &req) != nil || !images.ValidEntry(req.Entry) {
		return req, programs.ReasonBadSpec
	}
	if code := checkProject(pol, req.Project); code != "" {
		return req, code
	}
	return req, ""
}

// fits reports whether creating one container stays within max. removeOne
// is the owned container this verb deletes before that create.
func fits(n int, removeOne bool, max int) bool {
	if removeOne {
		n--
	}
	return n+1 <= max
}

func loadPair(ctx context.Context, eng engine, project, entry, host string) (int, slot, slot, string) {
	rows, err := eng.List(ctx)
	if err != nil {
		return 0, slot{}, slot{}, engReason(err)
	}
	curName, ok1 := images.ContainerName(project, entry)
	prevName, ok2 := images.PrevName(project, entry)
	if !ok1 || !ok2 {
		return 0, slot{}, slot{}, programs.ReasonBadSpec
	}
	cur, code := take(rows, curName, project, entry, host)
	if code != "" {
		return 0, slot{}, slot{}, code
	}
	prev, code := take(rows, prevName, project, entry, host)
	if code != "" {
		return 0, slot{}, slot{}, code
	}
	return countOwned(rows), cur, prev, ""
}

func take(rows []listed, name, project, entry, host string) (slot, string) {
	c, ok := findExact(rows, name)
	if !ok {
		return slot{}, ""
	}
	if !labelsOwn(c.Labels, project, entry, host) {
		return slot{}, programs.ReasonConflict
	}
	return slot{found: true, id: c.ID, state: c.State, image: c.Image, labels: c.Labels}, ""
}

func findExact(rows []listed, name string) (listed, bool) {
	for _, c := range rows {
		for _, n := range c.Names {
			if n == name {
				return c, true
			}
		}
	}
	return listed{}, false
}

func labelsOwn(labels map[string]string, project, entry, host string) bool {
	return labels[images.LabelOwner] == images.OwnerValue &&
		labels[images.LabelHost] == host &&
		labels[images.LabelProject] == project &&
		labels[images.LabelEntry] == entry
}

func countOwned(rows []listed) int {
	n := 0
	for _, c := range rows {
		if c.Labels[images.LabelOwner] == images.OwnerValue {
			n++
		}
	}
	return n
}

func stopRunning(ctx context.Context, eng engine, sl slot) error {
	if !sl.found {
		return nil
	}
	if sl.state != "running" && sl.state != "restarting" && sl.state != "paused" {
		return nil
	}
	return eng.Stop(ctx, sl.id)
}

func stopRemove(ctx context.Context, eng engine, sl slot) error {
	if !sl.found {
		return nil
	}
	if err := stopRunning(ctx, eng, sl); err != nil {
		return err
	}
	return eng.Remove(ctx, sl.id)
}

func park(ctx context.Context, eng engine, sl slot, name string) error {
	if !sl.found {
		return nil
	}
	if err := stopRunning(ctx, eng, sl); err != nil {
		return err
	}
	return eng.Rename(ctx, sl.id, name)
}

func ensureNet(ctx context.Context, eng engine, project, host string) string {
	name, ok := images.NetworkName(project)
	if !ok {
		return programs.ReasonBadSpec
	}
	info, err := eng.NetworkInspect(ctx, name)
	if errors.Is(err, errNotFound) {
		if err := eng.NetworkCreate(ctx, name, host, project); err != nil {
			return engReason(err)
		}
		info, err = eng.NetworkInspect(ctx, name)
	}
	if err != nil {
		return engReason(err)
	}
	if info.Driver != "bridge" || !scopeMatch(info.Labels, host, project) {
		return programs.ReasonConflict
	}
	return ""
}

func ensureVols(ctx context.Context, eng engine, project, host string, vols []images.Volume) string {
	for _, v := range vols {
		name, ok := images.VolumeName(project, v.Name)
		if !ok {
			return programs.ReasonBadSpec
		}
		info, err := eng.VolumeInspect(ctx, name)
		if errors.Is(err, errNotFound) {
			if err := eng.VolumeCreate(ctx, name, host, project); err != nil {
				return engReason(err)
			}
			info, err = eng.VolumeInspect(ctx, name)
		}
		if err != nil {
			return engReason(err)
		}
		if info.Driver != "local" || len(info.Options) > 0 || info.Labels[images.LabelProject] != project {
			return programs.ReasonConflict
		}
	}
	return ""
}

func scopeMatch(labels map[string]string, host, project string) bool {
	return labels[images.LabelOwner] == images.OwnerValue &&
		labels[images.LabelHost] == host &&
		labels[images.LabelProject] == project
}

func createStart(ctx context.Context, eng engine, project, entry string, body []byte) ([]byte, string) {
	name, ok := images.ContainerName(project, entry)
	if !ok {
		return nil, programs.ReasonBadSpec
	}
	id, err := eng.Create(ctx, name, body)
	if err != nil {
		return nil, engReason(err)
	}
	if err := eng.Start(ctx, id); err != nil {
		return nil, engReason(err)
	}
	return images.Encode(images.IDResponse{ContainerID: id}), ""
}

func inspectStatus(ctx context.Context, eng engine, id string) (images.ImageStatus, string, string, string) {
	in, err := eng.Inspect(ctx, id)
	if errors.Is(err, errNotFound) {
		return missingStatus(), "", "", ""
	}
	if err != nil {
		return images.ImageStatus{}, "", "", engReason(err)
	}
	return projectInspect(in), specHashOf(in.Labels), filesHashOf(in.Labels), ""
}

func encodeAction(st images.ImageStatus) []byte {
	if st.Ports == nil {
		st.Ports = []int{}
	}
	return images.Encode(images.ActionResult{Status: st})
}

func missingStatus() images.ImageStatus {
	return images.ImageStatus{State: "missing", Ports: []int{}}
}

func projectInspect(in inspected) images.ImageStatus {
	st := images.ImageStatus{
		State:        knownState(in.State),
		RestartCount: in.RestartCount,
		Digest:       repoDigest(in.Image),
		Ports:        hostPortList(in.Ports),
	}
	if in.Name != "" && !strings.ContainsAny(in.Name, "\r\n\x00/") {
		name := in.Name
		st.Container = &name
	}
	if st.State == "exited" || st.State == "dead" {
		code := in.ExitCode
		st.ExitCode = &code
	}
	st.StartedAt = dockerTime(in.StartedAt)
	st.FinishedAt = dockerTime(in.FinishedAt)
	return st
}

// maxListItems is the container_list cap. Deploybot-labelled rows are
// sorted first so an owned container is not dropped to make room for
// an unlabelled one. max_containers is 200, so the labelled set fits.
const maxListItems = 200

func projectRows(rows []listed) images.ListResponse {
	items := make([]images.ContainerInfo, 0, len(rows))
	for _, c := range rows {
		if len(c.Names) == 0 {
			continue
		}
		items = append(items, images.ContainerInfo{
			ID:     c.ID,
			Name:   c.Names[0],
			State:  knownState(c.State),
			Ports:  published(c.Ports),
			Digest: repoDigest(c.Image),
			Labels: keepLabels(c.Labels),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		li := len(items[i].Labels) > 0
		lj := len(items[j].Labels) > 0
		if li != lj {
			return li
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})
	truncated := false
	if len(items) > maxListItems {
		items = items[:maxListItems]
		truncated = true
	}
	return images.ListResponse{Items: items, Truncated: truncated}
}

func knownState(s string) string {
	switch s {
	case "running", "exited", "created", "paused", "restarting", "dead", "missing":
		return s
	default:
		return "dead"
	}
}

func repoDigest(image string) *string {
	i := strings.LastIndex(image, "@")
	if i < 0 {
		return nil
	}
	d := image[i+1:]
	if !images.ValidDigest(d) {
		return nil
	}
	return &d
}

func dockerTime(s string) *string {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return nil
	}
	out := s
	return &out
}

func specHashOf(labels map[string]string) string {
	h := labels[images.LabelSpecHash]
	if !images.ValidSpecHash(h) {
		return ""
	}
	return h
}

func filesHashOf(labels map[string]string) string {
	h := labels[images.LabelFilesHash]
	if !images.ValidSpecHash(h) {
		return ""
	}
	return h
}

func keepLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range []string{images.LabelOwner, images.LabelHost, images.LabelProject, images.LabelEntry, images.LabelSpecHash, images.LabelFilesHash} {
		v, ok := in[k]
		if !ok || strings.ContainsAny(v, "\r\n\x00") {
			continue
		}
		out[k] = v
	}
	return out
}

// allowPinned is the §12.2 re-check. image must be repo@sha256:<64 hex> and
// the repo must match the policy. Anything else is policy_denied.
func allowPinned(image string, patterns []string) string {
	ref, err := images.Parse(image)
	if err != nil || ref.Digest == "" || ref.Tag != "" || !images.Match(ref, patterns) {
		return programs.ReasonPolicy
	}
	return ""
}

func published(ports []portView) []images.PublishedPort {
	out := make([]images.PublishedPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, images.PublishedPort{
			HostIP: p.HostIP, HostPort: p.HostPort, ContainerPort: p.ContainerPort,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPort != out[j].HostPort {
			return out[i].HostPort < out[j].HostPort
		}
		return out[i].ContainerPort < out[j].ContainerPort
	})
	return out
}

func hostPortList(ports []portView) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ports {
		if p.HostPort < 1 || p.HostPort > 65535 || seen[p.HostPort] {
			continue
		}
		seen[p.HostPort] = true
		out = append(out, p.HostPort)
	}
	sort.Ints(out)
	if out == nil {
		return []int{}
	}
	return out
}

// demux splits a Docker multiplexed log stream. A buffer that does not start
// with an 8-byte frame is returned as text. Leftover bytes after a good frame
// set truncated.
func demux(b []byte) (string, bool) {
	if !frameHeader(b) {
		return string(b), false
	}
	var out []byte
	for frameHeader(b) {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		out = append(out, b[8:8+n]...)
		b = b[8+n:]
	}
	return string(out), len(b) > 0
}

func frameHeader(b []byte) bool {
	if len(b) < 8 || b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
		return false
	}
	n := int(binary.BigEndian.Uint32(b[4:8]))
	return n <= len(b)-8
}
