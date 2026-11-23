package run

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/tasks"
)

var hfFiles = []policy.HostFile{
	{Name: "dd-policy", Path: "/etc/dd-worker/policy.toml", Dir: "dd-worker", File: "policy.toml", Mode: 0o644,
		Unit: "dd-worker.service", Check: []string{"/usr/local/bin/dd-worker"}, Health: []string{"/usr/local/bin/dd-worker", "health"}},
	{Name: "dd-secrets", Path: "/etc/dd-worker/secrets.env", Dir: "dd-worker", File: "secrets.env", Mode: 0o600, Secret: true,
		Unit: "dd-worker.service", Check: []string{"/usr/local/bin/dd-worker"}},
}

func hfAgent(t *testing.T, files []policy.HostFile, fn func(string, []byte) ([]byte, error)) (*agent, *fakeHelper) {
	t.Helper()
	h := &fakeHelper{fn: fn}
	return &agent{
		cfg:    config.Config{HostID: agentHost},
		policy: policy.Policy{Exists: true, Valid: true, HostFiles: files},
		opt:    Options{Helper: h, Log: stdLog(&bytes.Buffer{}), Now: time.Now},
	}, h
}

func hfSpec(content string, extra string) []byte {
	return []byte(`{"name":"dd-policy","content_b64":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"` + extra + `}`)
}

// v1.18 item 3: hello.programs.host_files from the policy, no commands.
func TestHelloHostFiles(t *testing.T) {
	a, _ := hfAgent(t, hfFiles, nil)
	p, caps := a.helloPrograms()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `"host_files":[{"name":"dd-policy","path":"/etc/dd-worker/policy.toml","secret":false,"unit":"dd-worker.service","health":true},` +
		`{"name":"dd-secrets","path":"/etc/dd-worker/secrets.env","secret":true,"unit":"dd-worker.service","health":false}],"agent_upgrade":false,"console_policy":false}`
	if !strings.HasSuffix(string(b), want) || strings.Contains(string(b), "/usr/local/bin") {
		t.Fatalf("%s", b)
	}
	hello := protocol.Hello{
		V: protocol.Version, Type: "hello", HostID: "h", CredentialID: "c", AgentVersion: protocol.AgentVersion,
		OS: "linux", Arch: "arm64", Capabilities: caps, Programs: p,
		Services: []protocol.Discovered{}, ServicesComplete: true,
	}
	raw, _ := json.Marshal(hello)
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatal(err)
	}
	// An invalid policy sends [].
	a.policy = policy.Policy{Exists: true, Reason: "parse"}
	p, _ = a.helloPrograms()
	if p.HostFiles == nil || len(p.HostFiles) != 0 {
		t.Fatalf("%+v", p.HostFiles)
	}
}

// v1.18 item 4: refused forbidden without host files; spec checks first.
func TestHostFileTaskRejects(t *testing.T) {
	a, h := hfAgent(t, nil, nil)
	rej, _, ok := a.execute(context.Background(), imageTask(hostfiles.Kind, hfSpec("x", ""), 420))
	if ok || rej.ErrorCode != "forbidden" {
		t.Fatalf("%v %q", ok, rej.ErrorCode)
	}
	a, h = hfAgent(t, hfFiles, nil)
	svc := "web"
	for _, tc := range []struct {
		spec    string
		service *string
	}{
		{`{"name":"dd-policy","content_b64":""}`, nil},
		{`{"name":"dd-policy","content_b64":"eA==","settle_sec":20}`, nil},
		{`{"name":"dd-policy","content_b64":"eA==","x":1}`, nil},
		{`{"name":"Bad","content_b64":"eA=="}`, nil},
		{string(hfSpec("x", "")), &svc},
	} {
		task := imageTask(hostfiles.Kind, []byte(tc.spec), 420)
		task.Service = tc.service
		rej, _, ok := a.execute(context.Background(), task)
		if ok || rej.ErrorCode != "bad_spec" {
			t.Fatalf("%s: %v %q", tc.spec, ok, rej.ErrorCode)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatal(h.Calls())
	}
	if code, _ := tasks.Decide(imageTask(hostfiles.Kind, hfSpec("x", ""), 420), agentHost, time.Now(), nil, tasks.Gate{}); code != "forbidden" {
		t.Fatalf("zero gate: %q", code)
	}
}

func hfResult(t *testing.T, a *agent, spec []byte) protocol.TaskResult {
	t.Helper()
	rej, res, ok := a.execute(context.Background(), imageTask(hostfiles.Kind, spec, 420))
	if !ok {
		t.Fatalf("rejected %s", rej.ErrorCode)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return res
}

// The task is forwarded byte for byte with the 420 s budget, and each
// helper reply maps onto the task result.
func TestHostFileTaskResults(t *testing.T) {
	// runHostFile zeroes the spec it was given, so each call gets a copy.
	spec := func() []byte { return hfSpec("repo = \"new\"\n", `,"force":true`) }
	var deadline time.Duration
	var body []byte
	reply := ""
	a, h := hfAgent(t, hfFiles, nil)
	h.fn = func(verb string, b []byte) ([]byte, error) {
		if verb != hostfiles.VerbApply {
			return nil, errors.New("verb")
		}
		body = append([]byte(nil), b...)
		return []byte(reply), nil
	}
	a.opt.Helper = hfDeadlineHelper{h, &deadline}
	sum := strings.Repeat("a", 64)
	for _, tc := range []struct {
		reply, state, code, payload string
	}{
		{`{"outcome":"succeeded","stage":"","reason":"","took_ms":12,"sha256":"` + sum + `"}`, "succeeded", "",
			`{"outcome":"succeeded","reason":"","sha256":"` + sum + `","stage":"","took_ms":12}`},
		{`{"outcome":"unchanged","stage":"","reason":"","took_ms":1}`, "succeeded", "",
			`{"outcome":"unchanged","reason":"","stage":"","took_ms":1}`},
		{`{"outcome":"check_failed","stage":"check","reason":"bad_repo","took_ms":5}`, "failed", "check_failed",
			`{"outcome":"check_failed","reason":"bad_repo","stage":"check","took_ms":5}`},
		{`{"outcome":"worker_busy","stage":"idle","reason":"","took_ms":5}`, "failed", "worker_busy", ""},
		{`{"outcome":"rolled_back","stage":"health","reason":"not_connected","took_ms":5}`, "failed", "rolled_back", ""},
		{`{"outcome":"rollback_failed","stage":"rollback","reason":"not_connected","took_ms":5}`, "failed", "rollback_failed", ""},
		{`{"outcome":"forbidden_spec","stage":"target","reason":"","took_ms":0}`, "failed", "forbidden_spec", ""},
		{`{"ok":false,"reason":"io_error"}`, "failed", "io_error", ""},
		// Anything outside the shape is helper_unavailable, never relayed.
		{`{"outcome":"check_failed","stage":"check","reason":"Error: repo secret=x","took_ms":5}`, "failed", "helper_unavailable", ""},
		{`{"outcome":"succeeded","stage":"","reason":"","took_ms":1,"stderr":"x"}`, "failed", "helper_unavailable", ""},
		{`{"outcome":"weird","stage":"","reason":"","took_ms":1}`, "failed", "helper_unavailable", ""},
	} {
		reply = tc.reply
		res := hfResult(t, a, spec())
		if res.State != tc.state || (tc.code == "" && res.ErrorCode != nil) || (tc.code != "" && (res.ErrorCode == nil || *res.ErrorCode != tc.code)) {
			t.Fatalf("%s: %+v", tc.reply, res)
		}
		if tc.payload != "" && payloadJSON(t, res) != tc.payload {
			t.Fatalf("%s: payload %s", tc.reply, payloadJSON(t, res))
		}
		// payload.outcome is only ever one of the contract's seven; the four
		// error codes carry none.
		outcome, has := res.Payload["outcome"]
		switch tc.code {
		case "io_error", "helper_unavailable", "bad_spec", "policy_denied":
			if has {
				t.Fatalf("%s: outcome %v in payload", tc.reply, outcome)
			}
		default:
			if !has || !hfContractOutcome(outcome) {
				t.Fatalf("%s: outcome %v", tc.reply, outcome)
			}
		}
		if strings.Contains(payloadJSON(t, res), "secret=x") {
			t.Fatal("relayed helper text")
		}
	}
	if string(body) != string(spec()) {
		t.Fatalf("body %s", body)
	}
	if deadline < hostfiles.TaskTimeout-5*time.Second {
		t.Fatalf("budget %v", deadline)
	}
	// Transport failure.
	h.fn = func(string, []byte) ([]byte, error) { return nil, errHelper }
	if res := hfResult(t, a, spec()); res.State != "failed" || *res.ErrorCode != "helper_unavailable" || res.Payload["outcome"] != nil {
		t.Fatalf("%+v", res)
	}
	// Helper refusals policy_denied and bad_spec: error code, no outcome.
	for _, code := range []string{"policy_denied", "bad_spec"} {
		h.fn = func(string, []byte) ([]byte, error) { return []byte(`{"ok":false,"reason":"` + code + `"}`), nil }
		res := hfResult(t, a, spec())
		if _, has := res.Payload["outcome"]; res.State != "failed" || *res.ErrorCode != code || has {
			t.Fatalf("%s: %+v", code, res)
		}
	}
	// Every apply wakes the status sender.
	select {
	case <-a.hostFilesWake.wake():
	default:
		t.Fatal("no wake after apply")
	}
}

func hfContractOutcome(v any) bool {
	switch v {
	case hostfiles.OutcomeSucceeded, hostfiles.OutcomeUnchanged, hostfiles.OutcomeWorkerBusy,
		hostfiles.OutcomeCheckFailed, hostfiles.OutcomeRolledBack, hostfiles.OutcomeRollbackFailed,
		hostfiles.OutcomeForbiddenSpec:
		return true
	}
	return false
}

type hfDeadlineHelper struct {
	h   *fakeHelper
	out *time.Duration
}

func (d hfDeadlineHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		*d.out = time.Until(dl)
	}
	return d.h.Call(ctx, verb, body)
}

// Status from the helper becomes the frame; a bad reply sends nothing.
func TestHostFilesStatusFrame(t *testing.T) {
	reply := `{"files":[{"name":"dd-policy","exists":true,"owner_ok":true,"matches_applied":null,"size":3,"sha256":"` + strings.Repeat("b", 64) + `"},` +
		`{"name":"dd-secrets","exists":true,"owner_ok":true,"matches_applied":false}]}`
	a, h := hfAgent(t, hfFiles, func(verb string, b []byte) ([]byte, error) {
		if verb != hostfiles.VerbStatus || string(b) != `{}` {
			return nil, errors.New("verb")
		}
		return []byte(reply), nil
	})
	f, ok := a.hostFilesStatus(context.Background())
	if !ok {
		t.Fatal("no frame")
	}
	raw, _ := json.Marshal(f)
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	want := `{"v":1,"type":"host_files_status","files":[{"name":"dd-policy","exists":true,"owner_ok":true,"matches_applied":null,"size":3,"sha256":"` + strings.Repeat("b", 64) + `"},{"name":"dd-secrets","exists":true,"owner_ok":true,"matches_applied":false}]}`
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
	for _, bad := range []string{
		`{"files":[{"name":"x","exists":true,"owner_ok":true,"matches_applied":null,"content":"s3cret"}]}`,
		`{"files":[{"name":"x","exists":true,"owner_ok":false,"reason":"/etc/shadow","matches_applied":null}]}`,
		`{"ok":false,"reason":"policy_denied"}`,
		`not json`,
	} {
		reply = bad
		if _, ok := a.hostFilesStatus(context.Background()); ok {
			t.Fatalf("accepted %s", bad)
		}
	}
	// No host files: no helper call.
	a, h = hfAgent(t, nil, nil)
	if _, ok := a.hostFilesStatus(context.Background()); ok || len(h.Calls()) != 0 {
		t.Fatal("called without host files")
	}
}

// hfFS: every path is a root-owned dir except the dd-worker binary.
type hfFS struct{}

type hfFI struct{ mode os.FileMode }

func (i hfFI) Name() string       { return "x" }
func (i hfFI) Size() int64        { return 0 }
func (i hfFI) Mode() os.FileMode  { return i.mode }
func (i hfFI) ModTime() time.Time { return time.Time{} }
func (i hfFI) IsDir() bool        { return i.mode.IsDir() }
func (i hfFI) Sys() any           { return &syscall.Stat_t{} }

func (hfFS) Lstat(name string) (os.FileInfo, error) {
	if name == "/usr/local/bin/dd-worker" {
		return hfFI{mode: 0o755}, nil
	}
	return hfFI{mode: os.ModeDir | 0o755}, nil
}
func (hfFS) Readlink(string) (string, error) { return "", fs.ErrInvalid }

// v1.18 item 5 end to end against the fake server: a frame after hello,
// after an apply, and on the period. Every frame passes the validator.
func TestHostFilesStatusSession(t *testing.T) {
	srv, _, stop := startHostFilesRun(t, time.Hour)
	defer stop()
	if !srv.WaitType(hostfiles.FrameStatus, 1, 5*time.Second) {
		t.Fatalf("no status after hello: %v", frameTypes(srv.Frames()))
	}
	if err := srv.SendTask(protocol.Task{
		V: protocol.Version, Type: "task", TaskID: "t-hf", HostID: srv.HostID, Kind: hostfiles.Kind,
		ExpiresAt: protocol.FormatTime(time.Now().Add(time.Hour).UTC()), TimeoutSec: 420,
		Spec: hfSpec("x", ""),
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.WaitType(hostfiles.FrameStatus, 2, 5*time.Second) {
		t.Fatalf("no status after apply: %v", frameTypes(srv.Frames()))
	}
	types := frameTypes(srv.Frames())
	var sawResult, sawHello bool
	for _, ty := range types {
		switch ty {
		case "hello":
			sawHello = true
		case "task_result":
			sawResult = true
		}
	}
	if !sawHello || !sawResult {
		t.Fatalf("%v", types)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

// The periodic frame (10 minutes in production).
func TestHostFilesStatusPeriodic(t *testing.T) {
	srv, _, stop := startHostFilesRun(t, 100*time.Millisecond)
	defer stop()
	if !srv.WaitType(hostfiles.FrameStatus, 4, 10*time.Second) {
		t.Fatalf("no periodic status: %v", frameTypes(srv.Frames()))
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func startHostFilesRun(t *testing.T, every time.Duration) (*fakeserver.Server, *fakeHelper, func()) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	proc, sys, passwd := fixtureProc(t, 1)
	pw, err := os.ReadFile(passwd)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	passwd2 := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd2, append(pw, []byte("dd-worker:x:1500:1500::/:/bin/false\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(group, []byte("root:x:0:\ndd-worker:x:1500:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(pol, []byte(`host_files = ["dd-policy=/etc/dd-worker/policy.toml"]
host_file_modes = ["dd-policy:0644"]
host_file_units = ["dd-policy:dd-worker.service"]
host_file_run_as = ["dd-policy:dd-worker"]
host_file_checks = ["dd-policy:/usr/local/bin/dd-worker policy check --file -"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath, _ := writeEnv(t, srv.URL, t.TempDir())
	status := `{"files":[{"name":"dd-policy","exists":true,"owner_ok":true,"matches_applied":true,"size":3,"sha256":"` + strings.Repeat("c", 64) + `"}]}`
	h := &fakeHelper{fn: func(verb string, b []byte) ([]byte, error) {
		switch verb {
		case hostfiles.VerbStatus:
			return []byte(status), nil
		case hostfiles.VerbApply:
			return []byte(`{"outcome":"succeeded","stage":"","reason":"","took_ms":3,"sha256":"` + strings.Repeat("c", 64) + `"}`), nil
		}
		return nil, errors.New("verb")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd2, GroupPath: group,
			StatFS: statFS(1000, 1000), EUID: func() int { return 1000 },
			Sleep:      func(context.Context, time.Duration) error { return nil },
			Log:        stdLog(&bytes.Buffer{}),
			PolicyPath: pol, PolicyUID: func(os.FileInfo) (uint32, bool) { return 0, true },
			PolicyExec: hfFS{}, Helper: h,
			HostFilesEvery: every,
		})
	}()
	return srv, h, func() { stopRun(t, cancel, errCh) }
}

// C4: the spec bytes are zeroed when the task is done.
func TestHostFileSpecCleared(t *testing.T) {
	a, _ := hfAgent(t, hfFiles, func(string, []byte) ([]byte, error) {
		return []byte(`{"outcome":"unchanged","stage":"","reason":"","took_ms":1}`), nil
	})
	task := imageTask(hostfiles.Kind, hfSpec("DD_TOKEN=s3cret\n", ""), 0)
	if _, _, ok := a.execute(context.Background(), task); !ok {
		t.Fatal("rejected")
	}
	if bytes.Contains(task.Spec, []byte("czNjcmV0")) || strings.Trim(string(task.Spec), "\x00") != "" {
		t.Fatalf("spec not cleared: %q", task.Spec)
	}
}

// C7: the agent drops size and sha256 of a secret file itself.
func TestHostFileAgentDropsSecretHashes(t *testing.T) {
	sum := strings.Repeat("d", 64)
	a, _ := hfAgent(t, hfFiles, func(verb string, _ []byte) ([]byte, error) {
		if verb == hostfiles.VerbStatus {
			return []byte(`{"files":[{"name":"dd-policy","exists":true,"owner_ok":true,"matches_applied":null,"size":3,"sha256":"` + sum + `"},` +
				`{"name":"dd-secrets","exists":true,"owner_ok":true,"matches_applied":true,"size":9,"sha256":"` + sum + `"}]}`), nil
		}
		return []byte(`{"outcome":"succeeded","stage":"","reason":"","took_ms":1,"sha256":"` + sum + `"}`), nil
	})
	f, ok := a.hostFilesStatus(context.Background())
	if !ok || f.Files[0].SHA256 != sum || f.Files[0].Size == nil || f.Files[1].SHA256 != "" || f.Files[1].Size != nil {
		t.Fatalf("%+v", f)
	}
	secret := []byte(`{"name":"dd-secrets","content_b64":"eA=="}`)
	res := hfResult(t, a, secret)
	if _, has := res.Payload["sha256"]; has || res.State != "succeeded" {
		t.Fatalf("%+v", res)
	}
	if res := hfResult(t, a, hfSpec("x", "")); res.Payload["sha256"] != sum {
		t.Fatalf("plain lost its hash: %+v", res)
	}
}

// T6: the 600 s budget is fixed (a larger timeout_sec is capped) and an
// expired budget is timed_out.
func TestHostFileBudgetAndTimeout(t *testing.T) {
	var deadline time.Duration
	h := &fakeHelper{fn: func(string, []byte) ([]byte, error) {
		return []byte(`{"outcome":"unchanged","stage":"","reason":"","took_ms":1}`), nil
	}}
	a, _ := hfAgent(t, hfFiles, nil)
	a.opt.Helper = hfDeadlineHelper{h, &deadline}
	for _, sec := range []int{0, 30, 900} {
		if _, _, ok := a.execute(context.Background(), imageTask(hostfiles.Kind, hfSpec("x", ""), sec)); !ok {
			t.Fatal("rejected")
		}
		if deadline > hostfiles.TaskTimeout || deadline < hostfiles.TaskTimeout-5*time.Second {
			t.Fatalf("timeout_sec %d: budget %v", sec, deadline)
		}
	}
	a.opt.Helper = blockingHelper{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, res, ok := a.execute(ctx, imageTask(hostfiles.Kind, hfSpec("x", ""), 0))
	if !ok || res.State != "timed_out" {
		t.Fatalf("%+v", res)
	}
}

type blockingHelper struct{}

func (blockingHelper) Call(ctx context.Context, _ string, _ []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// T6: a 64 KiB apply goes through the real socket client in one frame.
func TestHostFileSocketHelperFrame(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hfsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReaderSize(c, 1<<20).ReadBytes('\n')
		got <- line
		_, _ = c.Write([]byte(`{"outcome":"unchanged","stage":"","reason":"","took_ms":1}` + "\n"))
	}()
	content := strings.Repeat("k", hostfiles.MaxContent)
	spec := hfSpec(content, "")
	out, err := (socketHelper{path: sock}).Call(context.Background(), hostfiles.VerbApply, spec)
	if err != nil || !strings.Contains(string(out), "unchanged") {
		t.Fatalf("%s %v", out, err)
	}
	line := <-got
	var env struct {
		Verb    string          `json:"verb"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &env); err != nil || env.Verb != hostfiles.VerbApply || string(env.Request) != string(spec) {
		t.Fatalf("frame %d bytes: %v", len(line), err)
	}
	if len(line) > hostfiles.MaxFrame {
		t.Fatalf("frame %d over the verb cap", len(line))
	}
}
