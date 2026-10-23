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
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestSuperviseEveryDefault(t *testing.T) {
	if got := (Options{}).withDefaults().SuperviseEvery; got != 15*time.Second {
		t.Fatalf("default %s", got)
	}
	if got := (Options{SuperviseEvery: time.Second}).withDefaults().SuperviseEvery; got != time.Second {
		t.Fatalf("override %s", got)
	}
}

// (a) A real image_deploy blocked inside container_deploy keeps the
// supervisor off that entry; once it finishes the supervisor can act.
func TestSupervisorSkipsBlockedDeploy(t *testing.T) {
	a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
	h := a.opt.Helper.(*fakeHelper)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.fn = func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case images.VerbPull:
			return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
		case images.VerbDeploy:
			once.Do(func() { close(entered) })
			<-release
			return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
		}
		return seq.call(verb, body)
	}
	resCh := make(chan protocol.TaskResult, 1)
	go func() {
		_, res, _ := a.execute(context.Background(), imageTask("image_deploy", serviceSpec(true, 5, 3031), 600))
		resCh <- res
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy not reached")
	}
	for i := 0; i < 6; i++ {
		poll(a, c, 15*time.Second)
	}
	if r, rb := seq.counts(); r != 0 || rb != 0 || len(a.autoq.list()) != 0 {
		t.Fatalf("acted during deploy: restarts %d rollbacks %d frames %v", r, rb, actions(a))
	}
	close(release)
	select {
	case res := <-resCh:
		assertDeploy(t, res, "succeeded", "succeeded")
	case <-time.After(5 * time.Second):
		t.Fatal("deploy did not finish")
	}
	poll(a, c, 0)
	poll(a, c, 15*time.Second)
	if r, _ := seq.counts(); r != 1 {
		t.Fatalf("restarts after deploy %d", r)
	}
}

// (a, reverse) A task that arrives during a supervisor action waits for it,
// and gives up at its own timeout without calling the helper.
func TestTaskWaitsForSupervisorAction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int
		release bool
	}{
		{"waits then runs", 15, true},
		{"times out", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
			h := a.opt.Helper.(*fakeHelper)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var taskStatus atomic.Int32
			var acting atomic.Bool
			h.fn = func(verb string, body []byte) ([]byte, error) {
				if verb == images.VerbRestart {
					acting.Store(true)
					once.Do(func() { close(entered) })
					<-release
					out, err := seq.call(verb, body)
					return out, err
				}
				if verb == images.VerbStatus && !acting.Load() {
					taskStatus.Add(1)
				}
				return seq.call(verb, body)
			}
			poll(a, c, 0)
			c.mu.Lock()
			c.t = c.t.Add(15 * time.Second)
			c.mu.Unlock()
			supDone := make(chan struct{})
			go func() { a.superviseOnce(context.Background()); close(supDone) }()
			<-entered
			resCh := make(chan protocol.TaskResult, 1)
			start := time.Now()
			go func() {
				_, res, _ := a.execute(context.Background(), imageTask("image_status", []byte(`{"project":"acme","entry":"web"}`), tc.timeout))
				resCh <- res
			}()
			if tc.release {
				select {
				case <-resCh:
					t.Fatal("task ran during the action")
				case <-time.After(200 * time.Millisecond):
				}
				if taskStatus.Load() != 0 {
					t.Fatal("task reached the helper")
				}
				acting.Store(false)
				close(release)
				<-supDone
				select {
				case res := <-resCh:
					assertStatus(t, res, "succeeded", "succeeded")
				case <-time.After(5 * time.Second):
					t.Fatal("task did not run after the action")
				}
				return
			}
			select {
			case res := <-resCh:
				if res.State != "failed" || time.Since(start) < 900*time.Millisecond {
					t.Fatalf("%+v after %s", res, time.Since(start))
				}
				assertStatus(t, res, "failed", "failed")
			case <-time.After(5 * time.Second):
				t.Fatal("task did not time out")
			}
			if taskStatus.Load() != 0 {
				t.Fatal("timed-out task reached the helper")
			}
			close(release)
			<-supDone
		})
	}
}

func TestSupervisorNeedsHostLabel(t *testing.T) {
	a, c, _ := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
	a.opt.MachineID = t.TempDir() + "/missing"
	for i := 0; i < 5; i++ {
		poll(a, c, 15*time.Second)
	}
	if n := countVerb(a.opt.Helper.(*fakeHelper), images.VerbList); n != 0 {
		t.Fatalf("lists %d", n)
	}
}

func TestSupervisorErrorHasNullDigest(t *testing.T) {
	a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3})
	seq.set(func(s *superSeq) { s.failVerb = true })
	poll(a, c, 0)
	poll(a, c, 15*time.Second)
	q := a.autoq.list()
	if len(q) != 1 || q[0].Result != "error" || q[0].Digest != nil {
		t.Fatalf("%+v", q)
	}
	if countVerb(a.opt.Helper.(*fakeHelper), images.VerbStatus) != 0 {
		t.Fatal("status read after a failed call")
	}
}

// (c) Automatic rollback once, then gave_up; a deploy or rollback task
// allows one more; a bad_spec task does not reset.
func TestSupervisorRollbackReset(t *testing.T) {
	resets := []struct {
		name string
		kind string
		spec []byte
		want string
	}{
		{"image_deploy", "image_deploy", serviceSpec(false, 5, 3031), "image_unavailable"},
		{"image_rollback", "image_rollback", serviceRollbackSpec(), "image_unavailable"},
	}
	for _, tc := range resets {
		t.Run(tc.name, func(t *testing.T) {
			a, c, seq := superRun(t, images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 5})
			seq.set(func(s *superSeq) { s.prev = true })
			for i := 0; i < 6; i++ {
				poll(a, c, 15*time.Second)
			}
			if got := strings.Join(actions(a), ","); got != "rollback:ok,gave_up:ok" {
				t.Fatalf("%s", got)
			}
			// Rejected before it runs: no reset.
			bad := withRunPolicy(serviceSpec(false, 5, 3031), `{"on_unhealthy":"restart"}`)
			if _, _, ok := a.execute(context.Background(), imageTask("image_deploy", bad, 30)); ok {
				t.Fatal("bad spec accepted")
			}
			for i := 0; i < 4; i++ {
				poll(a, c, 15*time.Second)
			}
			if _, rb := seq.counts(); rb != 1 || len(a.autoq.list()) != 2 {
				t.Fatalf("after bad_spec: rollbacks %d frames %v", rb, actions(a))
			}
			assertDeploy(t, mustImage(t, a, tc.kind, tc.spec, 30), tc.want, "failed")
			for i := 0; i < 6; i++ {
				poll(a, c, 15*time.Second)
			}
			if _, rb := seq.counts(); rb != 2 {
				t.Fatalf("rollbacks %d", rb)
			}
			if got := strings.Join(actions(a), ","); got != "rollback:ok,gave_up:ok,rollback:ok,gave_up:ok" {
				t.Fatalf("%s", got)
			}
		})
	}
}

// (e) Two entries: web gives up while api keeps acting.
func TestSupervisorTwoEntries(t *testing.T) {
	a, h, clock, _ := imageRun(t)
	idPath, host := writeMachineID(t)
	a.opt.MachineID = idPath
	var mu sync.Mutex
	restarts := map[string]int{}
	d := imgDigest
	unhealthy := "unhealthy"
	item := func(entry string, max int) images.ContainerInfo {
		return images.ContainerInfo{
			ID: entry, Name: "/acme-" + entry, State: "running", Digest: &d, Health: &unhealthy,
			Labels: ownedTestLabels(host, "acme", entry),
			Policy: &images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: max},
		}
	}
	h.fn = func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case images.VerbList:
			return images.Encode(images.ListResponse{Items: []images.ContainerInfo{item("web", 1), item("api", 10)}}), nil
		case images.VerbRestart:
			var req images.EntryRequest
			if err := images.Decode(body, &req); err != nil {
				return nil, err
			}
			mu.Lock()
			restarts[req.Entry]++
			mu.Unlock()
			return images.Encode(images.ActionResult{Status: runningStatus(0)}), nil
		case images.VerbStatus:
			return images.Encode(images.StatusResult{Status: runningStatus(0), SpecHash: imgHash}), nil
		}
		return nil, errors.New(verb)
	}
	for i := 0; i < 10; i++ {
		poll(a, clock, 15*time.Second)
	}
	mu.Lock()
	web, api := restarts["web"], restarts["api"]
	mu.Unlock()
	if web != 1 || api != 5 {
		t.Fatalf("web %d api %d", web, api)
	}
	gave := map[string]int{}
	for _, f := range a.autoq.list() {
		if f.Action == "gave_up" {
			gave[f.Entry]++
		}
	}
	if gave["web"] != 1 || gave["api"] != 0 {
		t.Fatalf("gave_up %v", gave)
	}
}

// (b) Run wiring: images enabled starts the supervisor and its frame reaches
// the server; images disabled never lists containers.
func TestRunSupervisorWiring(t *testing.T) {
	t.Run("images enabled", func(t *testing.T) {
		clearAgentEnv(t)
		srv := fakeserver.Start(t)
		srv.Heartbeat = 120
		srv.Config.MetricsIntervalSec = 60
		srv.Config.ProcessIntervalSec = 300
		proc, sys, _ := fixtureProc(t, 9)
		idPath, host := writeMachineID(t)
		d := imgDigest
		unhealthy := "unhealthy"
		var restarts atomic.Int32
		helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
			switch verb {
			case "list":
				return programs.EncodeList(nil), nil
			case images.VerbList:
				it := images.ContainerInfo{
					ID: "a", Name: "acme-web", State: "running", Digest: &d, Health: &unhealthy,
					Labels: ownedTestLabels(host, "acme", "web"),
					Policy: &images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3},
				}
				return images.Encode(images.ListResponse{Items: []images.ContainerInfo{it}}), nil
			case images.VerbRestart:
				restarts.Add(1)
				return images.Encode(images.ActionResult{Status: runningStatus(0)}), nil
			case images.VerbStatus:
				return images.Encode(images.StatusResult{Status: runningStatus(0), SpecHash: imgHash}), nil
			}
			return nil, errors.New(verb)
		}}
		// The agent clock runs 60x faster so unhealthy_after_sec 15 passes in
		// a quarter second of wall time.
		start := time.Now()
		fast := func() time.Time { return start.Add(time.Since(start) * 60) }
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, Options{
				EnvFile: writeCredEnv(t, srv, t.TempDir()), ProcRoot: proc, SysRoot: sys,
				Passwd: joePasswd(t), GroupPath: joeGroup(t),
				PolicyPath: writeImageRunner(t), PolicyUID: rootUID,
				Helper: helper, MachineID: idPath, Now: fast,
				EUID: func() int { return 1000 }, DiscoverEvery: time.Hour,
				SuperviseEvery: 20 * time.Millisecond,
				Sleep:          func(context.Context, time.Duration) error { return nil },
				Log:            stdLog(&bytes.Buffer{}),
			})
		}()
		defer stopRun(t, cancel, errCh)
		if !srv.Wait(8*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "auto_action") >= 1 }) {
			t.Fatalf("frames %v restarts %d", frameTypes(srv.Frames()), restarts.Load())
		}
		var f protocol.AutoAction
		for _, fr := range srv.Frames() {
			if fr.Type == "auto_action" {
				if err := json.Unmarshal(fr.Raw, &f); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		if f.Action != "restart" || f.Result != "ok" || f.Entry != "web" || restarts.Load() < 1 {
			t.Fatalf("%+v restarts %d", f, restarts.Load())
		}
		if v := srv.Violations(); len(v) != 0 {
			t.Fatalf("violations %v", v)
		}
	})

	t.Run("images disabled", func(t *testing.T) {
		proc, sys, _ := fixtureProc(t, 9)
		var lists atomic.Int32
		helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
			switch verb {
			case "list":
				return programs.EncodeList(nil), nil
			case images.VerbList:
				lists.Add(1)
			}
			return nil, errors.New(verb)
		}}
		clearAgentEnv(t)
		srv := fakeserver.Start(t)
		srv.Heartbeat = 120
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, Options{
				EnvFile: writeCredEnv(t, srv, t.TempDir()), ProcRoot: proc, SysRoot: sys,
				Passwd: joePasswd(t), GroupPath: joeGroup(t),
				PolicyPath: writeRunner(t, true, false), PolicyUID: rootUID,
				Helper: helper, SuperviseEvery: 5 * time.Millisecond,
				EUID: func() int { return 1000 }, DiscoverEvery: time.Hour,
				Sleep: func(context.Context, time.Duration) error { return nil },
				Log:   stdLog(&bytes.Buffer{}),
			})
		}()
		if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
			cancel()
			t.Fatal("no hello")
		}
		// Forty supervisor intervals with the agent connected.
		time.Sleep(200 * time.Millisecond)
		stopRun(t, cancel, errCh)
		if n := lists.Load(); n != 0 {
			t.Fatalf("container_list calls %d", n)
		}
	})
}
