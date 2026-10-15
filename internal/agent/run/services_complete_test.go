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
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/discover"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// startTuned runs one credentialed session on the fake clock. tune may
// replace the policy, the helper, and the server config. It runs before the
// agent dials. Discovery reads proc on every pass.
func startTuned(t *testing.T, proc, sys, passwd string, tune func(*fakeserver.Server, *Options)) *refreshRun {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	envPath := writeCredEnv(t, srv, t.TempDir())
	pace := &Pace{}
	pace.Metrics.Store(int64(24 * time.Hour))
	pace.Process.Store(int64(24 * time.Hour))
	r := &refreshRun{
		srv:      srv,
		clk:      newFakeClock(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)),
		proc:     proc,
		token:    srv.Credential,
		discover: 60 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	r.cancel = cancel
	r.errCh = errCh
	opt := Options{
		EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
		StatFS:               statFS(1000, 1000),
		EUID:                 func() int { return 1000 },
		Now:                  r.clk.Now,
		After:                r.clk.After,
		DiscoverEvery:        60 * time.Second,
		ServicesRefreshEvery: 300 * time.Second,
		Pace:                 pace,
		Sleep:                func(context.Context, time.Duration) error { return nil },
		Log:                  stdLog(&r.log),
		PolicyPath:           filepath.Join(t.TempDir(), "missing.toml"),
	}
	if tune != nil {
		tune(srv, &opt)
	}
	go func() { errCh <- Run(ctx, opt) }()
	return r
}

func helloOf(t *testing.T, r *refreshRun) (protocol.Hello, []byte) {
	t.Helper()
	for _, f := range r.srv.Frames() {
		if f.Type != "hello" {
			continue
		}
		var hello protocol.Hello
		if err := json.Unmarshal(f.Raw, &hello); err != nil {
			t.Fatal(err)
		}
		return hello, append([]byte(nil), f.Raw...)
	}
	t.Fatal("no hello")
	return protocol.Hello{}, nil
}

func hasService(list []protocol.Discovered, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

// breakScan makes discover.Scan fail without removing the saved table.
func breakScan(t *testing.T, proc string) {
	t.Helper()
	tcp := filepath.Join(proc, "net", "tcp")
	if err := os.Rename(tcp, tcp+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tcp, 0o755); err != nil {
		t.Fatal(err)
	}
}

func restoreScan(t *testing.T, proc string) {
	t.Helper()
	tcp := filepath.Join(proc, "net", "tcp")
	if err := os.Remove(tcp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tcp+".held", tcp); err != nil {
		t.Fatal(err)
	}
}

// clearListeners leaves no tcp table, which is a successful empty scan.
func clearListeners(t *testing.T, proc string) {
	t.Helper()
	net := filepath.Join(proc, "net")
	entries, err := os.ReadDir(net)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(net, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanFailureSkipsFrameThenSendsFullSet(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	r := startTuned(t, proc, sys, passwd, nil)
	defer stopRun(t, r.cancel, r.errCh)

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if !hello.ServicesComplete || !strings.Contains(string(raw), `"services_complete":true`) {
		t.Fatalf("hello %s", raw)
	}
	if !hasService(hello.Services, "nginx") {
		t.Fatalf("hello services %+v", hello.Services)
	}

	breakScan(t, proc)
	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)
	if !strings.Contains(r.log.String(), "bot-agent: discovery failed") {
		t.Fatalf("log %s", redactLog(&r.log, r.token))
	}

	restoreScan(t, proc)
	addExtraListener(t, proc)
	r.fire(t, 120*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 || !hasService(sets[0], "nginx") || !hasService(sets[0], "extra") {
		t.Fatalf("services %+v", sets)
	}
	got, err := discover.Scan(proc, passwd)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(sets[0]) != canonical(got.Services) {
		t.Fatalf("frame is not the full scan")
	}
	r.done(t)
}

func TestIncompleteHelloDoesNotCountAsSent(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	breakScan(t, proc)
	r := startTuned(t, proc, sys, passwd, nil)
	defer stopRun(t, r.cancel, r.errCh)

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if hello.ServicesComplete || len(hello.Services) != 0 {
		t.Fatalf("hello %+v", hello)
	}
	if !strings.Contains(string(raw), `"services_complete":false`) || !strings.Contains(string(raw), `"services":[]`) {
		t.Fatalf("hello %s", raw)
	}
	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)

	clearListeners(t, proc)
	r.fire(t, 120*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 || len(sets[0]) != 0 {
		t.Fatalf("services %+v", sets)
	}
	r.done(t)
}

func TestHelperFailureSkipsFrameThenSendsOwned(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	appendUnmapped(t, proc)
	passwd := joePasswd(t)
	var down atomic.Bool
	down.Store(true)
	helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if down.Load() {
			return nil, errors.New("down")
		}
		switch verb {
		case "list":
			return programs.EncodeList([]programs.UnitInfo{{
				Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active",
			}}), nil
		case "sockets":
			return programs.EncodeSockets([]int{3031}, protocol.Str("joe")), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	r := startTuned(t, proc, sys, passwd, func(_ *fakeserver.Server, opt *Options) {
		opt.PolicyPath = writeRunner(t, true, false)
		opt.PolicyUID = rootUID
		opt.GroupPath = joeGroup(t)
		opt.Helper = helper
	})
	defer stopRun(t, r.cancel, r.errCh)

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if hello.ServicesComplete || len(hello.Services) != 0 {
		t.Fatalf("hello %s", raw)
	}
	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)
	if !strings.Contains(r.log.String(), "bot-agent: program discovery failed") {
		t.Fatalf("log %s", redactLog(&r.log, r.token))
	}

	down.Store(false)
	r.fire(t, 120*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 || !hasService(sets[0], "web") || !hasService(sets[0], "nginx") {
		t.Fatalf("services %+v", sets)
	}
	if hasService(sets[0], "port-3031") {
		t.Fatal("owned port also reported as discovered")
	}
	var web *protocol.Discovered
	for i := range sets[0] {
		if sets[0][i].Name == "web" {
			web = &sets[0][i]
		}
	}
	if web == nil || web.Owner == nil || web.Owner.Kind != "program" || web.Owner.Entry != "web" {
		t.Fatalf("%+v", web)
	}
	r.done(t)
}

func TestPolicyDisabledStillSendsServices(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	passwd := joePasswd(t)
	helper := &fakeHelper{fn: func(string, []byte) ([]byte, error) {
		t.Error("helper called")
		return nil, errors.New("called")
	}}
	r := startTuned(t, proc, sys, passwd, func(_ *fakeserver.Server, opt *Options) {
		opt.PolicyPath = writeRunner(t, false, false)
		opt.PolicyUID = rootUID
		opt.GroupPath = joeGroup(t)
		opt.Helper = helper
	})
	defer stopRun(t, r.cancel, r.errCh)

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if !hello.ServicesComplete || !hasService(hello.Services, "nginx") {
		t.Fatalf("hello %s", raw)
	}
	if len(helper.Calls()) != 0 {
		t.Fatalf("calls %v", helper.Calls())
	}
	for _, sec := range []time.Duration{60 * time.Second, 120 * time.Second, 180 * time.Second, 240 * time.Second} {
		r.fire(t, sec)
		r.wantServices(t, 0)
	}
	r.fire(t, 300*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 || canonical(sets[0]) != canonical(hello.Services) {
		t.Fatalf("refresh %+v hello %+v", sets, hello.Services)
	}
	if len(helper.Calls()) != 0 {
		t.Fatalf("calls %v", helper.Calls())
	}
	r.done(t)
}

func TestIncompleteDiscoveryKeepsPreviousSet(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	var buf bytes.Buffer
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Log: stdLog(&buf),
			ServicesRefreshEvery: 300 * time.Second,
		},
	}
	res, complete := a.discover(context.Background())
	if !complete || a.serviceFor(42, "") != "nginx" {
		t.Fatalf("complete %v service %q", complete, a.serviceFor(42, ""))
	}
	a.lastCanon = canonical(res.Services)
	a.lastServicesSent = start
	held := canonical(a.services)

	breakScan(t, proc)
	sent := false
	w := &writer{onSend: func(any) error {
		sent = true
		return errors.New("sent")
	}}
	if err := a.maybeServices(context.Background(), w, start.Add(time.Minute)); err != nil || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	if a.lastCanon != held || !a.lastServicesSent.Equal(start) {
		t.Fatal("stamps moved")
	}
	if canonical(a.services) != held || a.serviceFor(42, "") != "nginx" {
		t.Fatal("in-memory set replaced")
	}
	if !strings.Contains(buf.String(), "bot-agent: discovery failed") {
		t.Fatalf("log %s", buf.String())
	}

	restoreScan(t, proc)
	var down atomic.Bool
	a.policy = policy.Policy{Exists: true, Valid: true, Enabled: true}
	a.opt.Helper = &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if down.Load() {
			return nil, errors.New("down")
		}
		switch verb {
		case "list":
			return programs.EncodeList([]programs.UnitInfo{{
				Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active",
			}}), nil
		case "sockets":
			return programs.EncodeSockets([]int{}, nil), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	again, complete := a.discover(context.Background())
	if !complete || !hasService(again.Services, "web") || !hasService(a.services, "web") {
		t.Fatalf("owned discovery %+v", again.Services)
	}
	owned := canonical(a.services)
	a.lastCanon = owned
	a.lastServicesSent = start
	down.Store(true)
	sent = false
	buf.Reset()
	if err := a.maybeServices(context.Background(), w, start.Add(2*time.Minute)); err != nil || sent {
		t.Fatalf("helper failure sent=%v err=%v", sent, err)
	}
	if a.lastCanon != owned || !a.lastServicesSent.Equal(start) {
		t.Fatal("stamps moved after helper failure")
	}
	if canonical(a.services) != owned || !hasService(a.services, "web") || a.serviceFor(42, "") != "nginx" {
		t.Fatal("helper failure replaced the set")
	}
	if !strings.Contains(buf.String(), "bot-agent: program discovery failed") {
		t.Fatalf("log %s", buf.String())
	}
}

// webUnit is one running catalog program. ports is what sockets reports.
func webUnit(verb string, ports []int) ([]byte, error) {
	switch verb {
	case "list":
		return programs.EncodeList([]programs.UnitInfo{{
			Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active",
		}}), nil
	case "sockets":
		return programs.EncodeSockets(ports, protocol.Str("joe")), nil
	default:
		return nil, errors.New(verb)
	}
}

func enableRunner(t *testing.T, opt *Options, helper Helper) {
	t.Helper()
	opt.PolicyPath = writeRunner(t, true, false)
	opt.PolicyUID = rootUID
	opt.GroupPath = joeGroup(t)
	opt.Helper = helper
}

func helloAt(t *testing.T, r *refreshRun, n int) protocol.Hello {
	t.Helper()
	seen := 0
	for _, f := range r.srv.Frames() {
		if f.Type != "hello" {
			continue
		}
		seen++
		if seen != n {
			continue
		}
		var hello protocol.Hello
		if err := json.Unmarshal(f.Raw, &hello); err != nil {
			t.Fatal(err)
		}
		return hello
	}
	t.Fatalf("hello %d of %d; %v", n, seen, frameTypes(r.srv.Frames()))
	return protocol.Hello{}
}

func healthDrops(r *refreshRun, id string) int {
	return strings.Count(r.log.String(), "drop health check id="+fmt.Sprintf("%q", id))
}

func nginxAndWebChecks() []protocol.HealthCheck {
	return []protocol.HealthCheck{
		{ID: "nginx", Service: "nginx", Protocol: "tcp", Port: 9, IntervalSec: 60},
		{ID: "web", Service: "web", Protocol: "tcp", Port: 3031, IntervalSec: 60},
	}
}

// TestFailedScanWithOwnedSuccessIsIncomplete fails if the owned-success arm
// sets complete back to true. The returned result still contains the unit,
// which is what proves that arm ran.
func TestFailedScanWithOwnedSuccessIsIncomplete(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	breakScan(t, proc)
	var buf bytes.Buffer
	helper := &fakeHelper{fn: func(verb string, _ []byte) ([]byte, error) {
		return webUnit(verb, []int{3031})
	}}
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Log: stdLog(&buf), Helper: helper,
			ServicesRefreshEvery: 300 * time.Second,
		},
		policy: policy.Policy{Exists: true, Valid: true, Enabled: true},
	}
	res, complete := a.discover(context.Background())
	if complete {
		t.Fatal("scan failure with a successful owned lookup reported complete")
	}
	if !hasService(res.Services, "web") {
		t.Fatalf("owned success did not run: %+v", res.Services)
	}
	if hasService(a.services, "web") || a.serviceFor(42, "") != "" {
		t.Fatalf("stored %+v service %q", a.services, a.serviceFor(42, ""))
	}
	if !helper.saw("list") || !helper.saw("sockets") {
		t.Fatalf("calls %v", helper.Calls())
	}
	sent := false
	w := &writer{onSend: func(any) error {
		sent = true
		return errors.New("sent")
	}}
	err := a.maybeServices(context.Background(), w, time.Date(2026, 10, 15, 12, 1, 0, 0, time.UTC))
	if err != nil || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}

	proc, sys, _ := fixtureProc(t, 9)
	breakScan(t, proc)
	passwd = joePasswd(t)
	sessionHelper := &fakeHelper{fn: func(verb string, _ []byte) ([]byte, error) {
		return webUnit(verb, []int{3031})
	}}
	r := startTuned(t, proc, sys, passwd, func(_ *fakeserver.Server, opt *Options) {
		enableRunner(t, opt, sessionHelper)
	})
	defer stopRun(t, r.cancel, r.errCh)
	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if hello.ServicesComplete || len(hello.Services) != 0 || hasService(hello.Services, "web") {
		t.Fatalf("hello %s", raw)
	}
	if !strings.Contains(string(raw), `"services_complete":false`) {
		t.Fatalf("hello %s", raw)
	}
	if !sessionHelper.saw("list") || !sessionHelper.saw("sockets") {
		t.Fatalf("calls %v", sessionHelper.Calls())
	}
	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)
	r.done(t)
}

func TestIncompleteHelloUsesPreviousHealthSet(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		proc, sys, _ := fixtureProc(t, 9)
		breakScan(t, proc)
		helper := &fakeHelper{fn: func(verb string, _ []byte) ([]byte, error) {
			return webUnit(verb, []int{3031})
		}}
		r := startTuned(t, proc, sys, joePasswd(t), func(srv *fakeserver.Server, opt *Options) {
			srv.Config.HealthChecks = nginxAndWebChecks()
			enableRunner(t, opt, helper)
		})
		defer stopRun(t, r.cancel, r.errCh)
		r.until(t, 60*time.Second)
		hello, raw := helloOf(t, r)
		if hello.ServicesComplete || len(hello.Services) != 0 {
			t.Fatalf("hello %s", raw)
		}
		if !helper.saw("list") || !helper.saw("sockets") {
			t.Fatalf("calls %v", helper.Calls())
		}
		if healthDrops(r, "nginx") != 1 || healthDrops(r, "web") != 1 {
			t.Fatalf("drops nginx=%d web=%d log %s", healthDrops(r, "nginx"), healthDrops(r, "web"), redactLog(&r.log, r.token))
		}
		r.done(t)
	})

	t.Run("previous", func(t *testing.T) {
		proc, sys, _ := fixtureProc(t, 9)
		var webOn atomic.Bool
		helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
			if !webOn.Load() {
				if verb == "list" {
					return programs.EncodeList(nil), nil
				}
				return nil, errors.New(verb)
			}
			return webUnit(verb, []int{3031})
		}}
		r := startTuned(t, proc, sys, joePasswd(t), func(srv *fakeserver.Server, opt *Options) {
			srv.Config.HealthChecks = nginxAndWebChecks()
			enableRunner(t, opt, helper)
		})
		defer stopRun(t, r.cancel, r.errCh)
		r.until(t, 60*time.Second)
		hello, raw := helloOf(t, r)
		if !hello.ServicesComplete || !hasService(hello.Services, "nginx") || hasService(hello.Services, "web") {
			t.Fatalf("hello %s", raw)
		}
		if healthDrops(r, "nginx") != 0 || healthDrops(r, "web") != 1 {
			t.Fatalf("drops nginx=%d web=%d log %s", healthDrops(r, "nginx"), healthDrops(r, "web"), redactLog(&r.log, r.token))
		}

		webOn.Store(true)
		breakScan(t, proc)
		r.srv.CloseConn()
		if !r.srv.WaitType("hello", 2, 5*time.Second) {
			t.Fatalf("no second hello %v log %s", frameTypes(r.srv.Frames()), redactLog(&r.log, r.token))
		}
		limit := time.Now().Add(5 * time.Second)
		for time.Now().Before(limit) && r.srv.Pings() < 2 {
			time.Sleep(2 * time.Millisecond)
		}
		if r.srv.Pings() < 2 {
			t.Fatalf("pings %d frames %v log %s", r.srv.Pings(), frameTypes(r.srv.Frames()), redactLog(&r.log, r.token))
		}
		second := helloAt(t, r, 2)
		if second.ServicesComplete || len(second.Services) != 0 || hasService(second.Services, "web") {
			t.Fatalf("second hello %+v", second)
		}
		lists, sockets := 0, 0
		for _, c := range helper.Calls() {
			switch c {
			case "list":
				lists++
			case "sockets":
				sockets++
			}
		}
		if lists < 2 || sockets < 1 {
			t.Fatalf("calls %v", helper.Calls())
		}
		if healthDrops(r, "nginx") != 0 || healthDrops(r, "web") != 2 {
			t.Fatalf("drops nginx=%d web=%d log %s", healthDrops(r, "nginx"), healthDrops(r, "web"), redactLog(&r.log, r.token))
		}
		if v := r.srv.Violations(); len(v) != 0 {
			t.Fatalf("violations %v", v)
		}
	})
}

// TestIncompleteReconnectSendsUnchangedSet is the sequence where this
// connection has not delivered a set, but lastCanon still names the previous
// connection's set. The 60s tick must send that set.
func TestIncompleteReconnectSendsUnchangedSet(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	var pings atomic.Int32
	var hold atomic.Bool
	var arms atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	r := startTuned(t, proc, sys, passwd, func(_ *fakeserver.Server, opt *Options) {
		base := opt.After
		opt.After = func(d time.Duration) <-chan time.Time {
			if d == 60*time.Second {
				arms.Add(1)
			}
			return base(d)
		}
		// The second ping is sent after the incomplete hello clears lastCanon
		// and before loop arms. Hold it so the clock stays at T0 until that
		// arm exists and the 60s tick cannot be missed.
		opt.OnSend = func(v any) error {
			if _, ok := v.(protocol.Ping); ok && pings.Add(1) == 2 {
				hold.Store(true)
				<-release
			}
			return nil
		}
	})
	defer stopRun(t, r.cancel, r.errCh)
	defer letGo()

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if !hello.ServicesComplete || !hasService(hello.Services, "nginx") {
		t.Fatalf("hello %s", raw)
	}
	r.wantServices(t, 0)

	breakScan(t, proc)
	r.srv.CloseConn()
	if !r.srv.WaitType("hello", 2, 5*time.Second) {
		t.Fatalf("no second hello %v log %s", frameTypes(r.srv.Frames()), redactLog(&r.log, r.token))
	}
	limit := time.Now().Add(5 * time.Second)
	for time.Now().Before(limit) && !hold.Load() {
		time.Sleep(2 * time.Millisecond)
	}
	if !hold.Load() {
		t.Fatalf("second ping was not held frames %v log %s", frameTypes(r.srv.Frames()), redactLog(&r.log, r.token))
	}
	second := helloAt(t, r, 2)
	if second.ServicesComplete || len(second.Services) != 0 {
		t.Fatalf("second hello %+v", second)
	}
	before := arms.Load()
	restoreScan(t, proc)
	letGo()
	limit = time.Now().Add(5 * time.Second)
	for time.Now().Before(limit) && arms.Load() <= before {
		time.Sleep(2 * time.Millisecond)
	}
	if arms.Load() <= before {
		t.Fatalf("second loop did not arm (%d) frames %v log %s", arms.Load(), frameTypes(r.srv.Frames()), redactLog(&r.log, r.token))
	}
	r.fire(t, 60*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 || canonical(sets[0]) != canonical(hello.Services) {
		t.Fatalf("frame %+v hello %+v", sets, hello.Services)
	}
	if v := r.srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}
