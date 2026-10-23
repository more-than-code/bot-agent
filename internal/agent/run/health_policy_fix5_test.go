package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// agent-protocol v1.9 rev 3: the helper call must outlast the helper's
// stop grace, so a container that ignores SIGTERM is restarted in time.
func TestActionBudgetDefaults(t *testing.T) {
	grace := time.Duration(images.StopGraceSec) * time.Second
	if actionCallTimeout != 60*time.Second || actionStatusTimeout != 5*time.Second {
		t.Fatalf("call %s status %s", actionCallTimeout, actionStatusTimeout)
	}
	if actionCallTimeout <= grace {
		t.Fatalf("call budget %s is not longer than the %s stop grace", actionCallTimeout, grace)
	}
	if actionCallTimeout+actionStatusTimeout != 65*time.Second {
		t.Fatalf("total %s", actionCallTimeout+actionStatusTimeout)
	}
}

// An action whose helper call runs out of time is reported as error; the
// helper has finished it and the container is still running, and the next
// unhealthy streak triggers the next action.
func TestSupervisorContinuesAfterErroredAction(t *testing.T) {
	shrinkBudget(t, 50*time.Millisecond, 20*time.Millisecond)
	calls := 0
	env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}}
	env.onAction = func(ctx context.Context) {
		calls++
		if calls == 1 {
			<-ctx.Done() // the stop outlasts the call; the fake list still shows it running
		}
	}
	a, c := digestRun(t, &env)
	poll(a, c, 0)
	poll(a, c, 15*time.Second) // first action: error
	if got := strings.Join(actions(a), ","); got != "restart:error" {
		t.Fatalf("%s", got)
	}
	poll(a, c, 15*time.Second) // a new streak starts after the action
	if calls != 1 {
		t.Fatalf("acted before the threshold: %d", calls)
	}
	poll(a, c, 15*time.Second)
	if got := strings.Join(actions(a), ","); got != "restart:error,restart:ok" {
		t.Fatalf("%s", got)
	}
	if q := a.autoq.list(); q[0].Digest != nil || q[1].Digest == nil || *q[1].Digest != imgDigest {
		t.Fatalf("digests %v %v", deref(q[0].Digest), deref(q[1].Digest))
	}
}

// sigtermIgnorer models a container whose PID 1 ignores SIGTERM: a restart
// only completes when the helper call has more than the stop grace left.
// With less, the call is cut off mid-stop and the container is left
// exited, as on pi3-2 with the pre-rev-3 25 s budget. The supervisor
// never acts on an exited container, so no second action would follow.
type sigtermIgnorer struct {
	mu       sync.Mutex
	host     string
	state    string
	restarts int
	cutOff   int
}

func (s *sigtermIgnorer) call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := imgDigest
	unhealthy := "unhealthy"
	switch verb {
	case images.VerbList:
		it := images.ContainerInfo{
			ID: "a", Name: "/acme-web", State: s.state, Digest: &d, Health: &unhealthy,
			Labels: ownedTestLabels(s.host, "acme", "web"),
			Policy: &images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3},
		}
		return images.Encode(images.ListResponse{Items: []images.ContainerInfo{it}}), nil
	case images.VerbRestart:
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) <= time.Duration(images.StopGraceSec)*time.Second {
			s.cutOff++
			s.state = "exited"
			return nil, context.DeadlineExceeded
		}
		s.restarts++
		s.state = "running"
		return images.Encode(images.ActionResult{Status: runningStatus(0)}), nil
	case images.VerbStatus:
		st := runningStatus(0)
		if s.state != "running" {
			st = exitedStatus(137)
		}
		return images.Encode(images.StatusResult{Status: st, SpecHash: imgHash}), nil
	}
	return nil, errors.New(verb)
}

// With the production budgets, two unhealthy streaks give two completed
// restarts. With the pre-rev-3 25 s call budget this test fails: the first
// restart is cut off, the container stays exited and nothing follows.
func TestSupervisorRestartsSigtermIgnorerTwice(t *testing.T) {
	a, _, clock, _ := imageRun(t)
	idPath, host := writeMachineID(t)
	a.opt.MachineID = idPath
	fake := &sigtermIgnorer{host: host, state: "running"}
	a.opt.Helper = &ctxHelper{fn: fake.call}
	poll(a, clock, 0)
	poll(a, clock, 15*time.Second) // first action
	poll(a, clock, 15*time.Second) // new streak
	poll(a, clock, 15*time.Second) // second action
	fake.mu.Lock()
	restarts, cut, st := fake.restarts, fake.cutOff, fake.state
	fake.mu.Unlock()
	if restarts != 2 || cut != 0 || st != "running" {
		t.Fatalf("restarts %d cut off %d state %s frames %v", restarts, cut, st, actions(a))
	}
	if got := strings.Join(actions(a), ","); got != "restart:ok,restart:ok" {
		t.Fatalf("%s", got)
	}
}
