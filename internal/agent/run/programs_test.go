package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestProgramStartSuccessUsesSockets(t *testing.T) {
	a, h, clock, spec := programAgent(t, 20)
	h.fn = programScript(t, "active", "running", []int{1}, []int{3031}, nil)
	res := mustRun(t, a, "program_start", spec)
	if res.State != "succeeded" || res.ErrorCode != nil {
		t.Fatalf("%+v", res)
	}
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateProgramStatus(raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-env-value") || strings.Contains(string(raw), `"ports":[1]`) {
		t.Fatalf("payload %s", raw)
	}
	ports := res.Payload["ports"].([]int)
	if len(ports) != 1 || ports[0] != 3031 {
		t.Fatalf("ports %+v", res.Payload["ports"])
	}
	if clock.sleeps() != 0 {
		t.Fatalf("slept %d", clock.sleeps())
	}
	if !h.saw("status") || !h.saw("sockets") {
		t.Fatalf("calls %v", h.Calls())
	}
}

func TestProgramStartFailedImmediately(t *testing.T) {
	a, h, clock, spec := programAgent(t, 20)
	long := strings.Repeat("a", 3000)
	h.fn = programScript(t, "failed", "exit-code", []int{1}, []int{3031}, []string{"password=hunter2", long})
	res := mustRun(t, a, "program_start", spec)
	if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "start_failed" {
		t.Fatalf("%+v", res)
	}
	if clock.sleeps() != 0 {
		t.Fatalf("slept %d", clock.sleeps())
	}
	lines, _ := res.Payload["lines"].([]string)
	if len(lines) != 2 || lines[0] != "password=[REDACTED]" {
		t.Fatalf("lines %+v", res.Payload["lines"])
	}
	if len([]rune(lines[1])) != 2048 {
		t.Fatalf("line %d", len([]rune(lines[1])))
	}
	raw := marshalPayload(t, res.Payload)
	if strings.Contains(string(raw), "super-secret-env-value") || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("payload %s", raw)
	}
	ports := res.Payload["ports"].([]int)
	if len(ports) != 1 || ports[0] != 3031 {
		t.Fatalf("ports %+v", ports)
	}
}

func TestProgramStartInactiveAtTenSeconds(t *testing.T) {
	a, h, clock, spec := programAgent(t, 20)
	h.fn = programScript(t, "inactive", "dead", nil, nil, []string{"down"})
	start := clock.now()
	res := mustRun(t, a, "program_start", spec)
	if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "start_failed" {
		t.Fatalf("%+v", res)
	}
	if clock.now().Sub(start) < 10*time.Second || clock.sleeps() == 0 {
		t.Fatalf("elapsed %s sleeps %d", clock.now().Sub(start), clock.sleeps())
	}
	assertPoll(t, clock)
}

func TestProgramActivatingSucceedsAtTenSeconds(t *testing.T) {
	a, h, clock, _ := programAgent(t, 20)
	h.fn = programScript(t, "activating", "start", nil, []int{3031}, nil)
	start := clock.now()
	res := mustRun(t, a, "program_restart", refSpec())
	if res.State != "succeeded" || res.ErrorCode != nil {
		t.Fatalf("%+v", res)
	}
	if clock.now().Sub(start) < 10*time.Second || clock.sleeps() == 0 {
		t.Fatalf("elapsed %s sleeps %d", clock.now().Sub(start), clock.sleeps())
	}
	assertPoll(t, clock)
	if !h.saw("restart") || !h.saw("status") {
		t.Fatalf("calls %v", h.Calls())
	}
}

func TestProgramStartStaleActivationIsStartFailed(t *testing.T) {
	a, h, clock, spec := programAgent(t, 20)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case "start":
			return programs.EncodeStart(programs.Status{
				Unit: "deploybot-tour-agent-tra-backend.service", ActiveState: "active", SubState: "running", Ports: []int{},
			}, false), nil
		case "status":
			t.Errorf("polled status after a stale start")
			return statusBody("active", "running", nil), nil
		default:
			return nil, errors.New(verb)
		}
	}
	res := mustRun(t, a, "program_start", spec)
	if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "start_failed" {
		t.Fatalf("%+v", res)
	}
	if clock.sleeps() != 0 {
		t.Fatalf("slept %d", clock.sleeps())
	}
	if res.Payload["active_state"] != "active" {
		t.Fatalf("payload %+v", res.Payload)
	}
}

func TestProgramBecomesActiveBeforeTenSeconds(t *testing.T) {
	a, h, clock, spec := programAgent(t, 20)
	var statusN int
	h.fn = func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case "start":
			return statusBody("inactive", "dead", nil), nil
		case "status":
			statusN++
			if statusN < 3 {
				return statusBody("activating", "start", []int{1}), nil
			}
			return statusBody("active", "running", []int{1}), nil
		case "sockets":
			return programs.EncodeSockets([]int{3031}, protocol.Str("joe")), nil
		default:
			return nil, errors.New("verb")
		}
	}
	res := mustRun(t, a, "program_start", spec)
	if res.State != "succeeded" {
		t.Fatalf("%+v", res)
	}
	if clock.sleeps() < 2 || clock.sleeps() > 20 {
		t.Fatalf("sleeps %d", clock.sleeps())
	}
}

func TestProgramWaitCancelIsTimedOut(t *testing.T) {
	a, _, _, spec := programAgent(t, 20)
	a.opt.Sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	a.opt.Helper.(*fakeHelper).fn = programScript(t, "activating", "start", nil, nil, nil)
	res := mustRun(t, a, "program_start", spec)
	if res.State != "timed_out" || res.ErrorCode == nil || *res.ErrorCode != "timed_out" || len(res.Payload) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestProgramHelperRefusalPayload(t *testing.T) {
	a, h, _, spec := programAgent(t, 20)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList(nil), nil
		}
		return programs.EncodeRefusal("not_created"), nil
	}
	res := mustRun(t, a, "program_start", spec)
	assertReason(t, res, "not_created")

	h.calls = nil
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList(nil), nil
		}
		return programs.EncodeRefusal("/tmp/super-secret-env-value"), nil
	}
	res = mustRun(t, a, "program_start", spec)
	assertReason(t, res, programs.ReasonPolicy)
	if strings.Contains(string(marshalResult(t, res)), "super-secret-env-value") {
		t.Fatal("leaked helper reason")
	}

	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList(nil), nil
		}
		return nil, errors.New("read /tmp/super-secret-env-value")
	}
	res = mustRun(t, a, "program_start", spec)
	assertReason(t, res, programs.ReasonHelper)
	if strings.Contains(string(marshalResult(t, res)), "super-secret-env-value") {
		t.Fatal("leaked helper error")
	}
}

func TestProgramStopKeepsEmptyPortsWhenSocketsFail(t *testing.T) {
	a, h, _, _ := programAgent(t, 20)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "stop":
			return statusBody("inactive", "dead", []int{1}), nil
		case "sockets":
			return programs.EncodeRefusal(programs.ReasonNotOwned), nil
		default:
			return nil, errors.New(verb)
		}
	}
	res := mustRun(t, a, "program_stop", refSpec())
	if res.State != "succeeded" {
		t.Fatalf("%+v", res)
	}
	ports := res.Payload["ports"].([]int)
	if len(ports) != 0 {
		t.Fatalf("ports %+v", ports)
	}
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateProgramStatus(raw); err != nil {
		t.Fatal(err)
	}
}

func TestProgramLogsRedactsAgain(t *testing.T) {
	a, h, _, _ := programAgent(t, 20)
	long := strings.Repeat("b", 3000)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb != "logs" {
			return nil, errors.New(verb)
		}
		return programs.EncodeLogs(programs.LogBatch{Lines: []string{"password=hunter2", long}, Truncated: false}), nil
	}
	spec := []byte(`{"project":"tour-agent","entry":"tra-backend","lines":10}`)
	res := mustRun(t, a, "program_logs", spec)
	if res.State != "succeeded" || res.Payload["truncated"] != true {
		t.Fatalf("%+v", res)
	}
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateProgramLogs(raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("%s", raw)
	}
}

func TestProgramListFailureFailsClosed(t *testing.T) {
	a, h, _, spec := programAgent(t, 20)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb != "list" {
			t.Errorf("verb %s", verb)
		}
		return nil, errors.New("down")
	}
	rej, _, ok := a.execute(context.Background(), programTask("program_start", spec))
	if ok || rej.ErrorCode != "policy_denied" {
		t.Fatalf("ok %v %+v", ok, rej)
	}
	if calls := h.Calls(); len(calls) != 1 || calls[0] != "list" {
		t.Fatalf("calls %v", calls)
	}
}

func TestProgramDisabledDoesNotCallHelper(t *testing.T) {
	a, h, _, spec := programAgent(t, 20)
	a.policy.Enabled = false
	h.fn = func(string, []byte) ([]byte, error) {
		t.Error("helper called")
		return nil, errors.New("called")
	}
	rej, _, ok := a.execute(context.Background(), programTask("program_start", spec))
	if ok || rej.ErrorCode != "policy_denied" || len(h.Calls()) != 0 {
		t.Fatalf("ok %v code %s calls %v", ok, rej.ErrorCode, h.Calls())
	}
	task := programTask("program_start", spec)
	task.Service = protocol.Str("web")
	rej, _, ok = a.execute(context.Background(), task)
	if ok || rej.ErrorCode != "bad_spec" || len(h.Calls()) != 0 {
		t.Fatalf("service ok %v code %s", ok, rej.ErrorCode)
	}
}

func TestInactiveUnitFileCountsAgainstMax(t *testing.T) {
	a, h, _, spec := programAgent(t, 1)
	h.fn = func(verb string, _ []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList([]programs.UnitInfo{{
				Unit: "deploybot-tour-agent-old.service", Project: "tour-agent", Entry: "old", ActiveState: "inactive",
			}}), nil
		}
		if verb == "start" {
			t.Error("start called")
		}
		return nil, errors.New(verb)
	}
	rej, _, ok := a.execute(context.Background(), programTask("program_start", spec))
	if ok || rej.ErrorCode != "policy_denied" {
		t.Fatalf("ok %v %+v", ok, rej)
	}
}

func TestProgramAlreadyActiveDoesNotCountAgainstMax(t *testing.T) {
	a, h, _, spec := programAgent(t, 1)
	unit := "deploybot-tour-agent-tra-backend.service"
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList([]programs.UnitInfo{{
				Unit: unit, Project: "tour-agent", Entry: "tra-backend", ActiveState: "active",
			}}), nil
		}
		if verb == "start" {
			return statusBody("active", "running", nil), nil
		}
		if verb == "status" {
			return statusBody("active", "running", nil), nil
		}
		if verb == "sockets" {
			return programs.EncodeSockets([]int{}, nil), nil
		}
		return nil, errors.New(verb)
	}
	res := mustRun(t, a, "program_start", spec)
	if res.State != "succeeded" {
		t.Fatalf("%+v", res)
	}
	other := bytes.Replace(spec, []byte("tra-backend"), []byte("other"), 1)
	h.calls = nil
	rej, _, ok := a.execute(context.Background(), programTask("program_start", other))
	if ok || rej.ErrorCode != "policy_denied" {
		t.Fatalf("ok %v %+v", ok, rej)
	}
	for _, c := range h.Calls() {
		if c == "start" {
			t.Fatal("started a second unit at max")
		}
	}
}

func TestHelloProgramsAbsentDisabledAndEnabled(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	appendUnmapped(t, proc)
	passwd := joePasswd(t)

	absentHelper := &fakeHelper{fn: func(string, []byte) ([]byte, error) {
		t.Error("helper called")
		return nil, errors.New("called")
	}}
	hello, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd,
		PolicyPath: filepath.Join(t.TempDir(), "missing.toml"),
		Helper:     absentHelper,
	})
	if hello.Programs != nil || strings.Contains(string(raw), `"programs"`) {
		t.Fatalf("absent hello has programs: %s", raw)
	}
	if len(hello.Capabilities) != 2 || len(absentHelper.Calls()) != 0 {
		t.Fatalf("caps %v calls %v", hello.Capabilities, absentHelper.Calls())
	}
	if !strings.Contains(string(raw), "port-3031") {
		t.Fatal("scan listener missing")
	}

	disabledPath := writeRunner(t, false, true)
	disabledHelper := &fakeHelper{fn: func(string, []byte) ([]byte, error) {
		t.Error("helper called")
		return nil, errors.New("called")
	}}
	hello, raw = helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: disabledPath, PolicyUID: rootUID,
		Helper: disabledHelper,
	})
	if hello.Programs == nil || hello.Programs.Enabled || hello.Programs.AllowShell != true {
		t.Fatalf("%+v", hello.Programs)
	}
	if len(hello.Programs.RunAs) != 1 || hello.Programs.RunAs[0] != "joe" || len(hello.Programs.PolicyHash) != 64 {
		t.Fatalf("%+v", hello.Programs)
	}
	if len(hello.Capabilities) != 2 {
		t.Fatalf("caps %v", hello.Capabilities)
	}
	if strings.Contains(string(raw), "SENTINEL") || len(disabledHelper.Calls()) != 0 {
		t.Fatalf("leaked or called: %s calls %v", raw, disabledHelper.Calls())
	}

	enabledHelper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList([]programs.UnitInfo{{
				Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active",
			}, {
				Unit: "deploybot-tour-agent-old.service", Project: "tour-agent", Entry: "old", ActiveState: "inactive",
			}}), nil
		case "sockets":
			return programs.EncodeSockets([]int{3031}, protocol.Str("joe")), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	hello, raw = helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: writeRunner(t, true, false), PolicyUID: rootUID,
		Helper: enabledHelper,
	})
	if hello.Programs == nil || !hello.Programs.Enabled || len(hello.Capabilities) != 3 || hello.Capabilities[2] != "programs" {
		t.Fatalf("caps %v programs %+v", hello.Capabilities, hello.Programs)
	}
	if strings.Contains(string(raw), "SENTINEL") || strings.Contains(string(raw), "port-3031") || strings.Contains(string(raw), `"entry":"old"`) {
		t.Fatalf("hello %s", raw)
	}
	var web *protocol.Discovered
	for i := range hello.Services {
		if hello.Services[i].Name == "web" {
			web = &hello.Services[i]
		}
	}
	if web == nil || web.Owner == nil || web.Owner.Kind != "program" || web.Owner.Project != "tour-agent" || web.Kind != "systemd" {
		t.Fatalf("%+v", web)
	}
	if len(web.Ports) != 1 || web.Ports[0] != 3031 || web.SystemdUnit == nil || *web.SystemdUnit != "deploybot-tour-agent-web.service" {
		t.Fatalf("%+v", web)
	}
	calls := enabledHelper.Calls()
	if len(calls) < 2 || calls[0] != "list" || calls[1] != "sockets" {
		t.Fatalf("calls %v", calls)
	}
}

func TestOwnedSocketsFailureStillReportsUnit(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Log: stdLog(&bytes.Buffer{}),
			Helper: &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
				if verb == "list" {
					return programs.EncodeList([]programs.UnitInfo{{
						Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active",
					}}), nil
				}
				return nil, errors.New("sockets down")
			}},
		},
		policy: policy.Policy{Exists: true, Valid: true, Enabled: true},
	}
	res := a.discover(context.Background())
	var web *protocol.Discovered
	for i := range res.Services {
		if res.Services[i].Name == "web" {
			web = &res.Services[i]
		}
	}
	if web == nil || web.Owner == nil || len(web.Ports) != 0 {
		t.Fatalf("%+v", web)
	}
}

func TestProgramDiscoveryFailureLogsOnce(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	var buf bytes.Buffer
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Log: stdLog(&buf),
			Helper: &fakeHelper{fn: func(string, []byte) ([]byte, error) { return nil, errors.New("down") }},
		},
		policy: policy.Policy{Exists: true, Valid: true, Enabled: true},
	}
	a.discover(context.Background())
	a.discover(context.Background())
	if n := strings.Count(buf.String(), "bot-agent: program discovery failed"); n != 1 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}
}

type fakeHelper struct {
	mu    sync.Mutex
	calls []string
	fn    func(string, []byte) ([]byte, error)
}

func (f *fakeHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, verb)
	fn := f.fn
	f.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if fn == nil {
		return nil, errors.New("helper")
	}
	return fn(verb, body)
}

func (f *fakeHelper) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHelper) saw(verb string) bool {
	for _, c := range f.Calls() {
		if c == verb {
			return true
		}
	}
	return false
}

type testClock struct {
	mu   sync.Mutex
	t    time.Time
	n    int
	durs []time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.n++
	c.durs = append(c.durs, d)
	c.mu.Unlock()
	return nil
}

func assertPoll(t *testing.T, c *testClock) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum time.Duration
	for _, d := range c.durs {
		if d >= time.Second {
			t.Fatalf("sleep %s waited out the task timeout", d)
		}
		sum += d
	}
	if sum != 10*time.Second {
		t.Fatalf("slept %s", sum)
	}
}

func (c *testClock) sleeps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func programAgent(t *testing.T, max int) (*agent, *fakeHelper, *testClock, []byte) {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	h := &fakeHelper{}
	pol := policy.Policy{
		Exists: true, Valid: true, Enabled: true, MaxPrograms: max,
		RunAs: []string{"joe"}, Dirs: []string{root}, ExecAllow: []string{"/usr/bin/pnpm"},
		Accounts: map[string]policy.Account{
			"joe": {Name: "joe", UID: 1000, GID: 1000, Home: "/home/joe", Group: "joe"},
		},
	}
	a := &agent{
		cfg:    config.Config{HostID: "11111111-1111-4111-8111-111111111111"},
		policy: pol,
		opt: Options{
			Now: clock.now, Sleep: clock.sleep, Helper: h, Log: stdLog(&bytes.Buffer{}),
		},
	}
	spec := []byte(fmt.Sprintf(`{"project":"tour-agent","entry":"tra-backend","dir":%q,"exec":"/usr/bin/pnpm","args":[],"shell":false,"run_as":"joe","env":{"PORT":"super-secret-env-value"},"restart":"on-failure"}`, app))
	return a, h, clock, spec
}

func refSpec() []byte {
	return []byte(`{"project":"tour-agent","entry":"tra-backend"}`)
}

func programTask(kind string, spec []byte) protocol.Task {
	return protocol.Task{
		V: protocol.Version, Type: "task", TaskID: "task-1",
		HostID: "11111111-1111-4111-8111-111111111111", Kind: kind,
		ExpiresAt:  protocol.FormatTime(time.Date(2026, 10, 15, 12, 5, 0, 0, time.UTC)),
		TimeoutSec: 30, Spec: spec,
	}
}

func mustRun(t *testing.T, a *agent, kind string, spec []byte) protocol.TaskResult {
	t.Helper()
	rej, res, ok := a.execute(context.Background(), programTask(kind, spec))
	if !ok {
		t.Fatalf("rejected %s", rej.ErrorCode)
	}
	return res
}

func programScript(t *testing.T, state, sub string, statusPorts, socketPorts []int, lines []string) func(string, []byte) ([]byte, error) {
	t.Helper()
	return func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case "start", "restart":
			return statusBody(state, sub, statusPorts), nil
		case "status":
			return statusBody(state, sub, statusPorts), nil
		case "sockets":
			if socketPorts == nil {
				socketPorts = []int{}
			}
			return programs.EncodeSockets(socketPorts, protocol.Str("joe")), nil
		case "logs":
			if lines == nil {
				lines = []string{}
			}
			return programs.EncodeLogs(programs.LogBatch{Lines: lines}), nil
		default:
			return nil, errors.New(verb)
		}
	}
}

func statusBody(state, sub string, ports []int) []byte {
	pid := 4242
	started := "2026-10-15T12:00:00Z"
	if ports == nil {
		ports = []int{}
	}
	return programs.EncodeStatus(programs.Status{
		Unit: "deploybot-tour-agent-tra-backend.service", ActiveState: state, SubState: sub,
		MainPID: &pid, StartedAt: &started, Ports: ports,
	})
}

func assertReason(t *testing.T, res protocol.TaskResult, reason string) {
	t.Helper()
	if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != programs.ReasonPolicy {
		t.Fatalf("%+v", res)
	}
	raw := marshalPayload(t, res.Payload)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m["reason"] != reason {
		t.Fatalf("payload %s", raw)
	}
}

func marshalPayload(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	b, err := protocol.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func marshalResult(t *testing.T, res protocol.TaskResult) []byte {
	t.Helper()
	b, err := protocol.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func helloFrame(t *testing.T, opt Options) (protocol.Hello, []byte) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	opt.EnvFile = writeCredEnv(t, srv, t.TempDir())
	opt.EUID = func() int { return 1000 }
	opt.DiscoverEvery = time.Hour
	opt.Sleep = func(context.Context, time.Duration) error { return nil }
	if opt.Log == nil {
		opt.Log = stdLog(&bytes.Buffer{})
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, opt) }()
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
		cancel()
		t.Fatal("no hello")
	}
	var hello protocol.Hello
	var raw []byte
	for _, f := range srv.Frames() {
		if f.Type == "hello" {
			raw = append([]byte(nil), f.Raw...)
			if err := json.Unmarshal(raw, &hello); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	stopRun(t, cancel, errCh)
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
	return hello, raw
}

func appendUnmapped(t *testing.T, proc string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(proc, "net", "tcp"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "   3: 00000000:0BD7 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 555 1 0000000000000000 100 0 0 10 0\n"); err != nil {
		t.Fatal(err)
	}
}

func joePasswd(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "passwd")
	body := "root:x:0:0:root:/root:/bin/bash\nwww-data:x:33:33:www-data:/var/www:/usr/sbin/nologin\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func joeGroup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "group")
	if err := os.WriteFile(path, []byte("root:x:0:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func rootUID(os.FileInfo) (uint32, bool) { return 0, true }

func writeRunner(t *testing.T, enabled, shell bool) string {
	t.Helper()
	flag, sh := "false", "false"
	if enabled {
		flag = "true"
	}
	if shell {
		sh = "true"
	}
	body := fmt.Sprintf("# disabled example\nenabled = %s\nrun_as = [\"joe\"]\ndirs = [\"/home/joe/workspace-SENTINEL-dir\"]\nexec_allow = [\"/usr/bin/pnpm-SENTINEL\"]\nallow_shell = %s\nmax_programs = 20\n", flag, sh)
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
