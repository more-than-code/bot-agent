package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

func (e *hfEnv) applyCtx(ctx context.Context, name, content, extra string) []byte {
	return Handle(ctx, hostfiles.VerbApply, hfApplyReq(name, content, extra), e.sys)
}

func decodeApply(t *testing.T, raw []byte) hostfiles.ApplyResult {
	t.Helper()
	var r hostfiles.ApplyResult
	if err := hostfiles.DecodeStrict(raw, &r); err != nil || !hostfiles.ValidResult(r) {
		t.Fatalf("reply %s: %v", raw, err)
	}
	return r
}

func (e *hfEnv) stateFile(name string) string {
	return filepath.Join(e.root, "var/lib/bot-agent-runner/host-files", name)
}

func (e *hfEnv) assertNoState(name string) {
	e.t.Helper()
	if _, err := os.Lstat(e.stateFile(name)); !os.IsNotExist(err) {
		e.t.Fatalf("%s exists", name)
	}
}

func (e *hfEnv) logged(token string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.logs {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}

// A1: dd-policy and dd-secrets share dd-worker.service, so their applies
// never overlap.
func TestHostFileUnitLockSerialises(t *testing.T) {
	e := newHFEnv(t)
	var mu sync.Mutex
	active, peak := 0, 0
	first := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.run = func(kind string, _ unitCall) (int, string, error) {
		if kind != "check" {
			return 0, "", nil
		}
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		once.Do(func() { close(first); <-release })
		mu.Lock()
		active--
		mu.Unlock()
		return 0, "", nil
	}
	var wg sync.WaitGroup
	results := make([]hostfiles.ApplyResult, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = decodeApply(t, e.applyCtx(context.Background(), "dd-policy", newPolicy, ""))
	}()
	<-first
	go func() {
		defer wg.Done()
		results[1] = decodeApply(t, e.applyCtx(context.Background(), "dd-secrets", newSecrets, ""))
	}()
	time.Sleep(3 * lockPoll)
	mu.Lock()
	if peak != 1 {
		mu.Unlock()
		t.Fatal("second apply ran while the first held the unit")
	}
	mu.Unlock()
	close(release)
	wg.Wait()
	if results[0].Outcome != hostfiles.OutcomeSucceeded || results[1].Outcome != hostfiles.OutcomeSucceeded || peak != 1 {
		t.Fatalf("%+v peak %d", results, peak)
	}
}

// A1: different units run in parallel.
func TestHostFileUnitLockParallel(t *testing.T) {
	e := newHFEnv(t)
	started := make(chan string, 2)
	both := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	seen := map[string]bool{}
	e.run = func(kind string, c unitCall) (int, string, error) {
		if kind != "check" {
			return 0, "", nil
		}
		who := strings.Join(c.argv, " ")
		started <- who
		mu.Lock()
		seen[who] = true
		if len(seen) == 2 {
			once.Do(func() { close(both) })
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return 1, "", nil
		}
		return 0, "", nil
	}
	var wg sync.WaitGroup
	out := make([]hostfiles.ApplyResult, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		out[0] = decodeApply(t, e.applyCtx(context.Background(), "dd-policy", newPolicy, ""))
	}()
	go func() {
		defer wg.Done()
		out[1] = decodeApply(t, e.applyCtx(context.Background(), "plain", "a=2\n", `,"settle_sec":30`))
	}()
	wg.Wait()
	if out[0].Outcome != hostfiles.OutcomeSucceeded || out[1].Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("not parallel: %+v", out)
	}
}

// A1: a queued apply whose request is cancelled stops waiting and leaves
// no candidate.
func TestHostFileQueuedApplyCancelled(t *testing.T) {
	e := newHFEnv(t)
	first := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.run = func(kind string, _ unitCall) (int, string, error) {
		if kind == "check" {
			once.Do(func() { close(first); <-release })
		}
		return 0, "", nil
	}
	done := make(chan []byte, 1)
	go func() { done <- e.applyCtx(context.Background(), "dd-policy", newPolicy, "") }()
	<-first
	ctx, cancel := context.WithTimeout(context.Background(), 3*lockPoll)
	defer cancel()
	raw := e.applyCtx(ctx, "dd-secrets", newSecrets, "")
	if !bytes.Contains(raw, []byte(`"reason":"io_error"`)) {
		t.Fatalf("%s", raw)
	}
	if e.read("etc/dd-worker/secrets.env") != oldSecrets {
		t.Fatal("queued apply wrote")
	}
	if _, err := os.Lstat(filepath.Join(e.root, "etc/dd-worker/.secrets.env.deploybot-new")); !os.IsNotExist(err) {
		t.Fatal("queued apply left a candidate")
	}
	close(release)
	if r := decodeApply(t, <-done); r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("%+v", r)
	}
}

// T1: after the swap the request context no longer matters.
func TestHostFileCancelAfterSwapStillRollsBack(t *testing.T) {
	e := newHFEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	healths := 0
	var healthCtxErr error
	runUnit := e.sys.RunUnit
	e.sys.RunUnit = func(c context.Context, args []string, stdin *os.File) (int, []byte, error) {
		if strings.Contains(strings.Join(args, " "), "--wait 30s") {
			healths++
			if healths == 1 {
				cancel()
				healthCtxErr = c.Err()
				return 3, []byte("not_connected\n"), nil
			}
		}
		return runUnit(c, args, stdin)
	}
	r := decodeApply(t, e.applyCtx(ctx, "dd-policy", newPolicy, ""))
	if r.Outcome != hostfiles.OutcomeRolledBack || r.Reason != hostfiles.ReasonNotConnected {
		t.Fatalf("%+v", r)
	}
	if healthCtxErr != nil || healths != 2 || e.restarts() != 2 || e.read("etc/dd-worker/policy.toml") != oldPolicy {
		t.Fatalf("ctx %v healths %d restarts %d", healthCtxErr, healths, e.restarts())
	}
	e.assertNoCandidate("etc/dd-worker")
}

// T1: a cancel during idle or check leaves no swap and no candidate.
func TestHostFileCancelBeforeSwap(t *testing.T) {
	for _, kind := range []string{"idle", "check"} {
		t.Run(kind, func(t *testing.T) {
			e := newHFEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.run = func(k string, _ unitCall) (int, string, error) {
				if k == kind {
					cancel()
					if kind == "check" {
						return -1, "", context.Canceled
					}
				}
				return 0, "", nil
			}
			raw := e.applyCtx(ctx, "dd-policy", newPolicy, "")
			if bytes.Contains(raw, []byte(hostfiles.OutcomeSucceeded)) {
				t.Fatalf("%s", raw)
			}
			if e.read("etc/dd-worker/policy.toml") != oldPolicy || e.restarts() != 0 {
				t.Fatal("swapped")
			}
			e.assertNoCandidate("etc/dd-worker")
			e.assertNoState("dd-policy.prev")
		})
	}
}

// C1: a failed directory fsync after the rename is logged and the apply
// goes on.
func TestHostFileDirFsyncFailure(t *testing.T) {
	e := newHFEnv(t)
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
	e.sys.hostFault = func(step string) error {
		if step == "dir_fsync" {
			return syscall.EIO
		}
		return nil
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || e.read("etc/dd-worker/policy.toml") != newPolicy || !e.logged("dir_fsync_failed") {
		t.Fatalf("%+v", r)
	}
	// The same in the rollback path.
	e = newHFEnv(t)
	e.sys.hostFault = func(step string) error {
		if step == "dir_fsync" {
			return syscall.EIO
		}
		return nil
	}
	n := 0
	e.run = func(kind string, _ unitCall) (int, string, error) {
		if kind == "health" {
			n++
			if n == 1 {
				return 3, "not_running\n", nil
			}
		}
		return 0, "", nil
	}
	r = e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeRolledBack || e.read("etc/dd-worker/policy.toml") != oldPolicy {
		t.Fatalf("%+v", r)
	}
}

// T3: every rollback failure is rollback_failed with no .applied and the
// old bytes in .prev.
func TestHostFileRollbackFailures(t *testing.T) {
	healthFails := func(reasons ...string) func(string, unitCall) (int, string, error) {
		n := 0
		return func(kind string, _ unitCall) (int, string, error) {
			if kind != "health" {
				return 0, "", nil
			}
			if n < len(reasons) {
				r := reasons[n]
				n++
				if r == "" {
					return 0, "", nil
				}
				return 3, r + "\n", nil
			}
			return 0, "", nil
		}
	}
	cases := map[string]struct {
		setup  func(e *hfEnv)
		reason string
	}{
		"rollback write fails": {func(e *hfEnv) {
			e.run = healthFails("not_connected")
			e.sys.hostFault = func(s string) error {
				if s == "rollback_write" {
					return syscall.EIO
				}
				return nil
			}
		}, hostfiles.ReasonNotConnected},
		"rollback swap fails": {func(e *hfEnv) {
			e.run = healthFails("not_connected")
			e.sys.hostFault = func(s string) error {
				if s == "rollback_swap" {
					return syscall.EIO
				}
				return nil
			}
		}, hostfiles.ReasonNotConnected},
		"rollback restart fails": {func(e *hfEnv) {
			e.run = healthFails("not_connected")
			ctl := e.sys.Systemctl
			n := 0
			e.sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
				if args[0] == "restart" {
					n++
					if n == 2 {
						return "", errors.New("exit 1")
					}
				}
				return ctl(ctx, args)
			}
		}, hostfiles.ReasonNotConnected},
		"second health reason wins": {func(e *hfEnv) {
			e.run = healthFails("not_connected", "policy_invalid")
		}, hostfiles.ReasonPolicyInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newHFEnv(t)
			c.setup(e)
			r := e.apply("dd-policy", newPolicy, "")
			if r.Outcome != hostfiles.OutcomeRollbackFailed || r.Stage != hostfiles.StageRollback || r.Reason != c.reason || r.SHA256 != "" {
				t.Fatalf("%+v", r)
			}
			e.assertNoState("dd-policy.applied")
			prev, err := os.ReadFile(e.stateFile("dd-policy.prev"))
			if err != nil || string(prev) != oldPolicy {
				t.Fatalf(".prev %q %v", prev, err)
			}
			e.assertNoCandidate("etc/dd-worker")
		})
	}
}

// C3: .applied cannot be written: the outcome stays, a fixed token is logged.
func TestHostFileAppliedRecordFailure(t *testing.T) {
	e := newHFEnv(t)
	if err := os.MkdirAll(e.stateFile("dd-policy.applied"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || !e.logged("dd-policy "+logAppliedFailed) {
		t.Fatalf("%+v", r)
	}
	// unchanged follows the same rule.
	e = newHFEnv(t)
	if err := os.MkdirAll(filepath.Join(e.root, "var/lib/bot-agent-runner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "var/lib/bot-agent-runner/host-files"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.apply("dd-policy", oldPolicy, "")
	if r.Outcome != hostfiles.OutcomeUnchanged || !e.logged(logAppliedFailed) {
		t.Fatalf("%+v", r)
	}
}

// T4: the state a helper killed after the swap leaves behind.
func TestHostFileCrashAfterSwap(t *testing.T) {
	e := newHFEnv(t)
	e.writeTarget("etc/dd-worker/policy.toml", newPolicy, 0o644)
	if err := writeHostState(e.sys, "dd-policy", ".prev", []byte(oldPolicy)); err != nil {
		t.Fatal(err)
	}
	if err := writeHostState(e.sys, "dd-policy", ".applied", []byte(sha256Hex([]byte(oldPolicy))+"\n")); err != nil {
		t.Fatal(err)
	}
	st := e.status()["dd-policy"]
	if st.MatchesApplied == nil || *st.MatchesApplied {
		t.Fatalf("%+v", st)
	}
	// Re-applying the bytes on the host is unchanged and records them.
	if r := e.apply("dd-policy", newPolicy, ""); r.Outcome != hostfiles.OutcomeUnchanged || e.restarts() != 0 {
		t.Fatalf("%+v", r)
	}
	if st := e.status()["dd-policy"]; st.MatchesApplied == nil || !*st.MatchesApplied {
		t.Fatalf("%+v", st)
	}
	// The next real apply overwrites the stale .prev with what is there now.
	if r := e.apply("dd-policy", "third\n", ""); r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(e.stateFile("dd-policy.prev")); string(b) != newPolicy {
		t.Fatalf(".prev %q", b)
	}
}

func TestHostFileCandidateIsDirectory(t *testing.T) {
	e := newHFEnv(t)
	cand := filepath.Join(e.root, "etc/dd-worker/.policy.toml.deploybot-new")
	if err := os.Mkdir(cand, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := e.applyCtx(context.Background(), "dd-policy", newPolicy, "")
	if !bytes.Contains(raw, []byte(`"reason":"io_error"`)) || e.read("etc/dd-worker/policy.toml") != oldPolicy || len(e.units) != 0 {
		t.Fatalf("%s", raw)
	}
	if fi, err := os.Lstat(cand); err != nil || !fi.IsDir() {
		t.Fatal("directory removed")
	}
	// status leaves it and logs it.
	e.status()
	if fi, err := os.Lstat(cand); err != nil || !fi.IsDir() || !e.logged("candidate_is_directory") {
		t.Fatal("status touched the directory")
	}
}

// C5: status sweeps a leftover candidate only when the unit is free.
func TestHostFileStatusSweepsCandidate(t *testing.T) {
	e := newHFEnv(t)
	e.writeTarget("etc/dd-worker/.policy.toml.deploybot-new", "stale", 0o600)
	cand := filepath.Join(e.root, "etc/dd-worker/.policy.toml.deploybot-new")
	if err := ensureRuntime(e.root); err != nil {
		t.Fatal(err)
	}
	unlock, ok := tryLockUnit(e.root, "dd-worker.service")
	if !ok {
		t.Fatal("lock")
	}
	e.status()
	if _, err := os.Lstat(cand); err != nil {
		t.Fatal("swept while an apply held the unit")
	}
	unlock()
	e.status()
	if _, err := os.Lstat(cand); !os.IsNotExist(err) || !e.logged("dd-policy leftover_candidate_removed") {
		t.Fatal("not swept")
	}
}

func (e *hfEnv) status() map[string]hostfiles.FileStatus {
	e.t.Helper()
	raw := Handle(context.Background(), hostfiles.VerbStatus, []byte(`{}`), e.sys)
	var s hostfiles.StatusResult
	if err := hostfiles.DecodeStrict(raw, &s); err != nil || !hostfiles.ValidStatus(s) {
		e.t.Fatalf("%s %v", raw, err)
	}
	out := map[string]hostfiles.FileStatus{}
	for _, f := range s.Files {
		out[f.Name] = f
	}
	return out
}

// T5: the file rule over synthetic stats.
func TestHostTargetCheckStat(t *testing.T) {
	plain := &hostTarget{mode: 0o644, uid: 0, gid: 0, altGID: -1}
	group := &hostTarget{mode: 0o640, uid: 0, gid: 0, altGID: 1500}
	st := func(kind uint32, perm uint32, uid, gid uint32, nlink int) syscall.Stat_t {
		var s syscall.Stat_t
		setUint(&s.Mode, uint64(kind|perm))
		s.Uid, s.Gid = uid, gid
		setUint(&s.Nlink, uint64(nlink))
		return s
	}
	for _, c := range []struct {
		t    *hostTarget
		st   syscall.Stat_t
		want string
	}{
		{plain, st(syscall.S_IFREG, 0o644, 0, 0, 1), ""},
		{plain, st(syscall.S_IFREG, 0o644, 1, 0, 1), hostfiles.StatusOwner},
		{plain, st(syscall.S_IFREG, 0o644, 0, 1500, 1), hostfiles.StatusOwner},
		{plain, st(syscall.S_IFREG, 0o664, 0, 0, 1), hostfiles.StatusMode},
		{plain, st(syscall.S_IFREG, 0o600, 0, 0, 1), hostfiles.StatusMode},
		{plain, st(syscall.S_IFREG, 0o644, 0, 0, 2), hostfiles.StatusNlink},
		{plain, st(syscall.S_IFLNK, 0o777, 0, 0, 1), hostfiles.StatusSymlink},
		{plain, st(syscall.S_IFIFO, 0o644, 0, 0, 1), hostfiles.StatusNotRegular},
		{plain, st(syscall.S_IFDIR, 0o644, 0, 0, 1), hostfiles.StatusNotRegular},
		{group, st(syscall.S_IFREG, 0o640, 0, 1500, 1), ""},
		{group, st(syscall.S_IFREG, 0o640, 0, 0, 1), ""},
		{group, st(syscall.S_IFREG, 0o640, 0, 1501, 1), hostfiles.StatusOwner},
		{group, st(syscall.S_IFREG, 0o640, 1500, 1500, 1), hostfiles.StatusOwner},
	} {
		if got := c.t.checkStat(c.st); got != c.want {
			t.Errorf("mode %o uid %d gid %d: %q want %q", c.st.Mode, c.st.Uid, c.st.Gid, got, c.want)
		}
	}
}

// T5: a 0640 file in the run_as group keeps that group across an apply.
func TestHostTargetKeepsGroup(t *testing.T) {
	e := newHFEnv(t)
	e.writeTarget("etc/plain/app.conf", "a=1\n", 0o640)
	hf := policy.HostFile{Name: "plain", Dir: "plain", File: "app.conf", Mode: 0o640, RunAs: policy.Account{GID: uint32(os.Getgid())}}
	// The "root" group is some other gid; the file's group is the run_as one.
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid() + 4242}
	tg, reason := openHostTarget(e.sys, hf)
	if reason != "" {
		t.Fatal(reason)
	}
	defer tg.close()
	_, gid, reason := tg.read()
	if reason != "" || gid != os.Getgid() {
		t.Fatalf("%q %d", reason, gid)
	}
	if err := tg.writeCandidate([]byte("a=2\n"), gid); err != nil {
		t.Fatal(err)
	}
	if renamed, err := tg.swap(); !renamed || err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(e.root, "etc/plain/app.conf"), &st); err != nil {
		t.Fatal(err)
	}
	if int(st.Gid) != os.Getgid() || permOf(st) != 0o640 {
		t.Fatalf("gid %d mode %o", st.Gid, permOf(st))
	}
}

// T5: a FIFO at the target is refused without blocking.
func TestHostFileFifoTarget(t *testing.T) {
	e := newHFEnv(t)
	p := filepath.Join(e.root, "etc/dd-worker/policy.toml")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan hostfiles.ApplyResult, 1)
	go func() { done <- e.apply("dd-policy", newPolicy, "") }()
	select {
	case r := <-done:
		if r.Outcome != hostfiles.OutcomeForbiddenSpec || r.Stage != hostfiles.StageTarget {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked on a FIFO")
	}
	if st := e.status()["dd-policy"]; st.Reason != hostfiles.StatusNotRegular {
		t.Fatalf("%+v", st)
	}
}

// T6: status reasons and a malformed .applied.
func TestHostFileStatusMoreReasons(t *testing.T) {
	e := newHFEnv(t)
	tgt := filepath.Join(e.root, "etc/dd-worker/policy.toml")
	other := filepath.Join(e.root, "cfg/x")
	e.writeTarget("cfg/x", "x", 0o644)
	if err := os.Remove(tgt); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, tgt); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(e.root, "etc/dd-worker/secrets.env"), filepath.Join(e.root, "cfg/hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(e.root, "etc/plain")); err != nil {
		t.Fatal(err)
	}
	if err := writeHostState(e.sys, "dd-secrets", ".applied", []byte("garbage\n")); err != nil {
		t.Fatal(err)
	}
	s := e.status()
	if f := s["dd-policy"]; f.Reason != hostfiles.StatusSymlink || !f.Exists || f.OwnerOK {
		t.Fatalf("symlink %+v", f)
	}
	if f := s["dd-secrets"]; f.Reason != hostfiles.StatusNlink || f.MatchesApplied != nil {
		t.Fatalf("nlink %+v", f)
	}
	if f := s["plain"]; f.Reason != hostfiles.StatusMissing || f.Exists {
		t.Fatalf("missing dir %+v", f)
	}

	e = newHFEnv(t)
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid() + 1}
	if f := e.status()["dd-policy"]; f.Reason != hostfiles.StatusOwner {
		t.Fatalf("owner %+v", f)
	}
	e = newHFEnv(t)
	if err := os.Chmod(filepath.Join(e.root, "etc/dd-worker"), 0o775); err != nil {
		t.Fatal(err)
	}
	if f := e.status()["dd-policy"]; f.Reason != hostfiles.StatusDir {
		t.Fatalf("dir %+v", f)
	}
	if os.Geteuid() != 0 {
		e = newHFEnv(t)
		d := filepath.Join(e.root, "etc/dd-worker")
		if err := os.Chmod(d, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
		if f := e.status()["dd-policy"]; f.Reason != hostfiles.StatusIO {
			t.Fatalf("io %+v", f)
		}
	}
}

// T6: failed check, busy and idle error leave no .prev; a secret file's
// results never carry sha256.
func TestHostFileNoPrevAfterEarlyFailures(t *testing.T) {
	for name, run := range map[string]func(string, unitCall) (int, string, error){
		"check": func(k string, _ unitCall) (int, string, error) {
			return map[bool]int{true: 2}[k == "check"], "bad_x\n", nil
		},
		"busy":       func(k string, _ unitCall) (int, string, error) { return map[bool]int{true: 4}[k == "idle"], "", nil },
		"idle error": func(k string, _ unitCall) (int, string, error) { return map[bool]int{true: 1}[k == "idle"], "", nil },
	} {
		for _, file := range []string{"dd-policy", "dd-secrets"} {
			t.Run(name+" "+file, func(t *testing.T) {
				e := newHFEnv(t)
				e.run = run
				content := newPolicy
				if file == "dd-secrets" {
					content = newSecrets
				}
				r := e.apply(file, content, "")
				if r.Outcome == hostfiles.OutcomeSucceeded || r.SHA256 != "" {
					t.Fatalf("%+v", r)
				}
				e.assertNoState(file + ".prev")
			})
		}
	}
	// Success and unchanged of a secret file carry no sha256 either.
	e := newHFEnv(t)
	if r := e.apply("dd-secrets", newSecrets, ""); r.Outcome != hostfiles.OutcomeSucceeded || r.SHA256 != "" {
		t.Fatalf("%+v", r)
	}
	if r := e.apply("dd-secrets", newSecrets, ""); r.Outcome != hostfiles.OutcomeUnchanged || r.SHA256 != "" {
		t.Fatalf("%+v", r)
	}
}

// S6: systemctl gets -- before the unit.
func TestHostFileSystemctlArgv(t *testing.T) {
	e := newHFEnv(t)
	if r := e.apply("plain", "a=2\n", `,"settle_sec":30`); r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("%+v", r)
	}
	for _, c := range e.ctl {
		if c[len(c)-2] != "--" || c[len(c)-1] != "plain.service" {
			t.Fatalf("argv %v", c)
		}
	}
	raw, _ := json.Marshal(e.ctl)
	if !strings.Contains(string(raw), `["restart","--","plain.service"]`) {
		t.Fatalf("%s", raw)
	}
}

// A4: the worst case fits the task timeout.
func TestHostFileWorstCaseBudget(t *testing.T) {
	parts := []struct {
		name string
		d    time.Duration
	}{
		{"check", hostfiles.CheckBudget + hostfiles.RunGrace},
		{"idle", hostfiles.IdleBudget + hostfiles.RunGrace},
		{"restart", hostfiles.RestartBudget},
		{"settle", time.Duration(hostfiles.MaxSettle) * time.Second},
		{"rollback restart", hostfiles.RestartBudget},
		{"rollback settle", time.Duration(hostfiles.MaxSettle) * time.Second},
	}
	var sum time.Duration
	for _, p := range parts {
		sum += p.d
	}
	if sum != hostfiles.WorstCase() || sum != 515*time.Second || sum >= hostfiles.TaskTimeout {
		t.Fatalf("sum %v worst %v timeout %v", sum, hostfiles.WorstCase(), hostfiles.TaskTimeout)
	}
}

// A4: the health window starts after the restart returns, and the health
// call gets exactly that window.
func TestHostFileHealthWindowAfterRestart(t *testing.T) {
	e := newHFEnv(t)
	clock := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	e.sys.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	ctl := e.sys.Systemctl
	e.sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
		if args[0] == "restart" {
			mu.Lock()
			clock = clock.Add(50 * time.Second) // a slow restart
			mu.Unlock()
			if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > hostfiles.RestartBudget {
				t.Fatalf("restart not bounded: %v %v", dl, ok)
			}
		}
		return ctl(ctx, args)
	}
	var budget string
	var deadline time.Duration
	runUnit := e.sys.RunUnit
	e.sys.RunUnit = func(ctx context.Context, args []string, stdin *os.File) (int, []byte, error) {
		if strings.Contains(strings.Join(args, " "), "--wait 30s") {
			for _, a := range args {
				if strings.HasPrefix(a, "RuntimeMaxSec=") {
					budget = a
				}
			}
			if dl, ok := ctx.Deadline(); ok {
				deadline = time.Until(dl)
			}
		}
		return runUnit(ctx, args, stdin)
	}
	if r := e.apply("dd-policy", newPolicy, ""); r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("%+v", r)
	}
	if budget != "RuntimeMaxSec=60" || deadline > 60*time.Second || deadline < 55*time.Second {
		t.Fatalf("budget %q deadline %v", budget, deadline)
	}
}

// T2: the real runUnit against a stub systemd-run in the test's temp dir.
func TestRunUnitRealExec(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "systemd-run")
	script := `#!/bin/sh
: > "` + dir + `/argv"
for a in "$@"; do printf '%s\n' "$a" >> "` + dir + `/argv"; done
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
case "$2" in
  exit) printf '%s\n' "line" >&2; exit "$3" ;;
  noise) i=0; while [ $i -lt 64 ]; do printf '%s' "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" >&2; i=$((i+1)); done; exit 2 ;;
  stdin) cat > "` + dir + `/stdin"; exit 0 ;;
  sleep) exec sleep 5 ;;
  signal) kill -TERM $$ ;;
esac
exit 9
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := runUnitAt(stub)
	hf := policy.HostFile{Name: "dd-policy", RunAs: policy.Account{UID: 1500, GID: 1501}}
	call := func(ctx context.Context, stdin *os.File, argv ...string) (int, []byte, error, []string) {
		args, err := hostRunArgs(hf, 15*time.Second, argv)
		if err != nil {
			t.Fatal(err)
		}
		code, stderr, err := run(ctx, args, stdin)
		b, _ := os.ReadFile(filepath.Join(dir, "argv"))
		got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("argv\n%v\nwant\n%v", got, args)
		}
		return code, stderr, err, args
	}
	for _, n := range []string{"0", "2", "3", "4", "5"} {
		code, stderr, err, _ := call(context.Background(), nil, "/x", "exit", n)
		if err != nil || string(stderr) != "line\n" || code != int(n[0]-'0') {
			t.Fatalf("exit %s: %d %q %v", n, code, stderr, err)
		}
	}
	code, stderr, err, _ := call(context.Background(), nil, "/x", "noise")
	if err != nil || code != 2 || len(stderr) != hostfiles.MaxStderr+1 {
		t.Fatalf("noise %d %d %v", code, len(stderr), err)
	}
	in := filepath.Join(dir, "in")
	content := strings.Repeat("k = \"v\"\n", hostfiles.MaxContent/8)
	if err := os.WriteFile(in, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err, _ = call(context.Background(), f, "/x", "stdin")
	f.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); err != nil || code != 0 || string(b) != content {
		t.Fatalf("stdin %d %v %d bytes", code, err, len(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	start := time.Now()
	code, _, err, _ = call(ctx, nil, "/x", "sleep")
	cancel()
	if code != -1 || err == nil || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout %d %v %v", code, err, time.Since(start))
	}
	code, _, err, _ = call(context.Background(), nil, "/x", "signal")
	if code != -1 || err == nil {
		t.Fatalf("signal %d %v", code, err)
	}
	if c, _, err := runUnitAt("systemd-run")(context.Background(), nil, nil); c != -1 || err == nil {
		t.Fatal("relative bin ran")
	}
}

// setUint stores v in a Stat_t field whose width differs by platform.
func setUint[T ~uint16 | ~uint32 | ~uint64](p *T, v uint64) { *p = T(v) }
