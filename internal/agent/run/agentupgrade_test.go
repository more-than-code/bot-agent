package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func upgradeTask(id, spec string, timeout int) protocol.Task {
	return protocol.Task{
		V: 1, Type: "task", TaskID: id, HostID: "h1", Kind: agentupgrade.Kind,
		ExpiresAt: protocol.FormatTime(time.Now().Add(10 * time.Minute)), TimeoutSec: timeout,
		Spec: json.RawMessage(spec),
	}
}

func upgradeAgent(on bool, h Helper) *agent {
	a := &agent{opt: Options{Helper: h, Log: stdLog(&bytes.Buffer{}), Now: time.Now}}
	a.cfg.HostID = "h1"
	a.policy = policy.Policy{Exists: true, Valid: true, AgentUpgrade: on}
	return a
}

// v1.20 item 4: an agent whose hello had agent_upgrade false refuses the
// task as forbidden and never asks the helper.
func TestUpgradeTaskForbiddenWhenHelloFalse(t *testing.T) {
	h := &fakeHelper{fn: func(string, []byte) ([]byte, error) { return nil, errors.New("called") }}
	for _, pol := range []policy.Policy{
		{},
		{Exists: true, Valid: true},
		{Exists: true, Valid: false, AgentUpgrade: true},
	} {
		a := upgradeAgent(false, h)
		a.policy = pol
		rej, _, ok := a.execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, 120))
		if ok || rej.ErrorCode != "forbidden" {
			t.Fatalf("%+v: ok %v %+v", pol, ok, rej)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatalf("helper called %v", h.Calls())
	}
}

func TestUpgradeTaskBadSpec(t *testing.T) {
	h := &fakeHelper{}
	a := upgradeAgent(true, h)
	for _, spec := range []string{`{}`, `{"version":"0.2"}`, `{"version":"0.2.16","url":"https://x"}`} {
		rej, _, ok := a.execute(context.Background(), upgradeTask("t1", spec, 120))
		if ok || rej.ErrorCode != "bad_spec" {
			t.Fatalf("%s: ok %v %+v", spec, ok, rej)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatal("helper called")
	}
}

func TestUpgradeTaskAccepted(t *testing.T) {
	var body []byte
	h := &fakeHelper{fn: func(verb string, b []byte) ([]byte, error) {
		if verb != agentupgrade.VerbStart {
			return nil, errors.New(verb)
		}
		body = append([]byte(nil), b...)
		return []byte(`{"ok":true,"state":"accepted"}`), nil
	}}
	a := upgradeAgent(true, h)
	a.opt.UpgradePollFor = time.Millisecond
	_, res, ok := a.execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, 120))
	if !ok || res.State != "succeeded" || res.ErrorCode != nil || res.Payload["state"] != "accepted" || len(res.Payload) != 1 {
		t.Fatalf("%+v", res)
	}
	if string(body) != `{"version":"0.2.16"}` {
		t.Fatalf("helper body %s", body)
	}
	raw, err := protocol.Marshal(res)
	if err != nil || protocol.ValidateAgent(raw) != nil {
		t.Fatalf("result does not validate: %s %v", raw, protocol.ValidateAgent(raw))
	}
}

func TestUpgradeTaskHelperRefusals(t *testing.T) {
	for reason, want := range map[string]string{
		"policy_denied": "policy_denied", "busy": "busy", "not_newer": "not_newer", "bad_spec": "bad_spec",
		"io_error": "helper_unavailable", "invalid_verb": "helper_unavailable",
		// FixedReason collapses a code it does not know to policy_denied.
		"something/else": "policy_denied",
	} {
		h := &fakeHelper{fn: func(string, []byte) ([]byte, error) {
			return []byte(`{"ok":false,"reason":"` + reason + `"}`), nil
		}}
		_, res, ok := upgradeAgent(true, h).execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, 120))
		if !ok || res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != want {
			t.Fatalf("%s: %+v", reason, res)
		}
	}
	h := &fakeHelper{fn: func(string, []byte) ([]byte, error) { return nil, errors.New("down") }}
	_, res, _ := upgradeAgent(true, h).execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, 120))
	if res.State != "failed" || *res.ErrorCode != "helper_unavailable" {
		t.Fatalf("transport: %+v", res)
	}
	h = &fakeHelper{fn: func(string, []byte) ([]byte, error) { return []byte(`{"ok":true,"state":"started"}`), nil }}
	_, res, _ = upgradeAgent(true, h).execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, 120))
	if res.State != "failed" || *res.ErrorCode != "helper_unavailable" {
		t.Fatalf("odd reply: %+v", res)
	}
}

type deadlineCapture struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *deadlineCapture) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	dl, ok := ctx.Deadline()
	c.mu.Lock()
	if ok {
		c.d = time.Until(dl)
	}
	c.mu.Unlock()
	return []byte(`{"ok":false,"reason":"busy"}`), nil
}

// The task runs with 120 s, not the generic 60 s clamp.
func TestUpgradeTaskTimeout(t *testing.T) {
	for _, tc := range []struct {
		sec      int
		min, max time.Duration
	}{
		{120, 110 * time.Second, 120 * time.Second},
		{0, 110 * time.Second, 120 * time.Second},
		{600, 110 * time.Second, 120 * time.Second},
		{30, 20 * time.Second, 30 * time.Second},
	} {
		c := &deadlineCapture{}
		upgradeAgent(true, c).execute(context.Background(), upgradeTask("t1", `{"version":"0.2.16"}`, tc.sec))
		c.mu.Lock()
		d := c.d
		c.mu.Unlock()
		if d < tc.min || d > tc.max {
			t.Fatalf("timeout_sec %d: deadline in %v", tc.sec, d)
		}
	}
}

// upgradeHelper serves the two v1.20 verbs from a mutable result.
type upgradeHelper struct {
	mu      sync.Mutex
	result  *agentupgrade.Result
	running bool
	calls   []string
}

func (h *upgradeHelper) setRunning(v bool) {
	h.mu.Lock()
	h.running = v
	h.mu.Unlock()
}

func (h *upgradeHelper) count(verb string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == verb {
			n++
		}
	}
	return n
}

func (h *upgradeHelper) set(r *agentupgrade.Result) {
	h.mu.Lock()
	h.result = r
	h.mu.Unlock()
}

func (h *upgradeHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, verb)
	switch verb {
	case agentupgrade.VerbResult:
		return agentupgrade.Encode(agentupgrade.ResultReply{OK: true, Result: h.result, Running: h.running}), nil
	case agentupgrade.VerbStart:
		return []byte(`{"ok":true,"state":"accepted"}`), nil
	default:
		return nil, errors.New(verb)
	}
}

func upgradeFrames(fr []fakeserver.Frame) []agentupgrade.Result {
	var out []agentupgrade.Result
	for _, f := range fr {
		if f.Type != agentupgrade.Frame {
			continue
		}
		var m protocol.AgentUpgradeResult
		if json.Unmarshal(f.Raw, &m) == nil {
			out = append(out, m.Result)
		}
	}
	return out
}

// Hello carries programs.agent_upgrade and last_upgrade (read at start),
// an acknowledged hello writes the marker, an accepted task starts the
// poll, and a new result is sent once as agent_upgrade_result.
func TestUpgradeEndToEnd(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	proc, sys, passwd := fixtureProc(t, 9)
	state := t.TempDir()
	polPath := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(polPath, []byte("agent_upgrade = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := agentupgrade.Result{Version: "0.2.15", Outcome: "succeeded", At: "2026-11-23T09:00:00Z"}
	h := &upgradeHelper{result: &first}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: writeCredEnv(t, srv, state), ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS: statFS(1000, 1000), EUID: func() int { return 1000 },
			DiscoverEvery: time.Hour, Sleep: func(context.Context, time.Duration) error { return nil },
			Log: stdLog(&bytes.Buffer{}), PolicyPath: polPath, PolicyUID: rootUID, GroupPath: joeGroup(t),
			Helper: h, UpgradePollEvery: 20 * time.Millisecond, UpgradePollFor: 10 * time.Second,
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "ping") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	var hello protocol.Hello
	for _, f := range srv.Frames() {
		if f.Type == "hello" {
			if err := json.Unmarshal(f.Raw, &hello); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if hello.Programs == nil || !hello.Programs.AgentUpgrade || hello.LastUpgrade == nil || *hello.LastUpgrade != first {
		t.Fatalf("hello %+v last %+v", hello.Programs, hello.LastUpgrade)
	}
	fi, err := os.Lstat(filepath.Join(state, "last_hello"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker %v %v", fi, err)
	}
	// The result read at start went out in hello, so no frame for it.
	if n := len(upgradeFrames(srv.Frames())); n != 0 {
		t.Fatalf("frame for the hello result: %d", n)
	}

	task := upgradeTask("up-1", `{"version":"0.2.16"}`, 120)
	task.HostID = srv.HostID
	if err := srv.SendTask(task); err != nil {
		t.Fatal(err)
	}
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "task_result") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	_, results := taskOutcomes(srv.Frames())
	if results["up-1"] != "succeeded" {
		t.Fatalf("results %v", results)
	}
	// The upgrade failed before the agent was replaced: the poll sees a
	// new result and sends it once.
	second := agentupgrade.Result{Version: "0.2.16", Outcome: "failed", Reason: "signature_invalid", At: "2026-11-23T10:00:00Z"}
	h.set(&second)
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return len(upgradeFrames(fr)) >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	time.Sleep(200 * time.Millisecond)
	got := upgradeFrames(srv.Frames())
	if len(got) != 1 || got[0] != second {
		t.Fatalf("frames %+v", got)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func startUpgradeRun(t *testing.T, h Helper, every, window time.Duration) (*fakeserver.Server, string, func()) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	proc, sys, passwd := fixtureProc(t, 9)
	state := t.TempDir()
	polPath := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(polPath, []byte("agent_upgrade = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: writeCredEnv(t, srv, state), ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS: statFS(1000, 1000), EUID: func() int { return 1000 },
			DiscoverEvery: time.Hour, Sleep: func(context.Context, time.Duration) error { return nil },
			Log: stdLog(&bytes.Buffer{}), PolicyPath: polPath, PolicyUID: rootUID, GroupPath: joeGroup(t),
			Helper: h, UpgradePollEvery: every, UpgradePollFor: window,
		})
	}()
	return srv, state, func() { stopRun(t, cancel, errCh) }
}

// H1: the result file is written after the new agent started, with no
// task accepted in this process. The start-up poll finds it and sends it
// once, then stops when no upgrade runs.
func TestUpgradeLateResultAfterStart(t *testing.T) {
	h := &upgradeHelper{running: true}
	srv, _, stop := startUpgradeRun(t, h, 20*time.Millisecond, 10*time.Second)
	defer stop()
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "ping") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	var hello protocol.Hello
	for _, f := range srv.Frames() {
		if f.Type == "hello" {
			_ = json.Unmarshal(f.Raw, &hello)
			break
		}
	}
	if hello.LastUpgrade != nil {
		t.Fatalf("hello last_upgrade %+v", hello.LastUpgrade)
	}
	late := agentupgrade.Result{Version: "0.2.16", Outcome: "succeeded", At: "2026-11-23T10:00:00Z"}
	h.set(&late)
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return len(upgradeFrames(fr)) >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	// Still running: the poll goes on; once the unit is done it stops.
	h.setRunning(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := h.count(agentupgrade.VerbResult)
		time.Sleep(200 * time.Millisecond)
		if h.count(agentupgrade.VerbResult) == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll did not stop")
		}
	}
	got := upgradeFrames(srv.Frames())
	if len(got) != 1 || got[0] != late {
		t.Fatalf("frames %+v", got)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

// A result equal to the one already known is not sent again.
func TestNoteUpgradeResultOnlyNew(t *testing.T) {
	a := upgradeAgent(true, nil)
	r := agentupgrade.Result{Version: "0.2.16", Outcome: "failed", Reason: "download_failed", At: "2026-11-23T10:00:00Z"}
	if !a.noteUpgradeResult(&r) {
		t.Fatal("first result not new")
	}
	if a.noteUpgradeResult(&r) || a.noteUpgradeResult(nil) {
		t.Fatal("same result counted twice")
	}
	_, seq := a.upgradeSnapshot()
	a.markUpgradeSent(seq)
	r.At = "2026-11-23T10:05:00Z"
	if !a.noteUpgradeResult(&r) {
		t.Fatal("changed result not new")
	}
}

// A host that did not opt in never asks the helper for a result.
func TestUpgradeResultNotReadWithoutOptIn(t *testing.T) {
	h := &upgradeHelper{}
	a := upgradeAgent(false, h)
	if a.readUpgradeResult(context.Background()) != nil || len(h.calls) != 0 {
		t.Fatalf("calls %v", h.calls)
	}
}
