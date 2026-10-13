package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestE2E(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 60
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer httpSrv.Close()
	live := portFrom(t, httpSrv.Listener.Addr().String())
	proc, sys, passwd := fixtureProc(t, live)
	state := t.TempDir()
	envPath, token := writeEnv(t, srv.URL, state)
	srv.Config.HealthChecks = []protocol.HealthCheck{
		{ID: "not-open", Service: "nginx", Protocol: "http", Port: 1, Path: "/", IntervalSec: 60, ExpectStatusMin: 200, ExpectStatusMax: 299},
		{ID: "live", Service: "nginx", Protocol: "http", Port: live, Path: "/health", IntervalSec: 60, ExpectStatusMin: 200, ExpectStatusMax: 299},
	}
	var logBuf bytes.Buffer
	var slept atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS: func(path string) (uint64, uint64, error) {
				switch path {
				case "/", "/home", "/home/user/My Files":
					return 10000, 5000, nil
				default:
					return 0, 0, os.ErrNotExist
				}
			},
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Sleep: func(context.Context, time.Duration) error {
				slept.Store(true)
				return errors.New("stop")
			},
			Log: stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)

	if !waitFrames(t, srv, &slept, 8*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "hello") >= 1 && countType(fr, "metrics") >= 1
	}) {
		t.Fatalf("startup frames %v log %s", frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if slept.Load() {
		t.Fatalf("reconnected during startup: %s", redactLog(&logBuf, token, srv.Credential))
	}
	assertFirstBatch(t, srv.Frames())
	if srv.Pings() < 1 {
		t.Fatal("server saw no ping")
	}
	host := srv.HostID
	exp := protocol.FormatTime(time.Now().Add(10 * time.Minute))
	past := protocol.FormatTime(time.Now().Add(-time.Minute))
	tasks := []protocol.Task{
		{V: 1, Type: "task", TaskID: "wrong-host", HostID: "99999999-9999-4999-8999-999999999999", Kind: "host_snapshot", ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{}`)},
		{V: 1, Type: "task", TaskID: "expired", HostID: host, Kind: "host_snapshot", ExpiresAt: past, TimeoutSec: 15, Spec: json.RawMessage(`{}`)},
		{V: 1, Type: "task", TaskID: "bad-spec", HostID: host, Kind: "host_snapshot", ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{"extra":true}`)},
		{V: 1, Type: "task", TaskID: "unknown-service", HostID: host, Kind: "probe_health", Service: protocol.Str("missing"), ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{"protocol":"tcp","port":9,"path":null,"expect_status_min":200,"expect_status_max":299}`)},
		{V: 1, Type: "task", TaskID: "forbidden-port", HostID: host, Kind: "probe_health", Service: protocol.Str("nginx"), ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{"protocol":"tcp","port":22,"path":null,"expect_status_min":200,"expect_status_max":299}`)},
		{V: 1, Type: "task", TaskID: "probe-ok", HostID: host, Kind: "probe_health", Service: protocol.Str("nginx"), ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(fmt.Sprintf(`{"protocol":"http","port":%d,"path":"/health","expect_status_min":200,"expect_status_max":299}`, live))},
		{V: 1, Type: "task", TaskID: "probe-refused", HostID: host, Kind: "probe_health", Service: protocol.Str("nginx"), ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{"protocol":"tcp","port":9,"path":null,"expect_status_min":200,"expect_status_max":299}`)},
		{V: 1, Type: "task", TaskID: "snapshot", HostID: host, Kind: "host_snapshot", ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{}`)},
	}
	for _, task := range tasks {
		if err := srv.SendTask(task); err != nil {
			t.Fatal(err)
		}
	}
	wantReject := map[string]string{
		"wrong-host": "wrong_host", "expired": "expired", "bad-spec": "bad_spec",
		"unknown-service": "unknown_service", "forbidden-port": "forbidden",
	}
	if !waitFrames(t, srv, &slept, 8*time.Second, func(fr []fakeserver.Frame) bool {
		gotR, gotRes := taskOutcomes(fr)
		for id, code := range wantReject {
			if gotR[id] != code {
				return false
			}
		}
		ok, hasOK := gotRes["probe-ok"]
		ref, hasRef := gotRes["probe-refused"]
		snap, hasSnap := gotRes["snapshot"]
		return hasOK && ok == "success" && hasRef && ref == "connection_refused" && hasSnap && snap == "succeeded"
	}) {
		t.Fatalf("tasks %v log %s violations %v", frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential), srv.Violations())
	}
	if slept.Load() {
		t.Fatal("unexpected reconnect")
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
	if countType(srv.Frames(), "enroll") != 1 {
		t.Fatalf("enroll count %d", countType(srv.Frames(), "enroll"))
	}
	assertNginx(t, srv.Frames(), live)
	assertSnapshotRedacted(t, srv.Frames())
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "JOIN_TOKEN") || strings.Contains(string(body), token) {
		t.Fatal("join token still on disk")
	}
	if !strings.Contains(string(body), "BOT_AGENT_HOST_ID="+srv.HostID) {
		t.Fatal("host id not stored")
	}
	fi, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	waitSpoolEmpty(t, state)
	logText := logBuf.String()
	if !strings.Contains(logText, `drop health check id="not-open"`) {
		t.Fatalf("missing drop log: %s", redactLog(&logBuf, token, srv.Credential))
	}
	if strings.Contains(logText, `id="live"`) {
		t.Fatal("live check was dropped")
	}
	if strings.Contains(logText, token) || strings.Contains(logText, srv.Credential) || strings.Contains(logText, "hunter2") {
		t.Fatal("log leaked a secret")
	}
}

func TestE2EReplay(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.DropNextMetrics()
	srv.Heartbeat = 60
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	state := t.TempDir()
	envPath, token := writeEnv(t, srv.URL, state)
	var logBuf bytes.Buffer
	var sleeps atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        func(string) (uint64, uint64, error) { return 1000, 1000, nil },
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Sleep: func(context.Context, time.Duration) error {
				if sleeps.Add(1) > 3 {
					return errors.New("stop")
				}
				return nil
			},
			Log: stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)
	var replayed string
	if !waitFrames(t, srv, nil, 8*time.Second, func(fr []fakeserver.Frame) bool {
		counts := map[string]int{}
		for _, f := range fr {
			if f.Type != "metrics" {
				continue
			}
			var m struct {
				BatchID string `json:"batch_id"`
			}
			if json.Unmarshal(f.Raw, &m) == nil && m.BatchID != "" {
				counts[m.BatchID]++
				if counts[m.BatchID] >= 2 {
					replayed = m.BatchID
				}
			}
		}
		return replayed != "" && srv.Conns() >= 3
	}) {
		t.Fatalf("replay conns=%d frames=%v log=%s", srv.Conns(), frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
	if strings.Contains(logBuf.String(), token) || strings.Contains(logBuf.String(), srv.Credential) {
		t.Fatal("log leaked a secret")
	}
}

func TestE2ETaskReplay(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.SkipNextTaskAck()
	srv.Heartbeat = 60
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	state := t.TempDir()
	envPath, token := writeEnv(t, srv.URL, state)
	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        func(string) (uint64, uint64, error) { return 1000, 1000, nil },
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !waitFrames(t, srv, nil, 8*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "hello") >= 1
	}) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	task := protocol.Task{
		V: 1, Type: "task", TaskID: "snap-replay", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: protocol.FormatTime(time.Now().Add(10 * time.Minute)), TimeoutSec: 15,
		Spec: json.RawMessage(`{}`),
	}
	deadline := time.Now().Add(8 * time.Second)
	var sent bool
	for time.Now().Before(deadline) {
		if !sent {
			if err := srv.SendTask(task); err == nil {
				sent = true
			}
		}
		n := 0
		for _, f := range srv.Frames() {
			if f.Type != "task_result" {
				continue
			}
			var head struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal(f.Raw, &head) == nil && head.TaskID == "snap-replay" {
				n++
			}
		}
		if n >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	n := 0
	for _, f := range srv.Frames() {
		if f.Type != "task_result" {
			continue
		}
		var head struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(f.Raw, &head) == nil && head.TaskID == "snap-replay" {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("task_result count %d frames %v log %s", n, frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestForbiddenStops(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.EnrollError = "forbidden"
	state := t.TempDir()
	envPath, _ := writeEnv(t, srv.URL, state)
	var slept atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath,
			EUID:    func() int { return 1000 },
			Sleep: func(context.Context, time.Duration) error {
				slept.Store(true)
				return errors.New("stop")
			},
			Log: stdLog(&bytes.Buffer{}),
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for srv.Conns() < 1 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	if srv.Conns() != 1 {
		t.Fatalf("conns %d", srv.Conns())
	}
	if slept.Load() {
		t.Fatal("forbidden retried")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not return")
	}
}

func TestUnauthenticatedBackoff(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.EnrollError = "unauthenticated"
	state := t.TempDir()
	envPath, token := writeEnv(t, srv.URL, state)
	var logBuf bytes.Buffer
	got := make(chan time.Duration, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath,
			EUID:    func() int { return 1000 },
			Sleep: func(_ context.Context, d time.Duration) error {
				got <- d
				return errors.New("stop")
			},
			Log: stdLog(&logBuf),
		})
	}()
	select {
	case d := <-got:
		lo := time.Duration(float64(30*time.Second)*0.8) - time.Millisecond
		hi := time.Duration(float64(30*time.Second)*1.2) + time.Millisecond
		if d < lo || d > hi {
			t.Fatalf("backoff %s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no backoff")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("did not return")
	}
	if strings.Contains(logBuf.String(), token) || strings.Contains(logBuf.String(), srv.Credential) {
		t.Fatal("log leaked a secret")
	}
	if !strings.Contains(logBuf.String(), `error_code="unauthenticated"`) {
		t.Fatalf("log %s", redactLog(&logBuf, token, srv.Credential))
	}
}

func TestRefusesRootAndLooseEnv(t *testing.T) {
	if err := Run(context.Background(), Options{EUID: func() int { return 0 }}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("%v", err)
	}
	clearAgentEnv(t)
	path := filepath.Join(t.TempDir(), "env")
	body := "BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\nBOT_AGENT_JOIN_TOKEN=join-token-test-value\nBOT_AGENT_STATE_DIR=/var/lib/bot-agent\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{EnvFile: path, EUID: func() int { return 1000 }})
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("%v", err)
	}
}

func fixtureProc(t *testing.T, live int) (proc, sys, passwd string) {
	t.Helper()
	base := t.TempDir()
	proc = filepath.Join(base, "proc")
	copyTree(t, filepath.Join(callerDir(), "..", "collect", "testdata", "proc"), proc)
	sys = filepath.Join(callerDir(), "..", "collect", "testdata", "sys")
	passwd = filepath.Join(callerDir(), "..", "collect", "testdata", "passwd")
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	fmt.Fprintf(&b, "   0: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0000000000000000 100 0 0 10 0\n", live)
	b.WriteString("   1: 00000000:0009 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 777 1 0000000000000000 100 0 0 10 0\n")
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fd := filepath.Join(proc, "42", "fd")
	if err := os.MkdirAll(fd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[111]", filepath.Join(fd, "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[777]", filepath.Join(fd, "4")); err != nil {
		t.Fatal(err)
	}
	return proc, sys, passwd
}

func writeEnv(t *testing.T, server, state string) (path, token string) {
	t.Helper()
	token = "join-token-test-value"
	path = filepath.Join(t.TempDir(), "env")
	body := fmt.Sprintf("BOT_AGENT_SERVER_URL=%s\nBOT_AGENT_JOIN_TOKEN=%s\nBOT_AGENT_INSECURE_DEV=1\nBOT_AGENT_STATE_DIR=%s\nBOT_AGENT_HOSTNAME=pi1\nBOT_AGENT_ALWAYS_ON=true\n", server, token, state)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, token
}

func waitFrames(t *testing.T, srv *fakeserver.Server, slept *atomic.Bool, d time.Duration, pred func([]fakeserver.Frame) bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if slept != nil && slept.Load() {
			return false
		}
		if pred(srv.Frames()) {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return pred(srv.Frames())
}

func stopRun(t *testing.T, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Error("run did not stop")
	}
}

func countType(fr []fakeserver.Frame, typ string) int {
	n := 0
	for _, f := range fr {
		if f.Type == typ {
			n++
		}
	}
	return n
}

func frameTypes(fr []fakeserver.Frame) []string {
	out := make([]string, len(fr))
	for i, f := range fr {
		out[i] = f.Type
	}
	return out
}

func taskOutcomes(fr []fakeserver.Frame) (reject map[string]string, results map[string]string) {
	reject = map[string]string{}
	results = map[string]string{}
	for _, f := range fr {
		switch f.Type {
		case "task_reject":
			var m struct {
				TaskID    string `json:"task_id"`
				ErrorCode string `json:"error_code"`
			}
			if json.Unmarshal(f.Raw, &m) == nil {
				reject[m.TaskID] = m.ErrorCode
			}
		case "task_result":
			var m struct {
				TaskID  string `json:"task_id"`
				State   string `json:"state"`
				Payload struct {
					Result string `json:"result"`
				} `json:"payload"`
			}
			if json.Unmarshal(f.Raw, &m) == nil {
				if m.Payload.Result != "" {
					results[m.TaskID] = m.Payload.Result
				} else {
					results[m.TaskID] = m.State
				}
			}
		}
	}
	return reject, results
}

func assertNginx(t *testing.T, fr []fakeserver.Frame, live int) {
	t.Helper()
	for _, f := range fr {
		if f.Type != "hello" {
			continue
		}
		var hello protocol.Hello
		if err := json.Unmarshal(f.Raw, &hello); err != nil {
			t.Fatal(err)
		}
		for _, s := range hello.Services {
			if s.Name != "nginx" {
				continue
			}
			hasLive, has9 := false, false
			for _, p := range s.Ports {
				if p == live {
					hasLive = true
				}
				if p == 9 {
					has9 = true
				}
			}
			if !hasLive || !has9 || s.Kind != "systemd" {
				t.Fatalf("nginx %+v", s)
			}
			return
		}
	}
	t.Fatal("hello had no nginx")
}

func assertFirstBatch(t *testing.T, fr []fakeserver.Frame) {
	t.Helper()
	for _, f := range fr {
		if f.Type != "metrics" {
			continue
		}
		has := measurementSet(f.Raw)
		if !has["mem"] || !has["system"] {
			t.Fatalf("first batch missing mem or system: %v", has)
		}
		return
	}
	t.Fatal("no metrics")
}

func measurementSet(raw []byte) map[string]bool {
	var m struct {
		Samples []struct {
			M string `json:"m"`
		} `json:"samples"`
	}
	_ = json.Unmarshal(raw, &m)
	has := map[string]bool{}
	for _, s := range m.Samples {
		has[s.M] = true
	}
	return has
}

func assertSnapshotRedacted(t *testing.T, fr []fakeserver.Frame) {
	t.Helper()
	for _, f := range fr {
		if f.Type != "task_result" {
			continue
		}
		var m struct {
			TaskID  string          `json:"task_id"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(f.Raw, &m) != nil || m.TaskID != "snapshot" {
			continue
		}
		if strings.Contains(string(m.Payload), "hunter2") {
			t.Fatal("snapshot cmdline leaked")
		}
		if !strings.Contains(string(m.Payload), "[REDACTED]") {
			t.Fatal("snapshot cmdline was not redacted")
		}
		return
	}
	t.Fatal("no snapshot")
}

func waitSpoolEmpty(t *testing.T, state string) {
	t.Helper()
	dir := filepath.Join(state, "spool")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(matches) == 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("spool not drained")
}

func redactLog(buf *bytes.Buffer, secrets ...string) string {
	s := buf.String()
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[REDACTED]")
		}
	}
	return s
}

func portFrom(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := splitPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func splitPort(addr string) (string, int, error) {
	host, portStr, err := hostPort(addr)
	if err != nil {
		return "", 0, err
	}
	var port int
	_, err = fmt.Sscanf(portStr, "%d", &port)
	return host, port, err
}

func hostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New(addr)
	}
	return addr[:i], addr[i+1:], nil
}

func callerDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

func clearAgentEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BOT_AGENT_SERVER_URL", "BOT_AGENT_JOIN_TOKEN", "BOT_AGENT_HOST_ID",
		"BOT_AGENT_CREDENTIAL_ID", "BOT_AGENT_CREDENTIAL", "BOT_AGENT_HOSTNAME",
		"BOT_AGENT_ALWAYS_ON", "BOT_AGENT_INSECURE_DEV", "BOT_AGENT_STATE_DIR",
		"BOT_AGENT_ENV_FILE",
	} {
		t.Setenv(k, "")
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func stdLog(buf *bytes.Buffer) *log.Logger {
	return log.New(buf, "", 0)
}
