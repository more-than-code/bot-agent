package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// fakeClock is the session loop's Now and After. Advance it only while the
// loop is armed; delivery is buffered so a fire that lands before select
// still wakes that wait.
type fakeClock struct {
	mu    sync.Mutex
	start time.Time
	now   time.Time
	waits []clockWait
}

type clockWait struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{start: start, now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	now := c.now
	if d <= 0 {
		c.mu.Unlock()
		ch <- now
		return ch
	}
	c.waits = append(c.waits, clockWait{at: now.Add(d), ch: ch})
	c.mu.Unlock()
	return ch
}

func (c *fakeClock) advanceBy(d time.Duration) {
	c.mu.Lock()
	at := c.now.Add(d)
	c.mu.Unlock()
	c.advanceTo(at)
}

func (c *fakeClock) advanceTo(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.Before(c.now) {
		return
	}
	c.now = at
	keep := make([]clockWait, 0, len(c.waits))
	for _, w := range c.waits {
		if c.now.Before(w.at) {
			keep = append(keep, w)
			continue
		}
		select {
		case w.ch <- c.now:
		default:
		}
	}
	c.waits = keep
}

func (c *fakeClock) armed(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waits {
		if w.at.Equal(at) {
			return true
		}
	}
	return false
}

func (c *fakeClock) waitOffsets() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.waits))
	for i, w := range c.waits {
		out[i] = w.at.Sub(c.start)
	}
	return out
}

func (c *fakeClock) nowOffset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.start)
}

// refreshRun is one enrolled session on a fake clock. Metrics and process
// paces are a day, and the server heartbeat is 120s, so a 60s discovery
// interval is always the next arm.
type refreshRun struct {
	srv      *fakeserver.Server
	clk      *fakeClock
	log      bytes.Buffer
	proc     string
	token    string
	discover time.Duration
	cancel   context.CancelFunc
	errCh    <-chan error
}

// startRefresh enrolls one session. beat <= 0 keeps the 120s heartbeat.
// hook, when set, runs at the start of each client Send.
func startRefresh(t *testing.T, discover, refresh time.Duration, beat int, hook func(*fakeClock, any) error) *refreshRun {
	t.Helper()
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	if beat > 0 {
		srv.Heartbeat = beat
	}
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	srv.Config.HealthChecks = []protocol.HealthCheck{{
		ID: "absent", Service: "no-such-service", Protocol: "tcp", Port: 1, IntervalSec: 60,
	}}
	proc, sys, passwd := fixtureProc(t, 9)
	envPath, token := writeEnv(t, srv.URL, t.TempDir())
	pace := &Pace{}
	pace.Metrics.Store(int64(24 * time.Hour))
	pace.Process.Store(int64(24 * time.Hour))
	r := &refreshRun{
		srv:      srv,
		clk:      newFakeClock(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)),
		proc:     proc,
		token:    token,
		discover: discover,
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	r.cancel = cancel
	r.errCh = errCh
	var onSend func(any) error
	if hook != nil {
		onSend = func(v any) error { return hook(r.clk, v) }
	}
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:               statFS(1000, 1000),
			EUID:                 func() int { return 1000 },
			Now:                  r.clk.Now,
			After:                r.clk.After,
			OnSend:               onSend,
			DiscoverEvery:        discover,
			ServicesRefreshEvery: refresh,
			Pace:                 pace,
			Sleep:                func(context.Context, time.Duration) error { return nil },
			Log:                  stdLog(&r.log),
		})
	}()
	return r
}

func (r *refreshRun) at(d time.Duration) time.Time {
	return r.clk.start.Add(d)
}

func (r *refreshRun) until(t *testing.T, at time.Duration) {
	t.Helper()
	r.untilAt(t, r.at(at))
}

func (r *refreshRun) untilAt(t *testing.T, deadline time.Time) {
	t.Helper()
	limit := time.Now().Add(5 * time.Second)
	for time.Now().Before(limit) {
		if r.clk.armed(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("not armed at %s (now %s waits %v) frames %v log %s",
		deadline.Sub(r.clk.start), r.clk.nowOffset(), r.clk.waitOffsets(),
		frameTypes(r.srv.Frames()), redactLog(&r.log, r.token, r.srv.Credential))
}

// awaitArm waits until the loop is armed. A pong or metrics ack can leave
// more than one waiter; the fake stop is a no-op, so those leftovers share
// the deadline the loop is actually waiting on.
func (r *refreshRun) awaitArm(t *testing.T) time.Time {
	t.Helper()
	limit := time.Now().Add(5 * time.Second)
	for time.Now().Before(limit) {
		r.clk.mu.Lock()
		var at time.Time
		agree := len(r.clk.waits) > 0
		if agree {
			at = r.clk.waits[0].at
			for _, w := range r.clk.waits[1:] {
				if !w.at.Equal(at) {
					agree = false
					break
				}
			}
		}
		r.clk.mu.Unlock()
		if agree {
			return at
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("not armed (now %s waits %v) frames %v log %s",
		r.clk.nowOffset(), r.clk.waitOffsets(),
		frameTypes(r.srv.Frames()), redactLog(&r.log, r.token, r.srv.Credential))
	return time.Time{}
}

// fire runs the discovery tick at logical time at. The next arm is one
// discovery interval later, which holds for a 60s interval against a 120s
// heartbeat and day-long metric paces.
func (r *refreshRun) fire(t *testing.T, at time.Duration) {
	t.Helper()
	r.until(t, at)
	r.clk.advanceTo(r.at(at))
	r.until(t, at+r.discover)
}

func (r *refreshRun) services() int {
	return countType(r.srv.Frames(), "services")
}

func (r *refreshRun) drops() int {
	return strings.Count(r.log.String(), `drop health check id="absent"`)
}

// wantServices asserts the server has read exactly n services frames.
// Callers reach it only after the agent has armed its next wait, so a count
// that is already n means this tick did not send. A short count waits until
// the server's read path has recorded the frame. The deadline is the failure
// bound, not proof that a frame was absent.
func (r *refreshRun) wantServices(t *testing.T, n int) {
	t.Helper()
	got := r.services()
	if got == n {
		return
	}
	if got > n || !r.srv.WaitType("services", n, 2*time.Second) || r.services() != n {
		t.Fatalf("services=%d want %d; %v log %s", r.services(), n, frameTypes(r.srv.Frames()), redactLog(&r.log, r.token, r.srv.Credential))
	}
}

func (r *refreshRun) serviceSets(t *testing.T) [][]protocol.Discovered {
	t.Helper()
	var out [][]protocol.Discovered
	for _, f := range r.srv.Frames() {
		switch f.Type {
		case "hello":
			continue
		case "services":
			var msg protocol.Services
			if err := json.Unmarshal(f.Raw, &msg); err != nil {
				t.Fatal(err)
			}
			out = append(out, msg.Services)
		}
	}
	return out
}

func (r *refreshRun) helloServices(t *testing.T) []protocol.Discovered {
	t.Helper()
	for _, f := range r.srv.Frames() {
		if f.Type != "hello" {
			continue
		}
		var hello protocol.Hello
		if err := json.Unmarshal(f.Raw, &hello); err != nil {
			t.Fatal(err)
		}
		return hello.Services
	}
	t.Fatal("no hello")
	return nil
}

func (r *refreshRun) done(t *testing.T) {
	t.Helper()
	if v := r.srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
	if n := countType(r.srv.Frames(), "hello"); n != 1 {
		t.Fatalf("hello count %d frames %v", n, frameTypes(r.srv.Frames()))
	}
}

func TestUnchangedServicesResentEvery300s(t *testing.T) {
	for _, every := range []time.Duration{300 * time.Second, 0, -time.Second} {
		t.Run(every.String(), func(t *testing.T) {
			r := startRefresh(t, 60*time.Second, every, 0, nil)
			defer stopRun(t, r.cancel, r.errCh)
			assertUnchangedCadence(t, r)
			r.done(t)
		})
	}
}

func assertUnchangedCadence(t *testing.T, r *refreshRun) {
	t.Helper()
	// Hello carried the services, so the first discovery ticks must not.
	r.until(t, 60*time.Second)
	r.wantServices(t, 0)
	if r.drops() != 1 {
		t.Fatalf("hello filter logs=%d log %s", r.drops(), redactLog(&r.log, r.token, r.srv.Credential))
	}
	for _, sec := range []time.Duration{60 * time.Second, 120 * time.Second, 180 * time.Second, 240 * time.Second} {
		r.fire(t, sec)
		r.wantServices(t, 0)
		if r.drops() != 1 {
			t.Fatalf("refilter at %s", sec)
		}
	}
	// 299s is between the 240s and 300s ticks.
	r.clk.advanceTo(r.at(299 * time.Second))
	r.wantServices(t, 0)

	before := r.log.String()
	r.fire(t, 300*time.Second)
	r.wantServices(t, 1)
	if r.drops() != 1 {
		t.Fatal("refilter ran on the 300s refresh")
	}
	if r.log.String() != before {
		t.Fatalf("refresh logged %s", redactLog(&r.log, r.token, r.srv.Credential))
	}
	if canonical(r.serviceSets(t)[0]) != canonical(r.helloServices(t)) {
		t.Fatal("refresh payload differs from hello")
	}

	for _, sec := range []time.Duration{360 * time.Second, 420 * time.Second, 480 * time.Second, 540 * time.Second} {
		r.fire(t, sec)
		r.wantServices(t, 1)
	}
	before = r.log.String()
	r.fire(t, 600*time.Second)
	r.wantServices(t, 2)
	if r.drops() != 1 {
		t.Fatal("refilter ran on the second refresh")
	}
	if r.log.String() != before {
		t.Fatal("second refresh logged")
	}
	sets := r.serviceSets(t)
	hello := canonical(r.helloServices(t))
	if len(sets) != 2 || canonical(sets[0]) != hello || canonical(sets[1]) != hello {
		t.Fatal("refreshes are not the hello set")
	}
}

func TestServicesNotRefreshedAt299s(t *testing.T) {
	r := startRefresh(t, 299*time.Second, 300*time.Second, 0, nil)
	defer stopRun(t, r.cancel, r.errCh)

	// First wait is the 120s ping. Jumping to 299 runs discovery then.
	r.until(t, 120*time.Second)
	r.clk.advanceTo(r.at(299 * time.Second))
	r.until(t, 419*time.Second)
	r.wantServices(t, 0)
	if r.drops() != 1 {
		t.Fatal("refilter at 299s")
	}

	before := r.log.String()
	r.clk.advanceTo(r.at(598 * time.Second))
	r.until(t, 718*time.Second)
	r.wantServices(t, 1)
	if r.drops() != 1 {
		t.Fatal("refilter on the refresh after 299s")
	}
	if r.log.String() != before {
		t.Fatal("refresh logged")
	}

	// 897s is 299s after the refresh at 598s: still inside the window.
	r.clk.advanceTo(r.at(897 * time.Second))
	r.until(t, 1017*time.Second)
	r.wantServices(t, 1)

	r.clk.advanceTo(r.at(1196 * time.Second))
	r.until(t, 1316*time.Second)
	r.wantServices(t, 2)
	sets := r.serviceSets(t)
	hello := canonical(r.helloServices(t))
	if len(sets) != 2 || canonical(sets[0]) != hello || canonical(sets[1]) != hello {
		t.Fatal("boundary refreshes differ from hello")
	}
	r.done(t)
}

func TestServicesChangeResetsRefreshAndRefilter(t *testing.T) {
	r := startRefresh(t, 60*time.Second, 300*time.Second, 0, nil)
	defer stopRun(t, r.cancel, r.errCh)

	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)
	if r.drops() != 1 {
		t.Fatalf("drops %d before the change", r.drops())
	}
	addExtraListener(t, r.proc)
	r.fire(t, 120*time.Second)
	r.wantServices(t, 1)
	if r.drops() != 2 {
		t.Fatalf("drops %d after the change; refilter did not run", r.drops())
	}
	if !servicesContain(r, "extra") {
		t.Fatal("change frame has no extra")
	}

	for _, sec := range []time.Duration{180 * time.Second, 240 * time.Second, 300 * time.Second, 360 * time.Second} {
		r.fire(t, sec)
		r.wantServices(t, 1)
		if r.drops() != 2 {
			t.Fatalf("refilter at %s", sec)
		}
	}
	// 419s is 299s after the change at 120s, and not yet the 420s tick.
	r.clk.advanceTo(r.at(419 * time.Second))
	r.wantServices(t, 1)

	before := r.log.String()
	r.fire(t, 420*time.Second)
	r.wantServices(t, 2)
	if r.drops() != 2 {
		t.Fatal("refilter ran on the refresh after the change")
	}
	if r.log.String() != before {
		t.Fatal("refresh after the change logged")
	}
	sets := r.serviceSets(t)
	if len(sets) != 2 || canonical(sets[0]) != canonical(sets[1]) {
		t.Fatal("refresh payload differs from the changed set")
	}
	if !servicesContain(r, "extra") {
		t.Fatal("refresh dropped extra")
	}
	r.done(t)
}

func servicesContain(r *refreshRun, name string) bool {
	for _, f := range r.srv.Frames() {
		if f.Type == "services" && strings.Contains(string(f.Raw), `"name":"`+name+`"`) {
			return true
		}
	}
	return false
}

// TestServicesRefreshStaysOnTickGrid advances the fake clock a few milliseconds
// inside every Send. The 300s refresh and the next one at 600s stay on the
// discovery grid measured from the loop's start time.
//
// Heartbeat is 80s so those two ticks are not also pings. A 120s ping lands on
// the 600s tick and would advance the clock by the same amount a late
// lastServicesSent stamp had added, hiding the slip.
func TestServicesRefreshStaysOnTickGrid(t *testing.T) {
	const skew = 5 * time.Millisecond
	r := startRefresh(t, 60*time.Second, 300*time.Second, 80, func(clk *fakeClock, _ any) error {
		clk.advanceBy(skew)
		return nil
	})
	defer stopRun(t, r.cancel, r.errCh)

	tick := r.awaitArm(t)
	origin := tick.Add(-r.discover)
	if !origin.Equal(r.clk.start.Add(2 * skew)) {
		t.Fatalf("loop start %s, want 10ms after epoch", origin.Sub(r.clk.start))
	}

	want := 0
	saw300, saw600 := false, false
	for step := 0; step < 40 && !saw600; step++ {
		if tick.Sub(origin)%time.Second != 0 {
			t.Fatalf("deadline %s is off the loop grid", tick.Sub(origin))
		}
		r.clk.advanceTo(tick)
		elapsed := tick.Sub(origin)
		following := r.awaitArm(t)
		if following.Sub(origin)%time.Second != 0 {
			t.Fatalf("next deadline %s drifted", following.Sub(origin))
		}
		if elapsed%r.discover == 0 {
			k := elapsed / r.discover
			if k == 5 || k == 10 {
				want++
			}
			r.wantServices(t, want)
			saw300 = saw300 || k == 5
			saw600 = k == 10
		} else if got := r.services(); got != want {
			t.Fatalf("services=%d at %s, off the discovery tick", got, elapsed)
		}
		tick = following
	}
	if !saw300 || !saw600 {
		t.Fatalf("refresh ticks 300s=%v 600s=%v services=%d", saw300, saw600, r.services())
	}
	if r.drops() != 1 {
		t.Fatalf("drops %d; refresh refiltered", r.drops())
	}
	r.done(t)
}

// TestFailedServicesSendPreservesState checks a writer whose Send fails once.
// The error is returned, the stamps stay put, and the reconnect hello carries
// the set that did not go out.
func TestFailedServicesSendPreservesState(t *testing.T) {
	t.Run("fields", func(t *testing.T) {
		proc, _, passwd := fixtureProc(t, 9)
		var buf bytes.Buffer
		start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
		a := &agent{
			opt: Options{
				ProcRoot:             proc,
				Passwd:               passwd,
				Log:                  stdLog(&buf),
				Now:                  func() time.Time { return start },
				DiscoverEvery:        60 * time.Second,
				ServicesRefreshEvery: 300 * time.Second,
			},
		}
		before := canonical(a.discover().Services)
		a.lastCanon = before
		a.lastServicesSent = start
		addExtraListener(t, proc)
		sendErr := errors.New("send failed")
		w := &writer{onSend: func(any) error { return sendErr }}
		err := a.maybeServices(context.Background(), w, start.Add(60*time.Second))
		if !errors.Is(err, sendErr) {
			t.Fatalf("err %v", err)
		}
		if a.lastCanon != before {
			t.Fatal("lastCanon changed on a failed send")
		}
		if !a.lastServicesSent.Equal(start) {
			t.Fatalf("lastServicesSent %s", a.lastServicesSent.Sub(start))
		}
		if a.healthCancel != nil {
			t.Fatal("refilter ran on a failed send")
		}
		if canonical(a.discover().Services) == before {
			t.Fatal("discovered set did not change")
		}
	})

	t.Run("reconnect", func(t *testing.T) {
		var once atomic.Bool
		r := startRefresh(t, 60*time.Second, 300*time.Second, 0, func(_ *fakeClock, v any) error {
			if _, ok := v.(protocol.Services); ok && once.CompareAndSwap(false, true) {
				return errors.New("send failed")
			}
			return nil
		})
		defer stopRun(t, r.cancel, r.errCh)
		r.until(t, 60*time.Second)
		addExtraListener(t, r.proc)
		r.clk.advanceTo(r.at(60 * time.Second))
		if !r.srv.WaitType("hello", 2, 3*time.Second) {
			t.Fatalf("hellos %d frames %v log %s", countType(r.srv.Frames(), "hello"), frameTypes(r.srv.Frames()), redactLog(&r.log, r.token, r.srv.Credential))
		}
		hellos := r.hellos(t)
		if len(hellos) != 2 {
			t.Fatalf("hellos %d", len(hellos))
		}
		if serviceName(hellos[0], "extra") {
			t.Fatal("first hello already had the new set")
		}
		if !serviceName(hellos[1], "extra") {
			t.Fatal("reconnect hello is missing the current set")
		}
		if n := r.services(); n != 0 {
			t.Fatalf("services frames %d; the failed send was written", n)
		}
	})
}

func (r *refreshRun) hellos(t *testing.T) [][]protocol.Discovered {
	t.Helper()
	var out [][]protocol.Discovered
	for _, f := range r.srv.Frames() {
		if f.Type != "hello" {
			continue
		}
		var hello protocol.Hello
		if err := json.Unmarshal(f.Raw, &hello); err != nil {
			t.Fatal(err)
		}
		out = append(out, hello.Services)
	}
	return out
}

func serviceName(services []protocol.Discovered, name string) bool {
	for _, s := range services {
		if s.Name == name {
			return true
		}
	}
	return false
}
