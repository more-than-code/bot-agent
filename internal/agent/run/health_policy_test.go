package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/collect"
	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/conn"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/spool"
)

func withRunPolicy(spec []byte, hp string) []byte {
	return bytes.Replace(spec, []byte(`"kind":"`), []byte(`"health_policy":`+hp+`,"kind":"`), 1)
}

// TestDeployFailurePolicy covers agent-protocol v1.9 item 2: each
// on_deploy_failure branch after a failed Docker health wait.
func TestDeployFailurePolicy(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		hp       string
		pre      string
		outcome  string
		rollback bool
		stop     bool
	}{
		{"default running", "image_deploy", "", "running", "rolled_back", true, false},
		{"default missing", "image_deploy", "", "missing", "failed", false, true},
		{"rollback running", "image_deploy", `{"on_deploy_failure":"rollback"}`, "running", "rolled_back", true, false},
		{"rollback missing", "image_deploy", `{"on_deploy_failure":"rollback"}`, "missing", "failed", false, true},
		{"stop running", "image_deploy", `{"on_deploy_failure":"stop"}`, "running", "failed", false, true},
		{"stop missing", "image_deploy", `{"on_deploy_failure":"stop"}`, "missing", "failed", false, true},
		{"keep running", "image_deploy", `{"on_deploy_failure":"keep"}`, "running", "failed", false, false},
		{"keep missing", "image_deploy", `{"on_deploy_failure":"keep"}`, "missing", "failed", false, false},
		// image_rollback is unchanged whatever the policy says.
		{"image_rollback keep", "image_rollback", `{"on_deploy_failure":"keep"}`, "running", "rolled_back", true, false},
		{"image_rollback stop", "image_rollback", `{"on_deploy_failure":"stop"}`, "running", "rolled_back", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			if tc.pre == "running" {
				seq.pre = runningStatus(0)
			} else {
				seq.pre = missingStatus()
			}
			seq.post = runningStatus(0)
			unhealthy := "unhealthy"
			seq.health = &unhealthy
			spec := specWithHealth(true, hcFast)
			if tc.kind == "image_rollback" {
				spec = []byte(strings.TrimSuffix(string(spec), "}") + `,"digest":"` + imgDigest + `"}`)
				spec = bytes.Replace(spec, []byte(`"force":true`), []byte(`"force":false`), 1)
			}
			if tc.hp != "" {
				spec = withRunPolicy(spec, tc.hp)
			}
			res := mustImage(t, a, tc.kind, spec, 120)
			assertDeploy(t, res, tc.outcome, "failed")
			if !seq.deployed {
				t.Fatalf("not deployed: %v", h.Calls())
			}
			if h.saw(images.VerbRollback) != tc.rollback || h.saw(images.VerbStop) != tc.stop {
				t.Fatalf("rollback %v stop %v calls %v", h.saw(images.VerbRollback), h.saw(images.VerbStop), h.Calls())
			}
		})
	}
}

// superSeq is a helper fake for the supervisor: one owned container
// acme-web, optionally an acme-web-prev, and the verbs the supervisor calls.
type superSeq struct {
	mu        sync.Mutex
	host      string
	state     string
	health    string
	policy    *images.ContainerPolicy
	prev      bool
	listErr   bool
	restarts  int
	rollbacks int
	failVerb  bool
}

func (s *superSeq) call(verb string, body []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := imgDigest
	switch verb {
	case images.VerbList:
		if s.listErr {
			return nil, errHelper
		}
		labels := ownedTestLabels(s.host, "acme", "web")
		cur := images.ContainerInfo{ID: "a", Name: "/acme-web", State: s.state, Digest: &d, Labels: labels, Policy: s.policy}
		if s.health != "" {
			h := s.health
			cur.Health = &h
		}
		items := []images.ContainerInfo{cur}
		if s.prev {
			items = append(items, images.ContainerInfo{ID: "b", Name: "/acme-web-prev", State: "exited", Digest: &d, Labels: labels, Policy: s.policy})
		}
		// A foreign container with the same labels but another name is ignored.
		items = append(items, images.ContainerInfo{ID: "c", Name: "/acme-web-new", State: "running", Digest: &d, Labels: labels, Policy: s.policy, Health: protocol.Str("unhealthy")})
		return images.Encode(images.ListResponse{Items: items}), nil
	case images.VerbRestart:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		s.restarts++
		if s.failVerb {
			return []byte(`{"error":"docker_unavailable"}`), nil
		}
		return images.Encode(images.ActionResult{Status: runningStatus(0)}), nil
	case images.VerbRollback:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		s.rollbacks++
		return images.Encode(images.IDResponse{ContainerID: "b"}), nil
	case images.VerbStatus:
		return images.Encode(images.StatusResult{Status: runningStatus(0), SpecHash: imgHash}), nil
	case images.VerbPull:
		return []byte(`{"error":"image_unavailable"}`), nil
	case images.VerbLogs:
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}}), nil
	default:
		return nil, errors.New(verb)
	}
}

func (s *superSeq) set(fn func(*superSeq)) {
	s.mu.Lock()
	fn(s)
	s.mu.Unlock()
}

func (s *superSeq) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts, s.rollbacks
}

func superRun(t *testing.T, pol images.ContainerPolicy) (*agent, *testClock, *superSeq) {
	t.Helper()
	a, h, clock, _ := imageRun(t)
	idPath, host := writeMachineID(t)
	a.opt.MachineID = idPath
	seq := &superSeq{host: host, state: "running", health: "unhealthy", policy: &pol}
	h.fn = seq.call
	return a, clock, seq
}

// poll advances the clock by step and runs one supervisor pass.
func poll(a *agent, c *testClock, step time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(step)
	c.mu.Unlock()
	a.superviseOnce(context.Background())
}

func actions(a *agent) []string {
	var out []string
	for _, f := range a.autoq.list() {
		out = append(out, f.Action+":"+f.Result)
	}
	return out
}

func TestSupervisorThreshold(t *testing.T) {
	t.Run("acts at the threshold", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 45, MaxActionsPerHour: 3})
		poll(a, c, 0)
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 0 || len(a.autoq.list()) != 0 {
			t.Fatalf("early: restarts %d frames %v", r, actions(a))
		}
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 1 {
			t.Fatalf("restarts %d", r)
		}
		q := a.autoq.list()
		if len(q) != 1 {
			t.Fatalf("%v", actions(a))
		}
		f := q[0]
		if f.Type != "auto_action" || f.Action != "restart" || f.Result != "ok" || f.Project != "acme" || f.Entry != "web" || f.Container != "acme-web" || f.Reason != "unhealthy for 45s" || f.Digest == nil || *f.Digest != imgDigest {
			t.Fatalf("%+v", f)
		}
		raw, err := protocol.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.ValidateAgent(raw); err != nil {
			t.Fatalf("%v %s", err, raw)
		}
	})

	t.Run("a healthy poll restarts the streak", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 30, MaxActionsPerHour: 3})
		poll(a, c, 0)
		poll(a, c, 15*time.Second)
		seq.set(func(s *superSeq) { s.health = "healthy" })
		poll(a, c, 15*time.Second)
		seq.set(func(s *superSeq) { s.health = "unhealthy" })
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 0 {
			t.Fatalf("restarts %d", r)
		}
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 1 {
			t.Fatalf("restarts %d", r)
		}
	})

	t.Run("a failed list restarts the streak", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 30, MaxActionsPerHour: 3})
		poll(a, c, 0)
		seq.set(func(s *superSeq) { s.listErr = true })
		poll(a, c, 15*time.Second)
		seq.set(func(s *superSeq) { s.listErr = false })
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 0 {
			t.Fatalf("restarts %d", r)
		}
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 1 {
			t.Fatalf("restarts %d", r)
		}
	})

	t.Run("starting and null health never act", func(t *testing.T) {
		for _, h := range []string{"starting", ""} {
			a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
			seq.set(func(s *superSeq) { s.health = h })
			for i := 0; i < 10; i++ {
				poll(a, c, 15*time.Second)
			}
			if r, _ := seq.counts(); r != 0 || len(a.autoq.list()) != 0 {
				t.Fatalf("%q: restarts %d", h, r)
			}
		}
	})
}

func TestSupervisorSkips(t *testing.T) {
	t.Run("not during an image task", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
		lk := a.entries.get("acme", "web")
		if err := lk.rLock(context.Background()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 0 || len(a.autoq.list()) != 0 {
			lk.rUnlock()
			t.Fatalf("acted during task: restarts %d", r)
		}
		lk.rUnlock()
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 1 {
			t.Fatalf("restarts %d", r)
		}
	})

	t.Run("not on a stopped container", func(t *testing.T) {
		for _, st := range []string{"exited", "restarting", "paused", "created", "dead"} {
			a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
			seq.set(func(s *superSeq) { s.state = st })
			for i := 0; i < 10; i++ {
				poll(a, c, 15*time.Second)
			}
			if r, rb := seq.counts(); r != 0 || rb != 0 || len(a.autoq.list()) != 0 {
				t.Fatalf("%s: restarts %d rollbacks %d", st, r, rb)
			}
		}
	})

	t.Run("no policy or none", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
		seq.set(func(s *superSeq) { s.policy = nil })
		for i := 0; i < 5; i++ {
			poll(a, c, 15*time.Second)
		}
		seq.set(func(s *superSeq) {
			s.policy = &images.ContainerPolicy{OnUnhealthy: "none", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}
		})
		for i := 0; i < 5; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 0 {
			t.Fatalf("restarts %d", r)
		}
	})

	t.Run("other host", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
		seq.set(func(s *superSeq) { s.host = "someone-else" })
		for i := 0; i < 5; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 0 {
			t.Fatalf("restarts %d", r)
		}
	})
}

func TestSupervisorLimits(t *testing.T) {
	t.Run("window limit gives up once", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 2})
		for i := 0; i < 20; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 2 {
			t.Fatalf("restarts %d", r)
		}
		if got := strings.Join(actions(a), ","); got != "restart:ok,restart:ok,gave_up:ok" {
			t.Fatalf("%s", got)
		}
		// Still ignored after the window has passed: gave_up holds until a task.
		poll(a, c, 2*time.Hour)
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second)
		if r, _ := seq.counts(); r != 2 || len(a.autoq.list()) != 3 {
			t.Fatalf("restarts %d frames %v", r, actions(a))
		}
	})

	t.Run("window slides", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 1})
		poll(a, c, 0)
		poll(a, c, 15*time.Second) // restart 1
		seq.set(func(s *superSeq) { s.health = "healthy" })
		poll(a, c, 61*time.Minute)
		seq.set(func(s *superSeq) { s.health = "unhealthy" })
		poll(a, c, 15*time.Second)
		poll(a, c, 15*time.Second) // restart 2: the first is outside the hour
		if r, _ := seq.counts(); r != 2 || strings.Join(actions(a), ",") != "restart:ok,restart:ok" {
			t.Fatalf("restarts %d frames %v", r, actions(a))
		}
	})

	t.Run("failed action counts and reports error", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 1})
		seq.set(func(s *superSeq) { s.failVerb = true })
		for i := 0; i < 6; i++ {
			poll(a, c, 15*time.Second)
		}
		if got := strings.Join(actions(a), ","); got != "restart:error,gave_up:ok" {
			t.Fatalf("%s", got)
		}
	})

	t.Run("rollback once per deploy", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 5})
		seq.set(func(s *superSeq) { s.prev = true })
		for i := 0; i < 10; i++ {
			poll(a, c, 15*time.Second)
		}
		if _, rb := seq.counts(); rb != 1 {
			t.Fatalf("rollbacks %d", rb)
		}
		if got := strings.Join(actions(a), ","); got != "rollback:ok,gave_up:ok" {
			t.Fatalf("%s", got)
		}
	})

	t.Run("rollback without prev gives up", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 5})
		for i := 0; i < 10; i++ {
			poll(a, c, 15*time.Second)
		}
		if _, rb := seq.counts(); rb != 0 {
			t.Fatalf("rollbacks %d", rb)
		}
		q := a.autoq.list()
		if len(q) != 1 || q[0].Action != "gave_up" || q[0].Digest == nil {
			t.Fatalf("%+v", q)
		}
	})

	t.Run("deploy task resets counters", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 1})
		for i := 0; i < 6; i++ {
			poll(a, c, 15*time.Second)
		}
		if got := strings.Join(actions(a), ","); got != "restart:ok,gave_up:ok" {
			t.Fatalf("%s", got)
		}
		// A deploy that fails at the pull still resets the entry.
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 30)
		assertDeploy(t, res, "image_unavailable", "failed")
		for i := 0; i < 3; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 2 {
			t.Fatalf("restarts %d frames %v", r, actions(a))
		}
	})

	t.Run("status task does not reset counters", func(t *testing.T) {
		a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 1})
		for i := 0; i < 6; i++ {
			poll(a, c, 15*time.Second)
		}
		_ = mustImage(t, a, "image_status", []byte(`{"project":"acme","entry":"web"}`), 15)
		for i := 0; i < 3; i++ {
			poll(a, c, 15*time.Second)
		}
		if r, _ := seq.counts(); r != 1 {
			t.Fatalf("restarts %d", r)
		}
	})
}

func TestSupervisorLoopInterval(t *testing.T) {
	a, _, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 1})
	var n int
	var mu sync.Mutex
	inner := seq.call
	a.opt.Helper.(*fakeHelper).fn = func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbList {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return inner(verb, body)
	}
	a.opt.SuperviseEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.supervise(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := n
		mu.Unlock()
		if got >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if n < 3 {
		t.Fatalf("lists %d", n)
	}
}

func TestAutoQueue(t *testing.T) {
	var q autoQueue
	for i := 0; i < maxAutoPending+5; i++ {
		id := protocol.FormatTime(time.Unix(int64(i), 0))
		if dropped := q.add(protocol.AutoAction{ID: id}); (i >= maxAutoPending) != (dropped != "") {
			t.Fatalf("i %d dropped %q", i, dropped)
		}
	}
	list := q.list()
	if len(list) != maxAutoPending || list[0].ID != protocol.FormatTime(time.Unix(5, 0)) {
		t.Fatalf("len %d first %s", len(list), list[0].ID)
	}
	q.ack(list[10].ID)
	q.ack("unknown")
	if got := q.list(); len(got) != maxAutoPending-1 || got[10].ID == list[10].ID {
		t.Fatalf("len %d", len(got))
	}
	select {
	case <-q.wake():
	default:
		t.Fatal("no wake")
	}
}

// sessionAgent builds an agent the way Run does, without a runner policy,
// so a test can seed the auto_action queue before the first connection.
func sessionAgent(t *testing.T, srv *fakeserver.Server) *agent {
	t.Helper()
	clearAgentEnv(t)
	proc, sys, passwd := fixtureProc(t, 9)
	state := t.TempDir()
	opt := Options{
		EnvFile: writeCredEnv(t, srv, state), ProcRoot: proc, SysRoot: sys, Passwd: passwd,
		StatFS: statFS(1000, 1000), EUID: func() int { return 1000 },
		DiscoverEvery: time.Hour, Log: stdLog(&bytes.Buffer{}),
	}.withDefaults()
	cfg, err := config.Load(opt.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &agent{
		opt: opt, cfg: cfg, host: "pi3", spool: sp,
		col:      collect.New(collect.Roots{Proc: opt.ProcRoot, Sys: opt.SysRoot, Passwd: opt.Passwd}, opt.StatFS),
		bo:       conn.NewBackoff(),
		healthCh: make(chan protocol.Sample, 128), taskSlots: make(chan struct{}, maxRunningTasks),
	}
}

func autoFrame(id string) protocol.AutoAction {
	d := imgDigest
	return protocol.AutoAction{
		V: protocol.Version, Type: "auto_action", ID: id, T: protocol.FormatTime(time.Now()),
		Project: "acme", Entry: "web", Container: "acme-web", Action: "restart",
		Reason: "unhealthy for 45s", Result: "ok", Digest: &d,
	}
}

func countAuto(fr []fakeserver.Frame, id string) int {
	n := 0
	for _, f := range fr {
		if f.Type != "auto_action" {
			continue
		}
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(f.Raw, &head) == nil && head.ID == id {
			n++
		}
	}
	return n
}

func TestAutoActionResendAndAck(t *testing.T) {
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	srv.SkipNextAutoAck()
	a := sessionAgent(t, srv)
	first := "8d5f2b8e-0d6c-4b8e-9b8a-1f0c2d3e4f50"
	second := "8d5f2b8e-0d6c-4b8e-9b8a-1f0c2d3e4f51"
	a.autoq.add(autoFrame(first))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = a.session(ctx)
		}
	}()
	defer func() { cancel(); <-done }()

	// The first connection drops without an ack; the frame is sent again on
	// the next connection and then acknowledged.
	if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return countAuto(fr, first) >= 2 }) {
		t.Fatalf("frames %v", frameTypes(srv.Frames()))
	}
	waitQueue := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(a.autoq.list()) == n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("queue %d", len(a.autoq.list()))
	}
	waitQueue(0)
	if srv.Conns() < 2 {
		t.Fatalf("conns %d", srv.Conns())
	}

	// A frame queued while connected is sent at once, not at the next reconnect.
	conns := srv.Conns()
	a.autoq.add(autoFrame(second))
	if !srv.Wait(5*time.Second, func(fr []fakeserver.Frame) bool { return countAuto(fr, second) >= 1 }) {
		t.Fatalf("frames %v", frameTypes(srv.Frames()))
	}
	waitQueue(0)
	if srv.Conns() != conns || countAuto(srv.Frames(), second) != 1 {
		t.Fatalf("conns %d sends %d", srv.Conns(), countAuto(srv.Frames(), second))
	}
	acks := srv.AutoAcks()
	if len(acks) != 2 || acks[0] != first || acks[1] != second {
		t.Fatalf("acks %v", acks)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}
