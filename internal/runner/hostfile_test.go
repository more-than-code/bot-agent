package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
)

// hfExecFS says every path is a root-owned directory except the listed
// executables, which are root-owned 0755 files. unsafe marks one 0775.
type hfExecFS struct {
	exes   map[string]bool
	unsafe map[string]bool
}

type hfInfo struct {
	mode os.FileMode
}

func (i hfInfo) Name() string       { return "x" }
func (i hfInfo) Size() int64        { return 0 }
func (i hfInfo) Mode() os.FileMode  { return i.mode }
func (i hfInfo) ModTime() time.Time { return time.Time{} }
func (i hfInfo) IsDir() bool        { return i.mode.IsDir() }
func (i hfInfo) Sys() any           { return &syscall.Stat_t{} }

func (f hfExecFS) Lstat(name string) (os.FileInfo, error) {
	if f.unsafe[name] {
		return hfInfo{mode: 0o775}, nil
	}
	if f.exes[name] {
		return hfInfo{mode: 0o755}, nil
	}
	if strings.Contains(name, "nosuch") {
		return nil, fs.ErrNotExist
	}
	return hfInfo{mode: os.ModeDir | 0o755}, nil
}

func (f hfExecFS) Readlink(string) (string, error) { return "", fs.ErrInvalid }

// unitCall is one fake systemd-run call.
type unitCall struct {
	args  []string
	argv  []string // after "--"
	stdin []byte
	isNil bool
}

// hfEnv is a helper rooted in a temp dir with /etc/dd-worker/policy.toml
// and secrets.env owned by the test user (standing in for root).
type hfEnv struct {
	t     *testing.T
	root  string
	sys   Sys
	mu    sync.Mutex
	units []unitCall
	ctl   [][]string
	logs  []string
	// run decides each fake command: kind is check, idle or health.
	run func(kind string, call unitCall) (int, string, error)
	// show is the ActiveState/InvocationID pair each show call returns, in
	// order; the last repeats.
	show []string
}

const hfPolicyBody = `host_files       = ["dd-policy=/etc/dd-worker/policy.toml", "dd-secrets=/etc/dd-worker/secrets.env", "plain=/etc/plain/app.conf"]
host_file_modes  = ["dd-policy:0644", "dd-secrets:0600", "plain:0644"]
host_file_secret = ["dd-secrets"]
host_file_units  = ["dd-policy:dd-worker.service", "dd-secrets:dd-worker.service", "plain:plain.service"]
host_file_run_as = ["dd-policy:dd-worker", "dd-secrets:dd-worker", "plain:dd-worker"]
host_file_checks = ["dd-policy:/usr/local/bin/dd-worker policy check --file -", "dd-secrets:/usr/local/bin/dd-worker secrets check --file -", "plain:/usr/bin/plaincheck"]
host_file_health = ["dd-policy:/usr/local/bin/dd-worker health --wait 30s", "dd-secrets:/usr/local/bin/dd-worker health --wait 30s"]
host_file_idle   = ["dd-policy:/usr/local/bin/dd-worker health --idle", "dd-secrets:/usr/local/bin/dd-worker health --idle"]
`

const (
	oldPolicy  = "managed_url = \"x\"\nrepo = \"old\"\n"
	newPolicy  = "managed_url = \"x\"\nrepo = \"new\"\n"
	oldSecrets = "DD_TOKEN=s3cret-old-value\n"
	newSecrets = "DD_TOKEN=s3cret-NEW-value\n"
)

func newHFEnv(t *testing.T) *hfEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &hfEnv{t: t, root: root}
	uid, gid := os.Getuid(), os.Getgid()
	for _, d := range []string{"etc", "etc/dd-worker", "etc/plain", "cfg"} {
		p := filepath.Join(root, d)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, uid, gid); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.writeTarget("etc/dd-worker/policy.toml", oldPolicy, 0o644)
	e.writeTarget("etc/dd-worker/secrets.env", oldSecrets, 0o600)
	e.writeTarget("etc/plain/app.conf", "a=1\n", 0o644)
	pol := filepath.Join(root, "cfg", "runner.toml")
	if err := os.WriteFile(pol, []byte(hfPolicyBody), 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(root, "cfg", "passwd")
	group := filepath.Join(root, "cfg", "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0::/root:/bin/sh\ndd-worker:x:1500:1501::/var/lib/dd-worker:/usr/sbin/nologin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\ndd-worker:x:1501:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.show = []string{"ActiveState=active\nInvocationID=aaa\n"}
	e.run = func(string, unitCall) (int, string, error) { return 0, "", nil }
	e.sys = Sys{
		Root:   root,
		Policy: pol,
		Passwd: passwd,
		Group:  group,
		UID:    func(os.FileInfo) (uint32, bool) { return 0, true },
		Logf: func(f string, a ...any) {
			e.mu.Lock()
			e.logs = append(e.logs, fmt.Sprintf(f, a...))
			e.mu.Unlock()
		},
		fileOwner: &fileIDs{uid: uid, gid: gid},
		execFS: hfExecFS{exes: map[string]bool{
			"/usr/local/bin/dd-worker": true, "/usr/bin/plaincheck": true,
		}},
		sleep: func(context.Context, time.Duration) {},
	}
	e.sys.Systemctl = func(_ context.Context, args []string) (string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.ctl = append(e.ctl, append([]string(nil), args...))
		if args[0] == "show" {
			out := e.show[0]
			if len(e.show) > 1 {
				e.show = e.show[1:]
			}
			return out, nil
		}
		return "", nil
	}
	e.sys.RunUnit = func(_ context.Context, args []string, stdin *os.File) (int, []byte, error) {
		c := unitCall{args: append([]string(nil), args...), isNil: stdin == nil}
		for i, a := range args {
			if a == "--" {
				c.argv = args[i+1:]
				break
			}
		}
		if stdin != nil {
			b, err := io.ReadAll(stdin)
			if err != nil {
				return -1, nil, err
			}
			c.stdin = b
		}
		e.mu.Lock()
		e.units = append(e.units, c)
		e.mu.Unlock()
		kind := "check"
		for _, a := range c.argv {
			if a == "--idle" {
				kind = "idle"
			} else if a == "health" && kind != "idle" {
				kind = "health"
			}
		}
		if kind == "health" {
			for _, a := range c.argv {
				if a == "--idle" {
					kind = "idle"
				}
			}
		}
		code, stderr, err := e.run(kind, c)
		return code, []byte(stderr), err
	}
	return e
}

func (e *hfEnv) writeTarget(rel, body string, mode os.FileMode) {
	e.t.Helper()
	p := filepath.Join(e.root, rel)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chown(p, os.Getuid(), os.Getgid()); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		e.t.Fatal(err)
	}
}

func (e *hfEnv) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func hfApplyReq(name, content string, extra string) []byte {
	return []byte(`{"name":"` + name + `","content_b64":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"` + extra + `}`)
}

func (e *hfEnv) apply(name, content, extra string) hostfiles.ApplyResult {
	e.t.Helper()
	raw := Handle(context.Background(), hostfiles.VerbApply, hfApplyReq(name, content, extra), e.sys)
	var r hostfiles.ApplyResult
	if err := hostfiles.DecodeStrict(raw, &r); err != nil {
		e.t.Fatalf("reply %s: %v", raw, err)
	}
	if !hostfiles.ValidResult(r) {
		e.t.Fatalf("invalid result %+v", r)
	}
	e.assertNoSecrets(string(raw))
	return r
}

func (e *hfEnv) restarts() int {
	n := 0
	for _, c := range e.ctl {
		if c[0] == "restart" {
			n++
		}
	}
	return n
}

func (e *hfEnv) assertNoCandidate(dir string) {
	e.t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.root, dir))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), hostfiles.CandidateSuffix) {
			e.t.Fatalf("candidate left: %s", ent.Name())
		}
	}
}

// assertNoSecrets: no secret value, nor its hash, in s or in any log line.
func (e *hfEnv) assertNoSecrets(s string) {
	e.t.Helper()
	all := s + strings.Join(e.logs, "\n")
	for _, v := range []string{"s3cret", sha256Hex([]byte(oldSecrets)), sha256Hex([]byte(newSecrets)), "repo = "} {
		if strings.Contains(all, v) {
			e.t.Fatalf("leaked %q in %q", v, all)
		}
	}
}

func appliedOf(t *testing.T, e *hfEnv, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, "var/lib/bot-agent-runner/host-files", name+".applied"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// §5 item 3 and item 9.
func TestHostFileApplySucceeds(t *testing.T) {
	e := newHFEnv(t)
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || r.Stage != "" || r.Reason != "" || r.SHA256 != sha256Hex([]byte(newPolicy)) {
		t.Fatalf("%+v", r)
	}
	if got := e.read("etc/dd-worker/policy.toml"); got != newPolicy {
		t.Fatalf("content %q", got)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(e.root, "etc/dd-worker/policy.toml"), &st); err != nil {
		t.Fatal(err)
	}
	if permOf(st) != 0o644 || nlinkOf(st) != 1 || int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Fatalf("stat mode %o nlink %d uid %d gid %d", permOf(st), nlinkOf(st), st.Uid, st.Gid)
	}
	if e.restarts() != 1 {
		t.Fatalf("restarts %v", e.ctl)
	}
	if appliedOf(t, e, "dd-policy") != sha256Hex([]byte(newPolicy)) {
		t.Fatal(".applied")
	}
	prev, err := os.ReadFile(filepath.Join(e.root, "var/lib/bot-agent-runner/host-files/dd-policy.prev"))
	if err != nil || string(prev) != oldPolicy {
		t.Fatalf(".prev %q %v", prev, err)
	}
	fi, err := os.Stat(filepath.Join(e.root, "var/lib/bot-agent-runner/host-files"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir %v %v", fi, err)
	}
	e.assertNoCandidate("etc/dd-worker")
	// check, idle, health in that order, each as dd-worker.
	if len(e.units) != 3 {
		t.Fatalf("units %+v", e.units)
	}
	check, idle, health := e.units[0], e.units[1], e.units[2]
	if string(check.stdin) != newPolicy || !idle.isNil || !health.isNil {
		t.Fatalf("stdin check %q idle nil %t health nil %t", check.stdin, idle.isNil, health.isNil)
	}
	if strings.Join(check.argv, " ") != "/usr/local/bin/dd-worker policy check --file -" ||
		strings.Join(idle.argv, " ") != "/usr/local/bin/dd-worker health --idle" ||
		strings.Join(health.argv, " ") != "/usr/local/bin/dd-worker health --wait 30s" {
		t.Fatalf("argv %v %v %v", check.argv, idle.argv, health.argv)
	}
	head := strings.Join(check.args[:len(check.args)-len(check.argv)], " ")
	for _, want := range []string{
		"--wait --pipe --collect --quiet --uid=1500 --gid=1501",
		"-p NoNewPrivileges=yes -p ProtectSystem=strict -p ProtectHome=yes -p PrivateTmp=yes -p PrivateNetwork=yes",
		"-p RuntimeMaxSec=15 ",
		"--unit=deploybot-hostfile-check-dd-policy-",
	} {
		if !strings.Contains(head, want) {
			t.Fatalf("missing %q in %q", want, head)
		}
	}
	if !strings.HasSuffix(head, " --") {
		t.Fatalf("no -- in %q", head)
	}
	// Rev 1a A3: every call (check, idle, health) carries the ids and the
	// whole sandbox, each property as its own -p.
	for _, u := range []unitCall{check, idle, health} {
		all := strings.Join(u.args, "\x00")
		for _, want := range append([]string{"--uid=1500", "--gid=1501"}, hfWantProps...) {
			if !strings.Contains("\x00"+all+"\x00", "\x00"+want+"\x00") {
				t.Fatalf("%v missing %q", u.argv, want)
			}
		}
	}
	if !strings.Contains(strings.Join(idle.args, " "), "RuntimeMaxSec=10 ") {
		t.Fatalf("idle budget %v", idle.args)
	}
	// Health gets what is left of the 60 s default window.
	if !strings.Contains(strings.Join(health.args, " "), "RuntimeMaxSec=60 ") {
		t.Fatalf("health budget %v", health.args)
	}
	if check.args[len(check.args)-len(check.argv)-2] == idle.args[len(idle.args)-len(idle.argv)-2] {
		t.Fatal("unit names repeat")
	}
}

// hfWantProps is every -p the spec (§2.4 and rev 1a A3) asks for.
var hfWantProps = []string{
	"-p\x00NoNewPrivileges=yes", "-p\x00ProtectSystem=strict", "-p\x00ProtectHome=yes", "-p\x00PrivateTmp=yes",
	"-p\x00PrivateNetwork=yes", "-p\x00PrivateDevices=yes", "-p\x00ProtectKernelTunables=yes",
	"-p\x00ProtectKernelModules=yes", "-p\x00ProtectControlGroups=yes", "-p\x00RestrictSUIDSGID=yes",
	"-p\x00LockPersonality=yes", "-p\x00LimitCORE=0", "-p\x00TimeoutStopSec=2", "-p\x00MemoryMax=256M",
	"-p\x00TasksMax=64", "-p\x00ProtectProc=invisible", "-p\x00ProcSubset=pid",
	"-p\x00InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/containerd/containerd.sock -/run/podman/podman.sock",
}

// §5 item 4.
func TestHostFileApplyUnchanged(t *testing.T) {
	e := newHFEnv(t)
	r := e.apply("dd-policy", oldPolicy, "")
	if r.Outcome != hostfiles.OutcomeUnchanged || r.SHA256 != sha256Hex([]byte(oldPolicy)) {
		t.Fatalf("%+v", r)
	}
	if len(e.ctl) != 0 || len(e.units) != 0 {
		t.Fatalf("ran %v %v", e.ctl, e.units)
	}
	if appliedOf(t, e, "dd-policy") != sha256Hex([]byte(oldPolicy)) {
		t.Fatal("unchanged not recorded")
	}
}

// §5 item 5.
func TestHostFileApplyCheckFails(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		stderr string
		err    error
		want   string
	}{
		{"bad reason", 2, "bad_repo\n", nil, "bad_repo"},
		{"bad reason no newline", 2, "bad_repo", nil, "bad_repo"},
		{"two lines", 2, "bad_repo\nbad_other\n", nil, hostfiles.ReasonCheckError},
		{"exit 5", 5, "bad_repo\n", nil, hostfiles.ReasonCheckError},
		{"exit 1 usage", 1, "", nil, hostfiles.ReasonCheckError},
		{"malformed", 2, "Bad repo\n", nil, hostfiles.ReasonCheckError},
		{"empty", 2, "", nil, hostfiles.ReasonCheckError},
		{"timeout", -1, "", context.DeadlineExceeded, hostfiles.ReasonCheckError},
		{"2 KiB stderr", 2, "bad_repo\n" + strings.Repeat("x", 2048), nil, hostfiles.ReasonCheckError},
		{"long token", 2, "bad_" + strings.Repeat("a", 65) + "\n", nil, hostfiles.ReasonCheckError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newHFEnv(t)
			e.run = func(kind string, _ unitCall) (int, string, error) {
				if kind == "check" {
					stderr := c.stderr
					if len(stderr) > hostfiles.MaxStderr+1 {
						stderr = stderr[:hostfiles.MaxStderr+1] // RunUnit keeps at most this much
					}
					return c.code, stderr, c.err
				}
				t.Fatalf("%s ran after a failed check", kind)
				return 0, "", nil
			}
			r := e.apply("dd-policy", newPolicy, "")
			if r.Outcome != hostfiles.OutcomeCheckFailed || r.Stage != hostfiles.StageCheck || r.Reason != c.want {
				t.Fatalf("%+v", r)
			}
			if e.read("etc/dd-worker/policy.toml") != oldPolicy || len(e.ctl) != 0 {
				t.Fatalf("target or unit touched: %v", e.ctl)
			}
			e.assertNoCandidate("etc/dd-worker")
			for _, l := range e.logs {
				if strings.Contains(l, "xxxx") || strings.Contains(l, "bad_other") {
					t.Fatalf("stderr logged: %q", l)
				}
			}
		})
	}
}

// §5 item 5b.
func TestHostFileApplyBusyAndForce(t *testing.T) {
	e := newHFEnv(t)
	e.run = func(kind string, _ unitCall) (int, string, error) {
		if kind == "idle" {
			return 4, "", nil
		}
		return 0, "", nil
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeWorkerBusy || r.Stage != hostfiles.StageIdle {
		t.Fatalf("%+v", r)
	}
	if e.read("etc/dd-worker/policy.toml") != oldPolicy || e.restarts() != 0 {
		t.Fatal("busy swapped or restarted")
	}
	e.assertNoCandidate("etc/dd-worker")
	e.units = nil
	r = e.apply("dd-policy", newPolicy, `,"force":true`)
	if r.Outcome != hostfiles.OutcomeSucceeded || e.read("etc/dd-worker/policy.toml") != newPolicy {
		t.Fatalf("force %+v", r)
	}
	for _, u := range e.units {
		if strings.Contains(strings.Join(u.argv, " "), "--idle") {
			t.Fatal("force ran the idle check")
		}
	}
}

func TestHostFileApplyIdleError(t *testing.T) {
	for name, res := range map[string]struct {
		code int
		err  error
	}{"exit 1": {1, nil}, "timeout": {-1, context.DeadlineExceeded}} {
		t.Run(name, func(t *testing.T) {
			e := newHFEnv(t)
			e.run = func(kind string, _ unitCall) (int, string, error) {
				if kind == "idle" {
					return res.code, "", res.err
				}
				return 0, "", nil
			}
			r := e.apply("dd-policy", newPolicy, "")
			if r.Outcome != hostfiles.OutcomeCheckFailed || r.Stage != hostfiles.StageIdle || r.Reason != hostfiles.ReasonIdleError {
				t.Fatalf("%+v", r)
			}
			if e.read("etc/dd-worker/policy.toml") != oldPolicy || e.restarts() != 0 {
				t.Fatal("swapped")
			}
		})
	}
}

// §5 item 6, health branch.
func TestHostFileApplyHealthFailsRollsBack(t *testing.T) {
	for _, c := range []struct {
		code   int
		stderr string
		err    error
		want   string
	}{
		{3, "not_connected\n", nil, hostfiles.ReasonNotConnected},
		{3, "policy_invalid\n", nil, hostfiles.ReasonPolicyInvalid},
		{3, "not_running\n", nil, hostfiles.ReasonNotRunning},
		{3, "bad_repo\n", nil, hostfiles.ReasonHealthError},
		{1, "", nil, hostfiles.ReasonHealthError},
		{-1, "", errors.New("killed"), hostfiles.ReasonHealthError},
	} {
		t.Run(c.want+c.stderr, func(t *testing.T) {
			e := newHFEnv(t)
			healths := 0
			e.run = func(kind string, _ unitCall) (int, string, error) {
				if kind != "health" {
					return 0, "", nil
				}
				healths++
				if healths == 1 {
					// The new file is in place while the first health runs.
					if e.read("etc/dd-worker/policy.toml") != newPolicy {
						t.Fatal("health ran before the swap")
					}
					return c.code, c.stderr, c.err
				}
				return 0, "", nil
			}
			r := e.apply("dd-policy", newPolicy, "")
			if r.Outcome != hostfiles.OutcomeRolledBack || r.Stage != hostfiles.StageHealth || r.Reason != c.want {
				t.Fatalf("%+v", r)
			}
			if e.read("etc/dd-worker/policy.toml") != oldPolicy || e.restarts() != 2 || healths != 2 {
				t.Fatalf("not rolled back: restarts %d healths %d", e.restarts(), healths)
			}
			e.assertNoCandidate("etc/dd-worker")
			if _, err := os.Stat(filepath.Join(e.root, "var/lib/bot-agent-runner/host-files/dd-policy.applied")); !os.IsNotExist(err) {
				t.Fatal("rolled back apply recorded")
			}
		})
	}
}

// §5 item 6, no health command: a unit that does not stay up rolls back
// with stage restart.
func TestHostFileApplyNoHealthRestartBranch(t *testing.T) {
	for name, c := range map[string]struct {
		show []string
		want string
	}{
		"stays up":            {[]string{"ActiveState=active\nInvocationID=a\n"}, hostfiles.OutcomeSucceeded},
		"not active":          {[]string{"ActiveState=failed\nInvocationID=a\n", "ActiveState=active\nInvocationID=b\n"}, hostfiles.OutcomeRolledBack},
		"exits in window":     {[]string{"ActiveState=active\nInvocationID=a\n", "ActiveState=failed\nInvocationID=a\n", "ActiveState=active\nInvocationID=c\n"}, hostfiles.OutcomeRolledBack},
		"restarted in window": {[]string{"ActiveState=active\nInvocationID=a\n", "ActiveState=active\nInvocationID=b\n", "ActiveState=active\nInvocationID=c\n"}, hostfiles.OutcomeRolledBack},
	} {
		t.Run(name, func(t *testing.T) {
			e := newHFEnv(t)
			e.show = c.show
			var slept time.Duration
			e.sys.sleep = func(_ context.Context, d time.Duration) { slept += d }
			r := e.apply("plain", "a=2\n", `,"settle_sec":30`)
			if r.Outcome != c.want {
				t.Fatalf("%+v", r)
			}
			if slept < 29*time.Second {
				t.Fatalf("did not wait out the window: %v", slept)
			}
			if c.want == hostfiles.OutcomeRolledBack {
				if r.Stage != hostfiles.StageRestart || e.read("etc/plain/app.conf") != "a=1\n" || e.restarts() != 2 {
					t.Fatalf("%+v restarts %d", r, e.restarts())
				}
			} else if e.read("etc/plain/app.conf") != "a=2\n" || e.restarts() != 1 {
				t.Fatal("not applied")
			}
		})
	}
}

func TestHostFileApplyRestartCommandFails(t *testing.T) {
	e := newHFEnv(t)
	calls := 0
	ctl := e.sys.Systemctl
	e.sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
		out, err := ctl(ctx, args)
		if args[0] == "restart" {
			calls++
			if calls == 1 {
				return "", errors.New("exit 1")
			}
		}
		return out, err
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeRolledBack || r.Stage != hostfiles.StageRestart || e.read("etc/dd-worker/policy.toml") != oldPolicy {
		t.Fatalf("%+v", r)
	}
}

// §5 item 7.
func TestHostFileApplyRollbackFails(t *testing.T) {
	e := newHFEnv(t)
	e.run = func(kind string, _ unitCall) (int, string, error) {
		if kind == "health" {
			return 3, "not_connected\n", nil
		}
		return 0, "", nil
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeRollbackFailed || r.Stage != hostfiles.StageRollback || r.Reason != hostfiles.ReasonNotConnected {
		t.Fatalf("%+v", r)
	}
	// The previous bytes are back even though the unit did not confirm.
	if e.read("etc/dd-worker/policy.toml") != oldPolicy || e.restarts() != 2 {
		t.Fatal("restore")
	}
}

// §5 item 8.
func TestHostFileApplyUnsafeTargets(t *testing.T) {
	type setup func(t *testing.T, e *hfEnv)
	target := func(e *hfEnv) string { return filepath.Join(e.root, "etc/dd-worker/policy.toml") }
	cases := map[string]struct {
		name string
		prep setup
	}{
		"symlink target": {"dd-policy", func(t *testing.T, e *hfEnv) {
			other := filepath.Join(e.root, "cfg", "other.toml")
			e.writeTarget("cfg/other.toml", oldPolicy, 0o644)
			os.Remove(target(e))
			if err := os.Symlink(other, target(e)); err != nil {
				t.Fatal(err)
			}
		}},
		"symlink dir": {"dd-policy", func(t *testing.T, e *hfEnv) {
			real := filepath.Join(e.root, "cfg", "real")
			if err := os.Rename(filepath.Join(e.root, "etc/dd-worker"), real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(e.root, "etc/dd-worker")); err != nil {
				t.Fatal(err)
			}
		}},
		"group not root": {"dd-policy", func(t *testing.T, e *hfEnv) {
			e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid() + 1}
		}},
		"owner not root": {"dd-policy", func(t *testing.T, e *hfEnv) {
			e.sys.fileOwner = &fileIDs{uid: os.Getuid() + 1, gid: os.Getgid()}
		}},
		"group-writable file": {"dd-policy", func(t *testing.T, e *hfEnv) {
			if err := os.Chmod(target(e), 0o664); err != nil {
				t.Fatal(err)
			}
		}},
		"wrong mode": {"dd-secrets", func(t *testing.T, e *hfEnv) {
			if err := os.Chmod(filepath.Join(e.root, "etc/dd-worker/secrets.env"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		"group-writable dir": {"dd-policy", func(t *testing.T, e *hfEnv) {
			if err := os.Chmod(filepath.Join(e.root, "etc/dd-worker"), 0o775); err != nil {
				t.Fatal(err)
			}
		}},
		"nlink 2": {"dd-policy", func(t *testing.T, e *hfEnv) {
			if err := os.Link(target(e), filepath.Join(e.root, "cfg", "hard")); err != nil {
				t.Fatal(err)
			}
		}},
		"missing": {"dd-policy", func(t *testing.T, e *hfEnv) {
			if err := os.Remove(target(e)); err != nil {
				t.Fatal(err)
			}
		}},
		"directory target": {"dd-policy", func(t *testing.T, e *hfEnv) {
			os.Remove(target(e))
			if err := os.Mkdir(target(e), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		"unknown name": {"nosuch", func(*testing.T, *hfEnv) {}},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			e := newHFEnv(t)
			c.prep(t, e)
			r := e.apply(c.name, newPolicy, "")
			if r.Outcome != hostfiles.OutcomeForbiddenSpec {
				t.Fatalf("%+v", r)
			}
			wantStage := hostfiles.StageTarget
			if c.name == "nosuch" {
				wantStage = hostfiles.StageLookup
			}
			if r.Stage != wantStage {
				t.Fatalf("stage %+v", r)
			}
			if len(e.units) != 0 || len(e.ctl) != 0 {
				t.Fatal("ran something")
			}
			if b, err := os.ReadFile(filepath.Join(e.root, "cfg", "other.toml")); err == nil && string(b) != oldPolicy {
				t.Fatal("wrote through the symlink")
			}
			if fi, err := os.Lstat(filepath.Join(e.root, "etc/dd-worker")); err == nil && fi.IsDir() {
				e.assertNoCandidate("etc/dd-worker")
			}
		})
	}
}

// Spec §2.1: the executable is checked at apply time too.
func TestHostFileApplyUnsafeExecutable(t *testing.T) {
	e := newHFEnv(t)
	e.sys.execFS = hfExecFS{
		exes:   map[string]bool{"/usr/local/bin/dd-worker": true, "/usr/bin/plaincheck": true},
		unsafe: map[string]bool{"/usr/local/bin/dd-worker": true},
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeForbiddenSpec || r.Stage != hostfiles.StageExec || len(e.units) != 0 || e.read("etc/dd-worker/policy.toml") != oldPolicy {
		t.Fatalf("%+v", r)
	}
	// Rev 1a A2: the other file (a safe executable) still applies.
	if r := e.apply("plain", "a=2\n", ""); r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("plain %+v", r)
	}
}

// An unsafe idle or health executable is stage exec too, and nothing runs.
func TestHostFileApplyUnsafeIdleOrHealth(t *testing.T) {
	for _, cmd := range []string{"idle", "health"} {
		t.Run(cmd, func(t *testing.T) {
			e := newHFEnv(t)
			body := strings.Replace(hfPolicyBody, "dd-policy:/usr/local/bin/dd-worker health --"+map[string]string{"idle": "idle", "health": "wait 30s"}[cmd],
				"dd-policy:/usr/local/bin/unsafe-"+cmd+" x", 1)
			if err := os.WriteFile(e.sys.Policy, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			e.sys.execFS = hfExecFS{
				exes:   map[string]bool{"/usr/local/bin/dd-worker": true, "/usr/local/bin/unsafe-" + cmd: true},
				unsafe: map[string]bool{"/usr/local/bin/unsafe-" + cmd: true},
			}
			r := e.apply("dd-policy", newPolicy, "")
			if r.Outcome != hostfiles.OutcomeForbiddenSpec || r.Stage != hostfiles.StageExec || len(e.units) != 0 {
				t.Fatalf("%+v %d", r, len(e.units))
			}
		})
	}
}

// The settle_sec bounds, including the health --wait + 15 rule.
func TestHostFileApplySettle(t *testing.T) {
	healthBudget := func(e *hfEnv) string {
		for _, u := range e.units {
			if strings.Contains(strings.Join(u.argv, " "), "--wait") {
				for _, a := range u.args {
					if strings.HasPrefix(a, "RuntimeMaxSec=") {
						return a
					}
				}
			}
		}
		return ""
	}
	for _, c := range []struct {
		extra   string
		outcome string
		budget  string
		refused bool
	}{
		{"", hostfiles.OutcomeSucceeded, "RuntimeMaxSec=60", false},
		{`,"settle_sec":44`, hostfiles.OutcomeForbiddenSpec, "", false},
		{`,"settle_sec":45`, hostfiles.OutcomeSucceeded, "RuntimeMaxSec=45", false},
		{`,"settle_sec":180`, hostfiles.OutcomeSucceeded, "RuntimeMaxSec=180", false},
		{`,"settle_sec":29`, "", "", true},
		{`,"settle_sec":181`, "", "", true},
	} {
		e := newHFEnv(t)
		raw := Handle(context.Background(), hostfiles.VerbApply, hfApplyReq("dd-policy", newPolicy, c.extra), e.sys)
		if c.refused {
			if !bytes.Contains(raw, []byte(`"reason":"bad_spec"`)) {
				t.Fatalf("%s: %s", c.extra, raw)
			}
			continue
		}
		var r hostfiles.ApplyResult
		if err := json.Unmarshal(raw, &r); err != nil || r.Outcome != c.outcome {
			t.Fatalf("%s: %s", c.extra, raw)
		}
		if c.outcome == hostfiles.OutcomeForbiddenSpec && r.Stage != hostfiles.StageSettle {
			t.Fatalf("%s: %+v", c.extra, r)
		}
		if got := healthBudget(e); got != c.budget {
			t.Fatalf("%s: budget %q", c.extra, got)
		}
	}
}

func TestHostFileApplyDefaultSettleFollowsWait(t *testing.T) {
	e := newHFEnv(t)
	body := strings.Replace(hfPolicyBody, "health --wait 30s\", \"dd-secrets", "health --wait 70s\", \"dd-secrets", 1)
	if err := os.WriteFile(e.sys.Policy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeSucceeded {
		t.Fatalf("%+v", r)
	}
	last := e.units[len(e.units)-1]
	if !strings.Contains(strings.Join(last.args, " "), "RuntimeMaxSec=85 ") {
		t.Fatalf("default settle not raised: %v", last.args)
	}
	r = e.apply("dd-policy", oldPolicy, `,"settle_sec":60`)
	if r.Outcome != hostfiles.OutcomeForbiddenSpec || r.Stage != hostfiles.StageSettle {
		t.Fatalf("%+v", r)
	}
}

func TestHostFileApplyBadRequests(t *testing.T) {
	e := newHFEnv(t)
	for _, body := range []string{
		`{}`,
		`{"name":"dd-policy","content_b64":""}`,
		`{"name":"dd-policy","content_b64":"!!"}`,
		`{"name":"DD","content_b64":"eA=="}`,
		`{"name":"dd-policy","content_b64":"eA==","extra":1}`,
		`{"name":"dd-policy","content_b64":"` + base64.StdEncoding.EncodeToString(make([]byte, hostfiles.MaxContent+1)) + `"}`,
	} {
		raw := Handle(context.Background(), hostfiles.VerbApply, []byte(body), e.sys)
		if !bytes.Contains(raw, []byte(`"reason":"bad_spec"`)) {
			t.Fatalf("%.60s: %s", body, raw)
		}
	}
	big := bytes.Repeat([]byte("x"), hostfiles.MaxFrame+1)
	if raw := Handle(context.Background(), hostfiles.VerbApply, big, e.sys); !bytes.Contains(raw, []byte(`"reason":"bad_spec"`)) {
		t.Fatalf("%s", raw)
	}
	// A 64 KiB file is accepted and reaches the check byte for byte.
	content := strings.Repeat("k = \"v\"\n", hostfiles.MaxContent/8)
	r := e.apply("dd-policy", content, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || string(e.units[0].stdin) != content {
		t.Fatalf("%+v %d", r, len(e.units[0].stdin))
	}
}

// §5 item 10 (helper side) and the status verb.
func TestHostFileSecretsAndStatus(t *testing.T) {
	e := newHFEnv(t)
	status := func() map[string]hostfiles.FileStatus {
		raw := Handle(context.Background(), hostfiles.VerbStatus, []byte(`{}`), e.sys)
		e.assertNoSecrets(string(raw))
		var s hostfiles.StatusResult
		if err := hostfiles.DecodeStrict(raw, &s); err != nil || !hostfiles.ValidStatus(s) {
			t.Fatalf("%s %v", raw, err)
		}
		out := map[string]hostfiles.FileStatus{}
		for _, f := range s.Files {
			out[f.Name] = f
		}
		if len(s.Files) != 3 || s.Files[0].Name != "dd-policy" {
			t.Fatalf("order %s", raw)
		}
		if !bytes.Contains(raw, []byte(`"matches_applied":`)) {
			t.Fatalf("matches_applied omitted: %s", raw)
		}
		return out
	}
	s := status()
	if f := s["dd-secrets"]; !f.Exists || !f.OwnerOK || f.MatchesApplied != nil || f.Size != nil || f.SHA256 != "" {
		t.Fatalf("secret before %+v", f)
	}
	if f := s["dd-policy"]; f.Size == nil || *f.Size != int64(len(oldPolicy)) || f.SHA256 != sha256Hex([]byte(oldPolicy)) {
		t.Fatalf("plain before %+v", f)
	}
	r := e.apply("dd-secrets", newSecrets, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || r.SHA256 != "" {
		t.Fatalf("%+v", r)
	}
	if e.read("etc/dd-worker/secrets.env") != newSecrets {
		t.Fatal("not applied")
	}
	s = status()
	if f := s["dd-secrets"]; f.MatchesApplied == nil || !*f.MatchesApplied {
		t.Fatalf("after apply %+v", f)
	}
	// A hand edit on the host flips matches_applied.
	e.writeTarget("etc/dd-worker/secrets.env", "DD_TOKEN=s3cret-hand-edit\n", 0o600)
	s = status()
	if f := s["dd-secrets"]; f.MatchesApplied == nil || *f.MatchesApplied {
		t.Fatalf("after hand edit %+v", f)
	}
	// A failed secret check is logged with its reason only.
	e.run = func(kind string, _ unitCall) (int, string, error) { return 2, "bad_token\n", nil }
	r = e.apply("dd-secrets", newSecrets, "")
	if r.Outcome != hostfiles.OutcomeCheckFailed || r.Reason != "bad_token" || r.SHA256 != "" {
		t.Fatalf("%+v", r)
	}
	e.assertNoSecrets("")
}

func TestHostFileStatusReasons(t *testing.T) {
	e := newHFEnv(t)
	if err := os.Remove(filepath.Join(e.root, "etc/plain/app.conf")); err != nil {
		t.Fatal(err)
	}
	e.writeTarget("etc/dd-worker/policy.toml", strings.Repeat("x", hostfiles.MaxContent+1), 0o644)
	if err := os.Chmod(filepath.Join(e.root, "etc/dd-worker/secrets.env"), 0o640); err != nil {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), hostfiles.VerbStatus, []byte(`{}`), e.sys)
	var s hostfiles.StatusResult
	if err := hostfiles.DecodeStrict(raw, &s); err != nil {
		t.Fatal(err)
	}
	got := map[string]hostfiles.FileStatus{}
	for _, f := range s.Files {
		got[f.Name] = f
	}
	if f := got["plain"]; f.Exists || f.OwnerOK || f.Reason != hostfiles.StatusMissing {
		t.Fatalf("missing %+v", f)
	}
	if f := got["dd-policy"]; !f.Exists || f.OwnerOK || f.Reason != hostfiles.StatusTooLarge || f.Size != nil {
		t.Fatalf("too large %+v", f)
	}
	if f := got["dd-secrets"]; !f.Exists || f.OwnerOK || f.Reason != hostfiles.StatusMode {
		t.Fatalf("mode %+v", f)
	}
	if raw := Handle(context.Background(), hostfiles.VerbStatus, []byte(`{"x":1}`), e.sys); !bytes.Contains(raw, []byte("bad_spec")) {
		t.Fatalf("%s", raw)
	}
}

func TestHostFileStatusInvalidPolicy(t *testing.T) {
	e := newHFEnv(t)
	if err := os.WriteFile(e.sys.Policy, []byte("host_files = [\"a=/etc/ssh/x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw := Handle(context.Background(), hostfiles.VerbStatus, []byte(`{}`), e.sys); !bytes.Contains(raw, []byte("policy_denied")) {
		t.Fatalf("%s", raw)
	}
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeForbiddenSpec || r.Stage != hostfiles.StageLookup {
		t.Fatalf("%+v", r)
	}
}

// A leftover candidate from a crashed apply is replaced.
func TestHostFileApplyLeftoverCandidate(t *testing.T) {
	e := newHFEnv(t)
	e.writeTarget("etc/dd-worker/.policy.toml.deploybot-new", "stale", 0o600)
	r := e.apply("dd-policy", newPolicy, "")
	if r.Outcome != hostfiles.OutcomeSucceeded || string(e.units[0].stdin) != newPolicy {
		t.Fatalf("%+v", r)
	}
	e.assertNoCandidate("etc/dd-worker")
}

func TestHostRunArgsOneLine(t *testing.T) {
	for in, want := range map[string]bool{
		"bad_x\n": true, "bad_x": true, "": false, "\n": false, "a\nb": false, "a\r": false,
		strings.Repeat("a", hostfiles.MaxStderr+1): false,
	} {
		if _, ok := oneLine([]byte(in)); ok != want {
			t.Fatalf("%q: %v", in, ok)
		}
	}
}
