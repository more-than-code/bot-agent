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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestHealthConfigAndSSRF(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
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
		{ID: "ssrf", Service: "nginx", Protocol: "http", Port: live, Path: "@evil.example/", IntervalSec: 60, ExpectStatusMin: 200, ExpectStatusMax: 299},
	}
	var logBuf safeBuf
	var slept atomic.Bool
	pace := &Pace{}
	pace.Metrics.Store(int64(40 * time.Millisecond))
	pace.Health.Store(int64(25 * time.Millisecond))
	pace.Process.Store(int64(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(10000, 5000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Pace:          pace,
			Sleep: func(context.Context, time.Duration) error {
				slept.Store(true)
				return errors.New("stop")
			},
			Log: log.New(&logBuf, "", 0),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !waitFrames(t, srv, &slept, 4*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "hello") >= 1
	}) {
		t.Fatalf("hello %v %s", frameTypes(srv.Frames()), redactLog(logBuf.copy(), token, srv.Credential))
	}
	// The drop is logged while handling hello_ok, which is after hello is sent.
	dropDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(dropDeadline) && !strings.Contains(logBuf.String(), `drop health check id="ssrf"`) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), `drop health check id="ssrf"`) {
		t.Fatalf("ssrf check was not dropped: %s", redactLog(logBuf.copy(), token, srv.Credential))
	}
	if err := srv.SendConfig(protocol.Config{
		MetricsIntervalSec: 60,
		ProcessIntervalSec: 300,
		HealthChecks: []protocol.HealthCheck{
			{ID: "fresh", Service: "nginx", Protocol: "http", Port: live, Path: "/health", IntervalSec: 60, ExpectStatusMin: 200, ExpectStatusMax: 299},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "ssrf-live", HostID: srv.HostID, Kind: "probe_health",
		Service: protocol.Str("nginx"), ExpiresAt: protocol.FormatTime(time.Now().Add(time.Minute)),
		TimeoutSec: 15,
		Spec:       json.RawMessage(fmt.Sprintf(`{"protocol":"http","port":%d,"path":"@evil.example/","expect_status_min":200,"expect_status_max":299}`, live)),
	}); err != nil {
		t.Fatal(err)
	}
	if !waitFrames(t, srv, &slept, 4*time.Second, func(fr []fakeserver.Frame) bool {
		rej, _ := taskOutcomes(fr)
		return rej["ssrf-live"] == "bad_spec" && healthPortString(fr, "fresh")
	}) {
		t.Fatalf("frames %v log %s violations %v", frameTypes(srv.Frames()), redactLog(logBuf.copy(), token, srv.Credential), srv.Violations())
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

// safeBuf is a bytes.Buffer the agent goroutine and the test can share.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *safeBuf) copy() *bytes.Buffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out bytes.Buffer
	out.Write(s.b.Bytes())
	return &out
}

func TestHelloForbiddenStaysUp(t *testing.T) {
	assertBearerStaysUp(t, func(srv *fakeserver.Server) { srv.HelloError = "forbidden" })
}

func TestHelloHostMismatchStaysUp(t *testing.T) {
	assertBearerStaysUp(t, func(srv *fakeserver.Server) {
		srv.HelloHostID = "99999999-9999-4999-8999-999999999999"
	})
}

func TestRedirectIsNotFollowed(t *testing.T) {
	clearAgentEnv(t)
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer evil.Close()
	srv := fakeserver.Start(t)
	srv.RedirectTo = evil.URL
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
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
		lo := 800*time.Millisecond - time.Millisecond
		hi := 1200*time.Millisecond + time.Millisecond
		if d < lo || d > hi {
			t.Fatalf("backoff %s", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no backoff")
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target saw %d", hits.Load())
	}
	if srv.RedirectHits() < 1 {
		t.Fatal("fake server did not answer 302")
	}
	if strings.Contains(logBuf.String(), token) || strings.Contains(logBuf.String(), srv.Credential) {
		t.Fatal("log leaked a secret")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("did not return")
	}
}

func TestProcessSamplesOnTheirOwnInterval(t *testing.T) {
	srv, _, cancel, errCh, logBuf, token := startPaced(t, 2*time.Second, 50*time.Millisecond, 0)
	defer stopRun(t, cancel, errCh)
	deadline := time.Now().Add(1200 * time.Millisecond)
	var second map[string]bool
	for time.Now().Before(deadline) {
		batches := metricSets(srv.Frames())
		if len(batches) >= 2 {
			second = batches[1]
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if second == nil {
		t.Fatalf("no second batch %v log %s", frameTypes(srv.Frames()), redactLog(logBuf, token, srv.Credential))
	}
	if !second["process"] || second["mem"] {
		t.Fatalf("second batch %v", second)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestConfigUpdatePullsProcess(t *testing.T) {
	srv, pace, cancel, errCh, logBuf, token := startPaced(t, time.Hour, 5*time.Second, 0)
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "metrics") >= 1
	}) {
		t.Fatalf("startup %v %s", frameTypes(srv.Frames()), redactLog(logBuf, token, srv.Credential))
	}
	pace.Process.Store(int64(40 * time.Millisecond))
	if err := srv.SendConfig(protocol.Config{MetricsIntervalSec: 60, ProcessIntervalSec: 30}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(700 * time.Millisecond)
	var second map[string]bool
	for time.Now().Before(deadline) {
		batches := metricSets(srv.Frames())
		if len(batches) >= 2 {
			second = batches[1]
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if second == nil || !second["process"] || second["mem"] {
		t.Fatalf("second batch %v frames %v log %s", second, frameTypes(srv.Frames()), redactLog(logBuf, token, srv.Credential))
	}
}

func TestClockOffsetExpiresTask(t *testing.T) {
	clearAgentEnv(t)
	base := time.Now().Add(-2 * time.Minute).UTC()
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Now = func() time.Time { return base.Add(2 * time.Minute) }
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc := t.TempDir()
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
	var logBuf bytes.Buffer
	var slept atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc,
			EUID:          func() int { return 1000 },
			Now:           func() time.Time { return base },
			DiscoverEvery: time.Hour,
			Sleep: func(context.Context, time.Duration) error {
				slept.Store(true)
				return errors.New("stop")
			},
			Log: stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !waitFrames(t, srv, &slept, 4*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "hello") >= 1
	}) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "skew", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: protocol.FormatTime(base.Add(time.Minute)), TimeoutSec: 15,
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !waitFrames(t, srv, &slept, 4*time.Second, func(fr []fakeserver.Frame) bool {
		rej, _ := taskOutcomes(fr)
		return rej["skew"] == "expired"
	}) {
		rej, res := taskOutcomes(srv.Frames())
		t.Fatalf("reject %v result %v log %s", rej, res, redactLog(&logBuf, token, srv.Credential))
	}
}

func TestAckIsNotReplayed(t *testing.T) {
	srv, cancel, errCh := startReplayAgent(t, nil, nil)
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "acked", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: protocol.FormatTime(time.Now().Add(time.Minute)), TimeoutSec: 15,
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(srv.TaskAcks(), "acked") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !containsStr(srv.TaskAcks(), "acked") {
		t.Fatal("no ack")
	}
	time.Sleep(50 * time.Millisecond)
	srv.CloseConn()
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 2 }) {
		t.Fatalf("no reconnect %v", frameTypes(srv.Frames()))
	}
	time.Sleep(200 * time.Millisecond)
	if taskSeenAfterHello(srv.Frames(), 2, "acked") {
		t.Fatal("acked task was resent")
	}
}

func TestSpoolReplayKeepsOrder(t *testing.T) {
	srv, cancel, errCh := startReplayAgent(t, func(p *Pace) {
		p.Metrics.Store(int64(40 * time.Millisecond))
		p.Process.Store(int64(time.Hour))
	}, func(s *fakeserver.Server) { s.SetAckMetrics(false) })
	defer stopRun(t, cancel, errCh)
	var first []string
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		first = metricIDs(srv.Frames())
		if len(first) >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if len(first) < 2 {
		t.Fatalf("batches %v", first)
	}
	srv.CloseConn()
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool {
		return countType(fr, "hello") >= 2 && len(metricIDs(afterHello(fr, 2))) >= 2
	}) {
		t.Fatalf("replay %v", frameTypes(srv.Frames()))
	}
	got := metricIDs(afterHello(srv.Frames(), 2))
	if len(got) < 2 || got[0] != first[0] || got[1] != first[1] {
		t.Fatalf("order got %v want prefix %v", got, first[:2])
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestServicesAfterDiscoveryChange(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(1000, 1000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: 40 * time.Millisecond,
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	if countType(srv.Frames(), "services") != 0 {
		t.Fatal("services sent before the fixture changed")
	}
	addExtraListener(t, proc)
	if !srv.Wait(3*time.Second, func(fr []fakeserver.Frame) bool {
		for _, f := range fr {
			if f.Type == "services" && strings.Contains(string(f.Raw), `"name":"extra"`) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no services frame %v log %s", frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestServicesWhenOwnerChanges(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(1000, 1000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: 40 * time.Millisecond,
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&logBuf),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool {
		for _, f := range fr {
			if f.Type == "hello" && strings.Contains(string(f.Raw), `"user":"root"`) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("hello %v log %s", frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if countType(srv.Frames(), "services") != 0 {
		t.Fatal("services sent before the owner changed")
	}
	rewriteListenerUIDs(t, proc)
	if !srv.Wait(3*time.Second, func(fr []fakeserver.Frame) bool {
		for _, f := range fr {
			if f.Type == "services" && strings.Contains(string(f.Raw), `"user":"1000"`) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no services frame %v log %s", frameTypes(srv.Frames()), redactLog(&logBuf, token, srv.Credential))
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestTaskWaitsAndExpires(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hang" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
		case <-timer.C:
		}
	}))
	defer hold.Close()
	live := portFrom(t, hold.Listener.Addr().String())
	proc, sys, passwd := fixtureProc(t, live)
	envPath, _ := writeEnv(t, srv.URL, t.TempDir())
	var started atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(1000, 1000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			OnTaskStart:   func() { started.Add(1) },
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&bytes.Buffer{}),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	exp := protocol.FormatTime(time.Now().Add(10 * time.Minute))
	spec := json.RawMessage(fmt.Sprintf(`{"protocol":"http","port":%d,"path":"/hang","expect_status_min":200,"expect_status_max":299}`, live))
	for i := 0; i < maxRunningTasks; i++ {
		if err := srv.SendTask(protocol.Task{
			V: 1, Type: "task", TaskID: fmt.Sprintf("block-%d", i), HostID: srv.HostID,
			Kind: "probe_health", Service: protocol.Str("nginx"), ExpiresAt: exp, TimeoutSec: 2, Spec: spec,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() < maxRunningTasks && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if started.Load() < maxRunningTasks {
		t.Fatalf("started %d", started.Load())
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "waiter", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: protocol.FormatTime(time.Now().Add(300 * time.Millisecond)), TimeoutSec: 15,
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.Wait(5*time.Second, func(fr []fakeserver.Frame) bool {
		rej, _ := taskOutcomes(fr)
		return rej["waiter"] == "expired"
	}) {
		rej, res := taskOutcomes(srv.Frames())
		t.Fatalf("reject %v result %v started %d", rej, res, started.Load())
	}
	_, res := taskOutcomes(srv.Frames())
	if _, ok := res["waiter"]; ok {
		t.Fatal("waiter ran")
	}
}

func assertBearerStaysUp(t *testing.T, setup func(*fakeserver.Server)) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	setup(srv)
	envPath := writeCredEnv(t, srv, t.TempDir())
	var slept atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile:  envPath,
			ProcRoot: t.TempDir(),
			EUID:     func() int { return 1000 },
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
		t.Fatal("retried")
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

func startPaced(t *testing.T, metrics, process, health time.Duration) (*fakeserver.Server, *Pace, context.CancelFunc, <-chan error, *bytes.Buffer, string) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
	pace := &Pace{}
	if metrics > 0 {
		pace.Metrics.Store(int64(metrics))
	}
	if process > 0 {
		pace.Process.Store(int64(process))
	}
	if health > 0 {
		pace.Health.Store(int64(health))
	}
	var logBuf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(10000, 5000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Pace:          pace,
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&logBuf),
		})
	}()
	return srv, pace, cancel, errCh, &logBuf, token
}

func startReplayAgent(t *testing.T, tune func(*Pace), setup func(*fakeserver.Server)) (*fakeserver.Server, context.CancelFunc, <-chan error) {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	if setup != nil {
		setup(srv)
	}
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	proc, sys, passwd := fixtureProc(t, 9)
	envPath, _ := writeEnv(t, srv.URL, t.TempDir())
	pace := &Pace{}
	pace.Metrics.Store(int64(time.Hour))
	pace.Process.Store(int64(time.Hour))
	if tune != nil {
		tune(pace)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(1000, 1000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			Pace:          pace,
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&bytes.Buffer{}),
		})
	}()
	return srv, cancel, errCh
}

func writeCredEnv(t *testing.T, srv *fakeserver.Server, state string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	body := fmt.Sprintf("BOT_AGENT_SERVER_URL=%s\nBOT_AGENT_HOST_ID=%s\nBOT_AGENT_CREDENTIAL_ID=%s\nBOT_AGENT_CREDENTIAL=%s\nBOT_AGENT_INSECURE_DEV=1\nBOT_AGENT_STATE_DIR=%s\nBOT_AGENT_HOSTNAME=pi1\nBOT_AGENT_ALWAYS_ON=true\n",
		srv.URL, srv.HostID, srv.CredentialID, srv.Credential, state)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rewriteListenerUIDs(t *testing.T, proc string) {
	t.Helper()
	path := filepath.Join(proc, "net", "tcp")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := []byte("00000000     0        0 ")
	next := []byte("00000000  1000        0 ")
	if !bytes.Contains(b, old) {
		t.Fatalf("uid column not found")
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(b, old, next), 0o644); err != nil {
		t.Fatal(err)
	}
}

func addExtraListener(t *testing.T, proc string) {
	t.Helper()
	dir := filepath.Join(proc, "77")
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/system.slice/extra.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[888]", filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(proc, "net", "tcp"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "   2: 0100007F:2382 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 888 1 0000000000000000 100 0 0 10 0\n"); err != nil {
		t.Fatal(err)
	}
}

func healthPortString(fr []fakeserver.Frame, id string) bool {
	for _, f := range fr {
		if f.Type != "metrics" {
			continue
		}
		var m struct {
			Samples []struct {
				M    string                     `json:"m"`
				Tags map[string]json.RawMessage `json:"tags"`
			} `json:"samples"`
		}
		if json.Unmarshal(f.Raw, &m) != nil {
			continue
		}
		for _, s := range m.Samples {
			if s.M != "health" {
				continue
			}
			if unquoteRaw(s.Tags["check_id"]) != id {
				continue
			}
			port := bytes.TrimSpace(s.Tags["port"])
			return len(port) >= 2 && port[0] == '"'
		}
	}
	return false
}

func unquoteRaw(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func metricSets(fr []fakeserver.Frame) []map[string]bool {
	var out []map[string]bool
	for _, f := range fr {
		if f.Type == "metrics" {
			out = append(out, measurementSet(f.Raw))
		}
	}
	return out
}

func metricIDs(fr []fakeserver.Frame) []string {
	var out []string
	for _, f := range fr {
		if f.Type != "metrics" {
			continue
		}
		var m struct {
			BatchID string `json:"batch_id"`
		}
		if json.Unmarshal(f.Raw, &m) == nil && m.BatchID != "" {
			out = append(out, m.BatchID)
		}
	}
	return out
}

func afterHello(fr []fakeserver.Frame, n int) []fakeserver.Frame {
	seen := 0
	for i, f := range fr {
		if f.Type != "hello" {
			continue
		}
		seen++
		if seen == n {
			return fr[i+1:]
		}
	}
	return nil
}

func taskSeenAfterHello(fr []fakeserver.Frame, helloN int, id string) bool {
	for _, f := range afterHello(fr, helloN) {
		if f.Type != "task_result" && f.Type != "task_reject" {
			continue
		}
		var m struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(f.Raw, &m) == nil && m.TaskID == id {
			return true
		}
	}
	return false
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
