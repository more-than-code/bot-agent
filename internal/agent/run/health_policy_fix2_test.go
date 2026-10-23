package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const prevDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// ctxHelper is a helper fake that sees the call context.
type ctxHelper struct {
	fn func(ctx context.Context, verb string, body []byte) ([]byte, error)
}

func (h *ctxHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	return h.fn(ctx, verb, body)
}

// digestEnv is one owned acme-web (digest imgDigest), optionally an
// acme-web-prev with prevDig, an action verb that succeeds or fails, and a
// status read that succeeds or fails.
type digestEnv struct {
	host       string
	policy     images.ContainerPolicy
	prev       bool
	prevDig    *string
	present    func() bool
	actionFail bool
	statusFail bool
	// statusStopped reads back an exited container; statusNoDigest a
	// running one without a digest.
	statusStopped  bool
	statusNoDigest bool
	// prevLabels, when set, replaces the -prev container's labels.
	prevLabels map[string]string
	rollbacks  int
	onAction   func(ctx context.Context)
	onStatus   func(ctx context.Context)
}

func (e *digestEnv) call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	d := imgDigest
	unhealthy := "unhealthy"
	switch verb {
	case images.VerbList:
		if e.present != nil && !e.present() {
			return images.Encode(images.ListResponse{Items: []images.ContainerInfo{}}), nil
		}
		labels := ownedTestLabels(e.host, "acme", "web")
		pol := e.policy
		items := []images.ContainerInfo{{ID: "a", Name: "/acme-web", State: "running", Digest: &d, Health: &unhealthy, Labels: labels, Policy: &pol}}
		if e.prev {
			pl := labels
			if e.prevLabels != nil {
				pl = e.prevLabels
			}
			items = append(items, images.ContainerInfo{ID: "b", Name: "/acme-web-prev", State: "exited", Digest: e.prevDig, Labels: pl, Policy: &pol})
		}
		return images.Encode(images.ListResponse{Items: items}), nil
	case images.VerbRestart, images.VerbRollback:
		if verb == images.VerbRollback {
			e.rollbacks++
		}
		if e.onAction != nil {
			e.onAction(ctx)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if e.actionFail {
			return []byte(`{"error":"docker_unavailable"}`), nil
		}
		if verb == images.VerbRestart {
			return images.Encode(images.ActionResult{Status: runningStatus(0)}), nil
		}
		return images.Encode(images.IDResponse{ContainerID: "b"}), nil
	case images.VerbStatus:
		if e.onStatus != nil {
			e.onStatus(ctx)
		}
		if e.statusFail {
			return nil, errors.New("status")
		}
		if e.statusStopped {
			return images.Encode(images.StatusResult{Status: exitedStatus(0), SpecHash: imgHash}), nil
		}
		if e.statusNoDigest {
			st := runningStatus(0)
			st.Digest = nil
			return images.Encode(images.StatusResult{Status: st, SpecHash: imgHash}), nil
		}
		return images.Encode(images.StatusResult{Status: runningStatus(0), SpecHash: imgHash}), nil
	}
	return nil, errors.New(verb)
}

func digestRun(t *testing.T, e *digestEnv) (*agent, *testClock) {
	t.Helper()
	a, _, clock, _ := imageRun(t)
	idPath, host := writeMachineID(t)
	a.opt.MachineID = idPath
	e.host = host
	a.opt.Helper = &ctxHelper{fn: e.call}
	return a, clock
}

func onlyFrameDigest(t *testing.T, a *agent, action, result string) *string {
	t.Helper()
	q := a.autoq.list()
	if len(q) != 1 || q[0].Action != action || q[0].Result != result {
		t.Fatalf("%v", actions(a))
	}
	return q[0].Digest
}

func TestAutoActionKnownDigest(t *testing.T) {
	b := prevDigestB
	cases := []struct {
		name   string
		env    digestEnv
		action string
		result string
		want   *string
	}{
		{"rollback ok, status fails: -prev digest", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b, statusFail: true}, "rollback", "ok", &b},
		{"restart ok, status fails: current digest", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, statusFail: true}, "restart", "ok", protoStr(imgDigest)},
		{"restart error: null", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, actionFail: true}, "restart", "error", nil},
		{"rollback error: null", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b, actionFail: true}, "rollback", "error", nil},
		{"prev digest unknown, status fails: null", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, statusFail: true}, "rollback", "ok", nil},
		{"status ok, nothing running: null", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b, statusStopped: true}, "rollback", "ok", nil},
		{"status ok, running without digest: known", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b, statusNoDigest: true}, "rollback", "ok", &b},
		{"restart, running without digest: current", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, statusNoDigest: true}, "restart", "ok", protoStr(imgDigest)},
		{"status ok wins over known", digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b}, "rollback", "ok", protoStr(imgDigest)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			var statusReads int
			env.onStatus = func(context.Context) { statusReads++ }
			a, c := digestRun(t, &env)
			poll(a, c, 0)
			poll(a, c, 15*time.Second)
			got := onlyFrameDigest(t, a, tc.action, tc.result)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("digest %v want %v", deref(got), deref(tc.want))
			}
			if tc.result == "error" && statusReads != 0 {
				t.Fatal("status read after a failed call")
			}
		})
	}
}

func protoStr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func shrinkBudget(t *testing.T, call, status time.Duration) {
	t.Helper()
	oldCall, oldStatus := actionCallTimeout, actionStatusTimeout
	actionCallTimeout, actionStatusTimeout = call, status
	t.Cleanup(func() { actionCallTimeout, actionStatusTimeout = oldCall, oldStatus })
}

// Test A: the helper call is bounded by actionCallTimeout.
func TestActionCallBudget(t *testing.T) {
	shrinkBudget(t, 50*time.Millisecond, 20*time.Millisecond)
	var dl, hookStart time.Time
	var hasDL bool
	var ctxErr error
	env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}}
	env.onAction = func(ctx context.Context) {
		hookStart = time.Now()
		dl, hasDL = ctx.Deadline()
		<-ctx.Done()
		ctxErr = ctx.Err()
	}
	a, c := digestRun(t, &env)
	poll(a, c, 0)
	before := time.Now()
	poll(a, c, 15*time.Second)
	if !hasDL {
		t.Fatal("no deadline on the action call")
	}
	if dl.Sub(before) < actionCallTimeout || dl.Sub(hookStart) > actionCallTimeout {
		t.Fatalf("deadline %s after poll, %s after call start", dl.Sub(before), dl.Sub(hookStart))
	}
	if !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Fatalf("ctx err %v", ctxErr)
	}
	if d := onlyFrameDigest(t, a, "restart", "error"); d != nil {
		t.Fatalf("digest %s", *d)
	}
}

// Test B: the status read gets a fresh actionStatusTimeout, not what the
// call left over.
func TestActionStatusBudget(t *testing.T) {
	// The call uses most of its budget, so a fresh status deadline lands
	// after the call deadline; a shared context could not. 400 ms of slack
	// keeps the sleep inside the call budget under load.
	shrinkBudget(t, time.Second, 500*time.Millisecond)
	var callDL, statusDL, statusStart time.Time
	var callOK, statusOK bool
	var statusErr error
	env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}}
	env.onAction = func(ctx context.Context) {
		callDL, callOK = ctx.Deadline()
		time.Sleep(600 * time.Millisecond)
	}
	env.onStatus = func(ctx context.Context) {
		statusStart = time.Now()
		statusDL, statusOK = ctx.Deadline()
		statusErr = ctx.Err()
	}
	a, c := digestRun(t, &env)
	poll(a, c, 0)
	poll(a, c, 15*time.Second)
	if !callOK || !statusOK {
		t.Fatalf("deadlines call %v status %v", callOK, statusOK)
	}
	if statusErr != nil {
		t.Fatalf("status ctx already done: %v", statusErr)
	}
	if !statusDL.After(callDL) {
		t.Fatalf("status deadline %s not after call deadline %s", statusDL, callDL)
	}
	if statusDL.Sub(statusStart) > actionStatusTimeout {
		t.Fatalf("status deadline %s after read start", statusDL.Sub(statusStart))
	}
	if d := onlyFrameDigest(t, a, "restart", "ok"); d == nil || *d != imgDigest {
		t.Fatalf("digest %v", deref(d))
	}
}

// A container on the -prev name counts only with the entry's owner,
// project, entry and host labels.
func TestSupervisorPrevNeedsLabels(t *testing.T) {
	b := prevDigestB
	mutate := func(host, key, val string) map[string]string {
		l := ownedTestLabels(host, "acme", "web")
		if val == "" {
			delete(l, key)
		} else {
			l[key] = val
		}
		return l
	}
	cases := []struct {
		name   string
		labels func(host string) map[string]string
		want   string
	}{
		{"owned", nil, "rollback:ok"},
		{"wrong owner", func(h string) map[string]string { return mutate(h, images.LabelOwner, "someone") }, "gave_up:ok"},
		{"no owner", func(h string) map[string]string { return mutate(h, images.LabelOwner, "") }, "gave_up:ok"},
		{"wrong host", func(h string) map[string]string { return mutate(h, images.LabelHost, "otherhost") }, "gave_up:ok"},
		{"wrong entry", func(h string) map[string]string { return mutate(h, images.LabelEntry, "api") }, "gave_up:ok"},
		{"wrong project", func(h string) map[string]string { return mutate(h, images.LabelProject, "other") }, "gave_up:ok"},
		{"no labels", func(string) map[string]string { return map[string]string{} }, "gave_up:ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b}
			a, c := digestRun(t, &env)
			if tc.labels != nil {
				env.prevLabels = tc.labels(env.host)
			}
			poll(a, c, 0)
			poll(a, c, 15*time.Second)
			if got := strings.Join(actions(a), ","); got != tc.want {
				t.Fatalf("%s", got)
			}
			wantRB := 0
			if tc.want == "rollback:ok" {
				wantRB = 1
			}
			if env.rollbacks != wantRB {
				t.Fatalf("rollback calls %d", env.rollbacks)
			}
			if tc.want == "gave_up:ok" {
				// gave_up carries the current container's digest, never the squatter's.
				if d := a.autoq.list()[0].Digest; d == nil || *d != imgDigest {
					t.Fatalf("digest %v", deref(d))
				}
			}
		})
	}
}

// An entry absent from one poll restarts its streak when it reappears.
func TestSupervisorStreakRestartsAfterAbsence(t *testing.T) {
	n := 0
	env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 30, MaxActionsPerHour: 3}}
	env.present = func() bool { n++; return n != 2 }
	a, c := digestRun(t, &env)
	poll(a, c, 0)              // unhealthy, streak starts
	poll(a, c, 15*time.Second) // absent
	poll(a, c, 15*time.Second) // back: streak restarts here (t=30)
	poll(a, c, 15*time.Second) // 15 s into the new streak
	poll(a, c, 14*time.Second) // 29 s
	if len(a.autoq.list()) != 0 {
		t.Fatalf("acted early: %v", actions(a))
	}
	poll(a, c, time.Second) // 30 s after the reappearance
	if got := strings.Join(actions(a), ","); got != "restart:ok" {
		t.Fatalf("%s", got)
	}
}
