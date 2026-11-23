package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// Edge config file names inside the edge directory. config.json is the
// applied config Caddy starts with. The other two exist only during apply.
const (
	edgeNewFile  = edge.ConfigFile + ".new"
	edgePrevFile = edge.ConfigFile + ".prev"
)

// edgeReadyBudget bounds the wait for a started edge to answer on its admin
// socket. edgeLoadBudget bounds one /load. edgePoll is the wait step. They
// are variables only so tests can shrink them.
var (
	edgeReadyBudget = 20 * time.Second
	edgeLoadBudget  = 30 * time.Second
	edgePoll        = 250 * time.Millisecond
)

// handleEdge runs one v1.15 verb. Refusals are {"error": code} with an
// optional sanitised "detail". Only edge.apply is gated on the policy key:
// stop and status must still work after the key is removed.
func handleEdge(ctx context.Context, verb string, in []byte, sys Sys) []byte {
	if ctx.Err() != nil {
		return edgeErr(sys, edge.ReasonDocker, "")
	}
	if len(in) > images.MaxFrame {
		return edgeErr(sys, edge.ReasonBadSpec, "")
	}
	switch verb {
	case edge.VerbApply:
		return doEdgeApply(ctx, in, sys)
	case edge.VerbStop:
		return doEdgeStop(ctx, in, sys)
	case edge.VerbStatus:
		return doEdgeStatus(ctx, in, sys)
	default:
		return edgeErr(sys, edge.ReasonBadSpec, "")
	}
}

func edgeErr(sys Sys, code, detail string) []byte {
	code = edge.FixedReason(code)
	detail = edge.Detail(detail)
	if sys.Logf != nil {
		sys.Logf("bot-agent-runner: refuse %s %s", refusalVerb(sys.logVerb), code)
		if detail != "" {
			sys.Logf("bot-agent-runner: edge: %s", detail)
		}
	}
	b, err := marshalJSON(edge.Refusal{Error: code, Detail: detail})
	if err != nil {
		return images.EncodeError(edge.ReasonDocker)
	}
	return b
}

func edgeOK(v any) []byte {
	b, err := marshalJSON(v)
	if err != nil {
		return images.EncodeError(edge.ReasonDocker)
	}
	return b
}

func doEdgeApply(ctx context.Context, in []byte, sys Sys) []byte {
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID, SkipEnvFiles: true,
	})
	// A release still carrying the placeholder digest never runs an edge.
	if !pol.EdgeEnabled() || !edge.ImagePinned() {
		return edgeErr(sys, edge.ReasonDisabled, "")
	}
	req, err := edge.ParseApply(in)
	if err != nil {
		return edgeErr(sys, edge.ReasonBadSpec, edge.SpecPath(err))
	}
	unlock, code := lockEdge(sys)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	defer unlock()
	host, code := readHost(sys)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	st, code := openEdgeStore(sys, true)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	defer closeStore(st)
	eng := newEngine(dockerSock(sys), sys.Logf)
	rows, err := eng.List(ctx)
	if err != nil {
		return edgeErr(sys, edgeEngReason(err), "")
	}
	cur, code := edgeSlot(rows, host)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	running := cur.found && cur.state == "running"
	port, who, busy, err := edgePortHolder(sys, rows, cur.id, running)
	if err != nil {
		return edgeErr(sys, edge.ReasonDocker, "cannot read the host's listening sockets")
	}
	if busy {
		return edgeErr(sys, edge.ReasonPortInUse, portDetail(port, who))
	}
	applied, err := st.edgeHash(edge.ConfigFile)
	if err != nil {
		return edgeErr(sys, storeCode(err), "")
	}
	imageOK := cur.found && cur.image == edge.Image
	if imageOK && running && applied == req.ConfigHash {
		return edgeOK(edge.ApplyResult{Outcome: edge.OutcomeUnchanged, ConfigHash: applied, Image: edge.Image})
	}
	if code := ensureEdgeVolume(ctx, eng, host); code != "" {
		return edgeErr(sys, code, "")
	}
	if imageOK && running {
		return reloadEdge(ctx, sys, st, req)
	}
	return startEdge(ctx, sys, eng, st, cur, imageOK, host, pol.CgroupnsMode(), req)
}

// reloadEdge loads a new config into the running edge. config.json is
// replaced only after Caddy accepted it, so a refused config leaves the
// file and the running config as they were.
func reloadEdge(ctx context.Context, sys Sys, st *store, req edge.ApplyRequest) []byte {
	admin, code := edgeAdmin(sys, st)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	if err := st.writeEdge(edgeNewFile, req.Config); err != nil {
		return edgeErr(sys, storeCode(err), "")
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), edgeLoadBudget)
	defer cancel()
	detail, err := admin.load(lctx, req.Config)
	if err != nil {
		_ = st.unlinkFile(edgeNewFile)
		if errors.Is(err, errLoad) {
			return edgeErr(sys, edge.ReasonInvalidConfig, detail)
		}
		return edgeErr(sys, edge.ReasonDocker, "")
	}
	if _, err := st.renameMoved(edgeNewFile, edge.ConfigFile); err != nil {
		return edgeErr(sys, storeCode(err), "")
	}
	return edgeOK(edge.ApplyResult{Outcome: edge.OutcomeApplied, ConfigHash: req.ConfigHash, Image: edge.Image})
}

// startEdge (re)creates the edge with the new config as config.json: the
// container is missing, stopped, or runs another image. The old container
// is removed only after the pinned image is pulled. When the new container
// cannot be created, started, or will not run the new config, it is
// removed, the previous config.json is put back and the previous container
// is recreated from its own image (restorePrevious).
// cgroupns is the host policy's CgroupnsMode (agent-protocol v1.24).
func startEdge(ctx context.Context, sys Sys, eng engine, st *store, cur slot, imageOK bool, host, cgroupns string, req edge.ApplyRequest) []byte {
	if !imageOK {
		ref, err := images.Parse(edge.Image)
		if err != nil {
			return edgeErr(sys, edge.ReasonBadSpec, "")
		}
		if _, _, err := eng.Pull(ctx, ref, nil); err != nil {
			return edgeErr(sys, edgeEngReason(err), "")
		}
	}
	body, err := edgeCreateBody(edge.Image, host, req.ConfigHash, cgroupns)
	if err != nil {
		return edgeErr(sys, edge.ReasonDocker, "")
	}
	prevHash, err := st.edgeHash(edge.ConfigFile)
	if err != nil {
		return edgeErr(sys, storeCode(err), "")
	}
	if err := st.writeEdge(edgeNewFile, req.Config); err != nil {
		return edgeErr(sys, storeCode(err), "")
	}
	hadPrev, err := st.swapEdgeConfig()
	if err != nil {
		_ = st.unlinkFile(edgeNewFile)
		return edgeErr(sys, storeCode(err), "")
	}
	// The steps below run to the end even if the agent gives up.
	fctx, cancel := finishCtx(ctx)
	defer cancel()
	if err := stopRemove(fctx, eng, cur); err != nil {
		st.restoreEdgeConfig(hadPrev)
		return edgeErr(sys, edgeEngReason(err), "")
	}
	// A socket left by the stopped Caddy would answer nothing; the new one
	// creates its own.
	if err := st.unlinkSocket(edge.AdminSocket); err != nil {
		restorePrevious(fctx, sys, eng, st, "", cur, hadPrev, host, cgroupns, prevHash)
		return edgeErr(sys, storeCode(err), "")
	}
	id, err := eng.Create(fctx, edge.ContainerName, body)
	if err == nil {
		err = eng.Start(fctx, id)
	}
	if err != nil {
		restorePrevious(fctx, sys, eng, st, id, cur, hadPrev, host, cgroupns, prevHash)
		return edgeErr(sys, edgeEngReason(err), "")
	}
	code, detail := waitEdge(fctx, sys, eng, st, id)
	if code != "" {
		restorePrevious(fctx, sys, eng, st, id, cur, hadPrev, host, cgroupns, prevHash)
		return edgeErr(sys, code, detail)
	}
	_ = st.unlinkFile(edgePrevFile)
	return edgeOK(edge.ApplyResult{Outcome: edge.OutcomeStarted, ConfigHash: req.ConfigHash, Image: edge.Image})
}

// restorePrevious undoes a failed start: the new container (id, when one
// was created) is removed, the previous config.json goes back, and the
// previous container is recreated from its own image with that config and
// started again if it was running. Failures here are logged, not returned:
// the caller already has the error to report.
func restorePrevious(ctx context.Context, sys Sys, eng engine, st *store, id string, cur slot, hadPrev bool, host, cgroupns, prevHash string) {
	if id != "" {
		_ = eng.Stop(ctx, id)
		if err := eng.Remove(ctx, id); err != nil {
			logf(sys, "bot-agent-runner: edge: could not remove the failed container")
		}
	}
	st.restoreEdgeConfig(hadPrev)
	if !cur.found || !hadPrev {
		return
	}
	body, err := edgeCreateBody(cur.image, host, prevHash, cgroupns)
	if err == nil {
		_ = st.unlinkSocket(edge.AdminSocket)
		var prevID string
		prevID, err = eng.Create(ctx, edge.ContainerName, body)
		if err == nil && cur.state == "running" {
			err = eng.Start(ctx, prevID)
		}
	}
	if err != nil {
		logf(sys, "bot-agent-runner: edge: could not recreate the previous edge container")
	}
}

// waitEdge waits until the new container runs and its admin socket answers.
// A container that exits or restarts did not accept its config: the answer
// is invalid_config with the first error line of its log.
func waitEdge(ctx context.Context, sys Sys, eng engine, st *store, id string) (string, string) {
	deadline := time.Now().Add(edgeReadyBudget)
	for {
		in, err := eng.Inspect(ctx, id)
		if err != nil {
			return edgeEngReason(err), ""
		}
		switch {
		case in.State == "exited" || in.State == "dead" || in.State == "restarting" || in.RestartCount > 0:
			return edge.ReasonInvalidConfig, edgeLogError(ctx, eng, id)
		case in.State == "running":
			if admin, code := edgeAdmin(sys, st); code == "" {
				rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				ok := admin.ready(rctx)
				cancel()
				if ok {
					return "", ""
				}
			}
		}
		if !time.Now().Before(deadline) {
			return edge.ReasonDocker, "the edge did not answer on its admin socket"
		}
		select {
		case <-ctx.Done():
			return edge.ReasonDocker, ""
		case <-time.After(edgePoll):
		}
	}
}

// edgeLogError is the first line of the container log that reads as an
// error (Caddy's "Error: ..." exit line or a JSON line at level error),
// else the last line.
func edgeLogError(ctx context.Context, eng engine, id string) string {
	raw, _, err := eng.Logs(ctx, id, 50)
	if err != nil {
		return ""
	}
	text, _ := demux(raw)
	last := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		if strings.HasPrefix(line, "Error:") {
			return line
		}
		var j struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &j) == nil && j.Level == "error" {
			if j.Error != "" {
				return j.Msg + ": " + j.Error
			}
			return j.Msg
		}
	}
	return last
}

func doEdgeStop(ctx context.Context, in []byte, sys Sys) []byte {
	if edge.ParseEmpty(in) != nil {
		return edgeErr(sys, edge.ReasonBadSpec, "")
	}
	unlock, code := lockEdge(sys)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	defer unlock()
	host, code := readHost(sys)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	eng := newEngine(dockerSock(sys), sys.Logf)
	rows, err := eng.List(ctx)
	if err != nil {
		return edgeErr(sys, edgeEngReason(err), "")
	}
	cur, code := edgeSlot(rows, host)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	if !cur.found {
		return edgeOK(edge.StopResult{Outcome: edge.OutcomeAbsent})
	}
	fctx, cancel := finishCtx(ctx)
	defer cancel()
	if err := stopRemove(fctx, eng, cur); err != nil {
		return edgeErr(sys, edgeEngReason(err), "")
	}
	return edgeOK(edge.StopResult{Outcome: edge.OutcomeStopped})
}

func doEdgeStatus(ctx context.Context, in []byte, sys Sys) []byte {
	if edge.ParseEmpty(in) != nil {
		return edgeErr(sys, edge.ReasonBadSpec, "")
	}
	host, code := readHost(sys)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	eng := newEngine(dockerSock(sys), sys.Logf)
	rows, err := eng.List(ctx)
	if err != nil {
		return edgeErr(sys, edgeEngReason(err), "")
	}
	cur, code := edgeSlot(rows, host)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	if !cur.found {
		return edgeOK(edge.StatusResult{State: edge.StateAbsent})
	}
	st, code := openEdgeStore(sys, false)
	if code != "" {
		return edgeErr(sys, code, "")
	}
	defer closeStore(st)
	hash := ""
	if st != nil {
		if hash, err = st.edgeHash(edge.ConfigFile); err != nil {
			return edgeErr(sys, storeCode(err), "")
		}
	}
	in2, err := eng.Inspect(ctx, cur.id)
	if errors.Is(err, errNotFound) {
		return edgeOK(edge.StatusResult{State: edge.StateAbsent})
	}
	if err != nil {
		return edgeErr(sys, edgeEngReason(err), "")
	}
	res := edge.StatusResult{State: edge.StateStopped, Image: in2.Image, ConfigHash: hash}
	if in2.State == "running" {
		res.State = edge.StateRunning
		res.StartedAt = dockerTime(in2.StartedAt)
	}
	return edgeOK(res)
}

// edgeSlot finds deploybot-edge. A container of that name without the
// owner, host and kind=edge labels, or with a project label, is not ours:
// conflict, nothing touched.
func edgeSlot(rows []listed, host string) (slot, string) {
	c, ok := findExact(rows, edge.ContainerName)
	if !ok {
		return slot{}, ""
	}
	l := c.Labels
	if l[images.LabelOwner] != images.OwnerValue || l[images.LabelHost] != host ||
		l[images.LabelKind] != edge.KindValue || l[images.LabelProject] != "" {
		return slot{}, edge.ReasonConflict
	}
	return slot{found: true, id: c.ID, state: c.State, image: c.Image, labels: c.Labels}, ""
}

// ensureEdgeVolume ensures deploybot-edge-data. A volume the helper created
// carries the edge labels. A local volume with no dev.deploybot.* label at
// all is adopted: that is the volume an operator creates to copy existing
// certificates in before the first apply (spec §7). Anything else, such as
// another project's volume of that name, is conflict.
func ensureEdgeVolume(ctx context.Context, eng engine, host string) string {
	info, err := eng.VolumeInspect(ctx, edge.VolumeName)
	if errors.Is(err, errNotFound) {
		if err := eng.EdgeVolumeCreate(ctx, edge.VolumeName, host); err != nil {
			return edgeEngReason(err)
		}
		info, err = eng.VolumeInspect(ctx, edge.VolumeName)
	}
	if err != nil {
		return edgeEngReason(err)
	}
	if info.Driver != "local" || len(info.Options) > 0 {
		return edge.ReasonConflict
	}
	ours := map[string]string{}
	for k, v := range info.Labels {
		if strings.HasPrefix(k, images.LabelPrefix) {
			ours[k] = v
		}
	}
	if len(ours) == 0 {
		return ""
	}
	want := edgeLabels(host)
	if len(ours) != len(want) {
		return edge.ReasonConflict
	}
	for k, v := range want {
		if ours[k] != v {
			return edge.ReasonConflict
		}
	}
	return ""
}

type edgeCreate struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Labels     map[string]string `json:"Labels"`
	HostConfig edgeHostConfig    `json:"HostConfig"`
}

type edgeHostConfig struct {
	NetworkMode    string            `json:"NetworkMode"`
	RestartPolicy  restartBody       `json:"RestartPolicy"`
	Mounts         []mountBody       `json:"Mounts"`
	CgroupnsMode   string            `json:"CgroupnsMode"`
	PidsLimit      int64             `json:"PidsLimit"`
	LogConfig      logConfigBody     `json:"LogConfig"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
}

// edgeCreateBody is the whole ContainerCreate body for the edge: the pinned
// image, host network, the data volume at /data and the config directory
// at /run/edge (writable: Caddy creates its admin socket there). Caddy runs
// with config.json; that file sets the admin endpoint to the socket.
// cgroupns is "private" or "host" (Policy.CgroupnsMode, v1.24).
func edgeCreateBody(image, host, configHash, cgroupns string) ([]byte, error) {
	if _, err := images.Parse(image); err != nil {
		return nil, err
	}
	labels := edgeLabels(host)
	labels[edge.LabelConfigHash] = configHash
	return marshalJSON(edgeCreate{
		Image:  image,
		Cmd:    []string{"caddy", "run", "--config", edge.ConfigPath},
		Labels: labels,
		HostConfig: edgeHostConfig{
			NetworkMode:   "host",
			RestartPolicy: restartBody{Name: "unless-stopped"},
			Mounts: []mountBody{
				{Type: "volume", Source: edge.VolumeName, Target: edge.DataTarget},
				{Type: "bind", Source: edge.Dir, Target: edge.MountTarget},
			},
			CgroupnsMode: cgroupns,
			PidsLimit:    1024,
			LogConfig: logConfigBody{
				Type:   "json-file",
				Config: logOptBody{MaxSize: "10m", MaxFile: "3"},
			},
			// Hardening (security review M3). Caddy runs as uid 0 and only
			// writes /data (a root-owned volume), /run/edge (root 0700) and
			// /config (its autosave), so it needs no DAC_OVERRIDE; binding 80
			// and 443 needs NET_BIND_SERVICE.
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"NET_BIND_SERVICE"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			Tmpfs: map[string]string{
				"/config": "rw,nosuid,nodev,size=16m",
				"/tmp":    "rw,nosuid,nodev,size=16m",
			},
			Memory:     edgeMemory,
			MemorySwap: edgeMemory,
		},
	})
}

// edgeMemory is the edge's memory limit, with no swap above it.
const edgeMemory = 512 * 1024 * 1024

func portDetail(port portUse, who string) string {
	if who == "" {
		return fmt.Sprintf("%s is in use", port)
	}
	return fmt.Sprintf("%s is used by %s", port, who)
}

func lockEdge(sys Sys) (func(), string) {
	if err := ensureRuntime(sys.Root); err != nil {
		return nil, edge.ReasonDocker
	}
	unlock, err := lockFile(joinRoot(sys.Root, filepath.Join("run", "bot-agent", "edge.lock")))
	if err != nil {
		return nil, edge.ReasonDocker
	}
	return unlock, ""
}

func (sys Sys) edgeRoot() (string, bool) {
	if sys.edgeStore != "" {
		return sys.edgeStore, true
	}
	// Unit tests are not root and must not create the production directory.
	if os.Geteuid() != 0 {
		return "", false
	}
	return edge.Dir, true
}

// openEdgeStore opens the edge directory (root 0700) with the store walk.
// create makes the last component. Without create, a missing directory is
// a nil store and no error.
func openEdgeStore(sys Sys, create bool) (*store, string) {
	root, ok := sys.edgeRoot()
	if !ok {
		return nil, edge.ReasonDocker
	}
	uid, gid := sys.fileIDs()
	st, err := openStore(root, uid, gid, create)
	if err != nil {
		if errors.Is(err, errStoreAbsent) {
			return nil, ""
		}
		return nil, storeCode(err)
	}
	return st, ""
}

// edgeAdmin is the admin client for the socket in the edge directory. The
// socket must be a socket, not a link to one elsewhere.
func edgeAdmin(sys Sys, st *store) (*caddyAdmin, string) {
	root, ok := sys.edgeRoot()
	if !ok {
		return nil, edge.ReasonDocker
	}
	fi, err := fstatAt(st.fd, edge.AdminSocket)
	if err != nil || kindOf(fi) != uint32(syscall.S_IFSOCK) {
		return nil, edge.ReasonDocker
	}
	return newCaddyAdmin(filepath.Join(root, edge.AdminSocket)), ""
}

// edgeHash is the SHA-256 of a regular file in the edge directory, or ""
// when it is missing.
func (s *store) edgeHash(name string) (string, error) {
	fd, err := openat(s.fd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return "", nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", errStoreBad
		}
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errStoreBad
	}
	b, err := io.ReadAll(io.LimitReader(f, edge.MaxConfig+1))
	if err != nil {
		return "", err
	}
	if len(b) > edge.MaxConfig {
		return "", errStoreBad
	}
	return edge.HashConfig(b), nil
}

// writeEdge writes content as name (root 0444), replacing a leftover
// regular file of that name from an earlier crash.
func (s *store) writeEdge(name string, content []byte) error {
	if err := s.unlinkFile(name); err != nil {
		return err
	}
	if err := s.writeFile(s.fd, name, content); err != nil {
		return err
	}
	return syscall.Fsync(s.fd)
}

// unlinkFile removes a regular file. A missing name is a no-op; anything
// that is not a regular file is left in place and is errStoreBad.
func (s *store) unlinkFile(name string) error {
	fi, err := fstatAt(s.fd, name)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isReg(fi) {
		return errStoreBad
	}
	if err := unlinkat(s.fd, name, 0); err != nil {
		return err
	}
	return syscall.Fsync(s.fd)
}

// unlinkSocket removes a stale socket of that name (security review L9).
// A missing name is a no-op; anything that is not a socket is left in place
// and is errStoreBad.
func (s *store) unlinkSocket(name string) error {
	fi, err := fstatAt(s.fd, name)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if kindOf(fi) != uint32(syscall.S_IFSOCK) {
		return errStoreBad
	}
	return unlinkat(s.fd, name, 0)
}

// swapEdgeConfig moves config.json aside as config.json.prev (when there is
// one) and config.json.new into its place. It reports whether a previous
// config was kept.
func (s *store) swapEdgeConfig() (bool, error) {
	if err := s.unlinkFile(edgePrevFile); err != nil {
		return false, err
	}
	hadPrev, err := s.renameMoved(edge.ConfigFile, edgePrevFile)
	if err != nil {
		return hadPrev, err
	}
	if _, err := s.renameMoved(edgeNewFile, edge.ConfigFile); err != nil {
		if hadPrev {
			_, _ = s.renameMoved(edgePrevFile, edge.ConfigFile)
		}
		return false, err
	}
	return hadPrev, nil
}

// restoreEdgeConfig undoes swapEdgeConfig: the previous config goes back,
// or the new one is removed when there was none.
func (s *store) restoreEdgeConfig(hadPrev bool) {
	if hadPrev {
		_, _ = s.renameMoved(edgePrevFile, edge.ConfigFile)
		return
	}
	_ = s.unlinkFile(edge.ConfigFile)
}

// edgeEngReason is engReason for the edge verbs, which keep their own
// codes: a start the runtime refuses stays docker_unavailable there
// (agent-protocol v1.24 changes only catalog containers).
func edgeEngReason(err error) string {
	if errors.Is(err, errStart) {
		return edge.ReasonDocker
	}
	return engReason(err)
}
