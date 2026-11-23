package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/images"
)

type upEnv struct {
	t      *testing.T
	root   string
	sys    Sys
	mu     sync.Mutex
	active string
	units  [][]string
	ctl    [][]string
	runRC  int
}

func newUpEnv(t *testing.T, policy string) *upEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &upEnv{t: t, root: root, active: "inactive"}
	cfg := filepath.Join(root, "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(cfg, "runner.toml")
	if err := os.WriteFile(pol, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(cfg, "passwd")
	group := filepath.Join(cfg, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0::/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeRecord("0.2.15", true)
	e.sys = Sys{
		Root: root, Policy: pol, Passwd: passwd, Group: group,
		UID: func(os.FileInfo) (uint32, bool) { return 0, true },
	}
	e.sys.Systemctl = func(_ context.Context, args []string) (string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.ctl = append(e.ctl, append([]string(nil), args...))
		if args[0] == "is-active" {
			return e.active + "\n", nil
		}
		return "", nil
	}
	e.sys.RunUnit = func(_ context.Context, args []string, stdin *os.File) (int, []byte, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.units = append(e.units, append([]string(nil), args...))
		return e.runRC, nil, nil
	}
	return e
}

func (e *upEnv) writeRecord(version string, runner bool) {
	e.t.Helper()
	body := "version " + version + "\n" + strings.Repeat("a", 64) + "  /usr/local/bin/bot-agent\n"
	if runner {
		body += strings.Repeat("b", 64) + "  " + agentupgrade.RunnerPath + "\n"
	}
	path := InstallerRecordPath(e.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *upEnv) start(body string) string {
	e.t.Helper()
	return string(Handle(context.Background(), agentupgrade.VerbStart, []byte(body), e.sys))
}

const upgradeOn = "agent_upgrade = true\n"

func TestUpgradeStartAccepted(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	out := e.start(`{"version":"0.2.16"}`)
	if !agentupgrade.DecodeStartReply([]byte(out)) {
		t.Fatalf("reply %s", out)
	}
	want := [][]string{{
		"--unit=bot-agent-upgrade", "--collect", "--property=RuntimeMaxSec=900",
		"--property=TimeoutStopSec=150", "--property=KillMode=mixed",
		"/usr/local/libexec/bot-agent-runner", "upgrade-run", "0.2.16",
	}}
	if !reflect.DeepEqual(e.units, want) {
		t.Fatalf("systemd-run %q", e.units)
	}
	if len(e.ctl) != 1 || !reflect.DeepEqual(e.ctl[0], []string{"is-active", "bot-agent-upgrade.service"}) {
		t.Fatalf("systemctl %q", e.ctl)
	}
}

func refusal(t *testing.T, out string) string {
	t.Helper()
	var m struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil || m.OK {
		t.Fatalf("not a refusal: %s", out)
	}
	return m.Reason
}

// The policy gate comes first: with the key off nothing else runs, even
// for a bad version.
func TestUpgradeStartPolicyDenied(t *testing.T) {
	for _, pol := range []string{"", "agent_upgrade = false\n", "enabled = true\n", "agent_upgrade = true\nbogus = 1\n"} {
		e := newUpEnv(t, pol)
		for _, body := range []string{`{"version":"0.2.16"}`, `{"version":"x"}`} {
			if got := refusal(t, e.start(body)); got != "policy_denied" {
				t.Fatalf("%q %s: %s", pol, body, got)
			}
		}
		if len(e.units) != 0 || len(e.ctl) != 0 {
			t.Fatal("ran something with the policy off")
		}
	}
	e := newUpEnv(t, upgradeOn)
	if err := os.Remove(e.sys.Policy); err != nil {
		t.Fatal(err)
	}
	if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "policy_denied" {
		t.Fatalf("missing policy: %s", got)
	}
}

func TestUpgradeStartBadSpec(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	for _, body := range []string{
		`{}`, `{"version":"0.2"}`, `{"version":"v0.2.16"}`, `{"version":"0.2.16-rc1"}`,
		`{"version":"0.2.16;reboot"}`, `{"version":"0.2.16","url":"https://evil"}`,
		`{"version":"0.2.16","command":"sh"}`, `{"version":"10000.0.0"}`,
	} {
		if got := refusal(t, e.start(body)); got != "bad_spec" {
			t.Fatalf("%s: %s", body, got)
		}
	}
	if len(e.units) != 0 {
		t.Fatal("started a unit")
	}
}

func TestUpgradeStartNotNewer(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	for _, v := range []string{"0.2.15", "0.2.14", "0.1.99"} {
		if got := refusal(t, e.start(`{"version":"`+v+`"}`)); got != "not_newer" {
			t.Fatalf("%s: %s", v, got)
		}
	}
	// A missing record cannot prove the target is newer.
	if err := os.Remove(InstallerRecordPath(e.root)); err != nil {
		t.Fatal(err)
	}
	if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "not_newer" {
		t.Fatalf("no record: %s", got)
	}
	if len(e.units) != 0 {
		t.Fatal("started a unit")
	}
}

func holdLock(t *testing.T, path string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func TestUpgradeStartBusyHelperLock(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	release := holdLock(t, joinRoot(e.root, "run/bot-agent/helper.lock"))
	if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "busy" {
		t.Fatalf("helper lock held: %s", got)
	}
	release()
	if out := e.start(`{"version":"0.2.16"}`); !agentupgrade.DecodeStartReply([]byte(out)) {
		t.Fatalf("after release: %s", out)
	}
}

// An image deploy, an edge apply or a host-file apply holds its own lock;
// any of them makes the upgrade busy.
func TestUpgradeStartBusyOtherLocks(t *testing.T) {
	for _, name := range []string{"image-count.lock", "image-demo-web.lock", "edge.lock", "hostfile-unit-x.service.lock"} {
		e := newUpEnv(t, upgradeOn)
		release := holdLock(t, joinRoot(e.root, "run/bot-agent/"+name))
		if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "busy" {
			t.Fatalf("%s held: %s", name, got)
		}
		release()
		if out := e.start(`{"version":"0.2.16"}`); !agentupgrade.DecodeStartReply([]byte(out)) {
			t.Fatalf("%s free: %s", name, out)
		}
	}
}

func TestUpgradeStartBusyUnitActive(t *testing.T) {
	for _, st := range []string{"active", "activating", "deactivating"} {
		e := newUpEnv(t, upgradeOn)
		e.active = st
		if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "busy" {
			t.Fatalf("%s: %s", st, got)
		}
		if len(e.units) != 0 {
			t.Fatal("started a second unit")
		}
	}
	e := newUpEnv(t, upgradeOn)
	e.active = "failed"
	if out := e.start(`{"version":"0.2.16"}`); !agentupgrade.DecodeStartReply([]byte(out)) {
		t.Fatalf("failed unit: %s", out)
	}
}

func TestUpgradeStartSystemdRunFails(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	e.runRC = 1
	if got := refusal(t, e.start(`{"version":"0.2.16"}`)); got != "io_error" {
		t.Fatalf("%s", got)
	}
}

func TestUpgradeResultVerb(t *testing.T) {
	e := newUpEnv(t, "")
	read := func(body string) string {
		return string(Handle(context.Background(), agentupgrade.VerbResult, []byte(body), e.sys))
	}
	if out := read(`{}`); out != `{"ok":true,"result":null,"running":false}` {
		t.Fatalf("no file: %s", out)
	}
	if got := refusal(t, read(`{"x":1}`)); got != "bad_spec" {
		t.Fatalf("extra key: %s", got)
	}
	r := agentupgrade.Result{Version: "0.2.16", Outcome: "failed", Reason: "signature_invalid", At: "2026-11-23T10:00:00Z"}
	if err := os.MkdirAll(UpgradeStateDir(e.root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UpgradeResultPath(e.root), agentupgrade.Encode(r), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := agentupgrade.DecodeResultReply([]byte(read(`{}`)))
	if !ok || got.Result == nil || *got.Result != r || got.Running {
		t.Fatalf("result %+v %v", got, ok)
	}
	// A file that is not a valid result is null, not relayed.
	if err := os.WriteFile(UpgradeResultPath(e.root), []byte(`{"version":"0.2.16","outcome":"failed","reason":"/etc/shadow","at":"2026-11-23T10:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := read(`{}`); out != `{"ok":true,"result":null,"running":false}` {
		t.Fatalf("invalid file: %s", out)
	}
}

func TestRunnerInstalledFromRecord(t *testing.T) {
	e := newUpEnv(t, "")
	if !RunnerInstalled(e.root) {
		t.Fatal("runner line not seen")
	}
	if v, ok := InstalledVersion(e.root); !ok || v != "0.2.15" {
		t.Fatalf("version %q", v)
	}
	e.writeRecord("0.2.15", false)
	if RunnerInstalled(e.root) {
		t.Fatal("runner seen without its line")
	}
}

// L5: a failed or rolled-back result for the same version under ten
// minutes old is a cool-down; another version, a success or an old
// result is not.
func TestUpgradeStartCoolDown(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		r    agentupgrade.Result
		busy bool
	}{
		{agentupgrade.Result{Version: "0.2.16", Outcome: "failed", Reason: "download_failed", At: "2026-11-23T09:55:00Z"}, true},
		{agentupgrade.Result{Version: "0.2.16", Outcome: "rolled_back", Reason: "not_reconnected", At: "2026-11-23T09:51:00Z"}, true},
		{agentupgrade.Result{Version: "0.2.16", Outcome: "failed", Reason: "download_failed", At: "2026-11-23T09:49:00Z"}, false},
		{agentupgrade.Result{Version: "0.2.16", Outcome: "succeeded", At: "2026-11-23T09:59:00Z"}, false},
		{agentupgrade.Result{Version: "0.2.17", Outcome: "failed", Reason: "download_failed", At: "2026-11-23T09:59:00Z"}, false},
	} {
		e := newUpEnv(t, upgradeOn)
		e.sys.now = func() time.Time { return now }
		if err := os.MkdirAll(UpgradeStateDir(e.root), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(UpgradeResultPath(e.root), agentupgrade.Encode(tc.r), 0o600); err != nil {
			t.Fatal(err)
		}
		out := e.start(`{"version":"0.2.16"}`)
		if tc.busy {
			if got := refusal(t, out); got != "busy" {
				t.Fatalf("%+v: %s", tc.r, out)
			}
		} else if !agentupgrade.DecodeStartReply([]byte(out)) {
			t.Fatalf("%+v: %s", tc.r, out)
		}
	}
}

// L6: while upgrade-run holds its lock, one mutating verb of each family
// answers busy in that family's shape, a read-only verb is not refused for
// it, and the result verb reports running.
func TestMutatingVerbsBusyDuringUpgrade(t *testing.T) {
	e := newUpEnv(t, upgradeOn)
	release := holdLock(t, UpgradeLockPath(e.root))
	for verb, body := range map[string]string{
		"start":                `{"project":"a","entry":"b"}`,
		images.VerbDeploy:      `{}`,
		edge.VerbApply:         `{}`,
		hostfiles.VerbApply:    `{}`,
		images.VerbRestart:     `{}`,
		"stop":                 `{}`,
		edge.VerbStop:          `{}`,
		agentupgrade.VerbStart: `{"version":"0.2.16"}`,
	} {
		out := string(Handle(context.Background(), verb, []byte(body), e.sys))
		var m struct {
			OK     *bool  `json:"ok"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &m); err != nil || (m.Reason != "busy" && m.Error != "busy") {
			t.Fatalf("%s: %s", verb, out)
		}
	}
	out := string(Handle(context.Background(), "list", []byte(`{}`), e.sys))
	if strings.Contains(out, "busy") {
		t.Fatalf("list refused busy: %s", out)
	}
	rr, ok := agentupgrade.DecodeResultReply(Handle(context.Background(), agentupgrade.VerbResult, []byte(`{}`), e.sys))
	if !ok || !rr.Running {
		t.Fatalf("running %+v %v", rr, ok)
	}
	release()
	if UpgradeRunning(e.root) {
		t.Fatal("still running after release")
	}
	out = string(Handle(context.Background(), "start", []byte(`{"project":"a","entry":"b"}`), e.sys))
	if strings.Contains(out, "busy") {
		t.Fatalf("busy after the upgrade: %s", out)
	}
}

// Notes: result.json and the record are read with a 64 KiB cap.
func TestUpgradeFilesSizeCap(t *testing.T) {
	e := newUpEnv(t, "")
	if err := os.WriteFile(InstallerRecordPath(e.root), []byte("version 0.2.15\n"+strings.Repeat("#", maxSmallFile)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := InstalledVersion(e.root); ok {
		t.Fatal("read an oversized record")
	}
}
