package run

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// agent-protocol v1.16: a restart loop fails every health wait, and the
// wait leaves the last 60 s of the task budget for the failure path.

// restartAfter makes the new container report restart_count 1 from the
// (n+1)th container_status read after container_deploy.
func restartAfter(seq *imageSeq, n int) func(string, []byte) ([]byte, error) {
	polls := 0
	return func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbStatus && seq.deployed {
			polls++
			if polls > n {
				seq.post = runningStatus(1)
			}
		}
		return seq.call(verb, body)
	}
}

// closedPort is a local port nothing listens on, so a tcp probe is refused.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func failingCheck(port int) []protocol.HealthCheck {
	return []protocol.HealthCheck{{
		ID: "up", Service: "web", Protocol: "tcp", Port: port, IntervalSec: 60,
	}}
}

func TestRestartLoopFailsProbeWait(t *testing.T) {
	t.Run("old container running rolls back at the first restart", func(t *testing.T) {
		port := closedPort(t)
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		h.fn = restartAfter(seq, 1)
		a.configured = failingCheck(port)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 1 || !clock.now().Equal(start.Add(imagePoll)) || seq.rollbackStarts != 1 || seq.stops != 0 {
			t.Fatalf("sleeps %d now %s rolls %d stops %d calls %v", clock.sleeps(), clock.now(), seq.rollbackStarts, seq.stops, h.Calls())
		}
	})

	t.Run("first deploy stops the new container", func(t *testing.T) {
		port := closedPort(t)
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		h.fn = restartAfter(seq, 1)
		a.configured = failingCheck(port)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 300)
		assertDeploy(t, res, "failed", "failed")
		if clock.sleeps() != 1 || seq.rollbackStarts != 0 || seq.stops != 1 {
			t.Fatalf("sleeps %d rolls %d stops %d calls %v", clock.sleeps(), seq.rollbackStarts, seq.stops, h.Calls())
		}
	})

	t.Run("restarting with restart_count 0", func(t *testing.T) {
		port := closedPort(t)
		a, h, clock, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = stateStatus("restarting")
		a.configured = failingCheck(port)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 0 || seq.rollbackStarts != 1 {
			t.Fatalf("sleeps %d rolls %d calls %v", clock.sleeps(), seq.rollbackStarts, h.Calls())
		}
	})
}

func TestRestartLoopFailsDockerWait(t *testing.T) {
	starting := "starting"
	healthy := "healthy"

	t.Run("old container running rolls back at the first restart", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		seq.health = &starting
		h.fn = restartAfter(seq, 1)
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 1 || !clock.now().Equal(start.Add(imagePoll)) || seq.listN != 2 || seq.rollbackStarts != 1 || seq.stops != 0 {
			t.Fatalf("sleeps %d now %s lists %d rolls %d stops %d calls %v", clock.sleeps(), clock.now(), seq.listN, seq.rollbackStarts, seq.stops, h.Calls())
		}
	})

	t.Run("first deploy stops the new container", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		seq.health = &starting
		h.fn = restartAfter(seq, 1)
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
		assertDeploy(t, res, "failed", "failed")
		if clock.sleeps() != 1 || seq.rollbackStarts != 0 || seq.stops != 1 {
			t.Fatalf("sleeps %d rolls %d stops %d calls %v", clock.sleeps(), seq.rollbackStarts, seq.stops, h.Calls())
		}
	})

	t.Run("healthy but restarting is not healthy", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = stateStatus("restarting")
		seq.health = &healthy
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 0 || seq.rollbackStarts != 1 {
			t.Fatalf("sleeps %d rolls %d calls %v", clock.sleeps(), seq.rollbackStarts, h.Calls())
		}
	})

	t.Run("status error does not roll back", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		seq.health = &starting
		seq.waitErr = true
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
		assertDeploy(t, res, "failed", "failed")
		if seq.rollbackStarts != 0 || seq.stops != 0 || seq.listN != 1 {
			t.Fatalf("rolls %d stops %d lists %d calls %v", seq.rollbackStarts, seq.stops, seq.listN, h.Calls())
		}
	})
}

// TestFailurePathReserve runs on the wall clock with a 61 s budget, so the
// wait has 1 s and the failure path 60 s. The sleeper blocks until its
// context ends, as the real one does. fakeHelper returns ctx.Err() on a
// done context, as socketHelper does. Before v1.16 the wait ran on the
// task context: it ended only at the task deadline (61 s), and rollback,
// stop and log tail then failed on the expired context.
func TestFailurePathReserve(t *testing.T) {
	const budget = 61
	run := func(t *testing.T, seq *imageSeq, a *agent) (protocol.TaskResult, time.Duration) {
		t.Helper()
		a.opt.Now = time.Now
		a.opt.Sleep = func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		}
		a.configured = failingCheck(closedPort(t))
		seq.post = runningStatus(0)
		seq.logs = []string{"boom"}
		begin := time.Now()
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, a.configured[0].Port), budget)
		return res, time.Since(begin)
	}

	t.Run("rollback completes", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		res, took := run(t, seq, a)
		assertDeploy(t, res, "rolled_back", "failed")
		if seq.rollbackStarts != 1 || len(seq.logReqs) == 0 {
			t.Fatalf("rolls %d logs %d calls %v", seq.rollbackStarts, len(seq.logReqs), h.Calls())
		}
		if took >= budget*time.Second-failReserve/2 {
			t.Fatalf("took %s", took)
		}
	})

	t.Run("stop completes", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		res, took := run(t, seq, a)
		assertDeploy(t, res, "failed", "failed")
		if seq.stops != 1 || len(seq.logReqs) == 0 {
			t.Fatalf("stops %d logs %d calls %v", seq.stops, len(seq.logReqs), h.Calls())
		}
		if took >= budget*time.Second-failReserve/2 {
			t.Fatalf("took %s", took)
		}
	})
}

func TestWaitBudget(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{
		{15 * time.Second, 15 * time.Second},
		{60 * time.Second, 60 * time.Second},
		{61 * time.Second, time.Second},
		{420 * time.Second, 360 * time.Second},
	} {
		if got := waitBudget(tc.in); got != tc.want {
			t.Fatalf("waitBudget(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestExitedFailsDockerWait: a service with restart "no" that crashes is
// exited in container_status while container_list may still show the last
// health. The Docker wait ends at once, as the other two waits do.
func TestExitedFailsDockerWait(t *testing.T) {
	starting := "starting"
	healthy := "healthy"
	for _, tc := range []struct {
		name    string
		post    images.ImageStatus
		health  *string
		pre     images.ImageStatus
		outcome string
		rolls   int
		stops   int
	}{
		{"exited, old running rolls back", exitedStatus(1), &starting, runningStatus(0), "rolled_back", 1, 0},
		{"exited, first deploy stops", exitedStatus(1), &starting, missingStatus(), "failed", 0, 1},
		{"exited with a stale healthy list", exitedStatus(1), &healthy, runningStatus(0), "rolled_back", 1, 0},
		{"dead", stateStatus("dead"), &starting, runningStatus(0), "rolled_back", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h, clock, seq := imageRun(t)
			seq.pre = tc.pre
			seq.preHash = strings.Repeat("ab", 32)
			seq.post = tc.post
			seq.health = tc.health
			res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
			assertDeploy(t, res, tc.outcome, "failed")
			if clock.sleeps() != 0 || seq.listN != 1 || seq.rollbackStarts != tc.rolls || seq.stops != tc.stops {
				t.Fatalf("sleeps %d lists %d rolls %d stops %d calls %v", clock.sleeps(), seq.listN, seq.rollbackStarts, seq.stops, h.Calls())
			}
		})
	}
}

// TestDockerWaitStatusRefusal: a container_status refusal during the Docker
// wait returns that code. It neither rolls back nor stops.
func TestDockerWaitStatusRefusal(t *testing.T) {
	starting := "starting"
	for _, tc := range []struct{ code, outcome string }{
		{"docker_unavailable", "failed"},
		{"conflict", "conflict"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			a, h, clock, seq := imageRun(t)
			seq.pre = runningStatus(0)
			seq.preHash = strings.Repeat("ab", 32)
			seq.post = runningStatus(0)
			seq.health = &starting
			seq.waitCode = tc.code
			res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
			assertDeploy(t, res, tc.outcome, "failed")
			if clock.sleeps() != 0 || seq.listN != 1 || seq.rollbackStarts != 0 || seq.stops != 0 {
				t.Fatalf("sleeps %d lists %d rolls %d stops %d calls %v", clock.sleeps(), seq.listN, seq.rollbackStarts, seq.stops, h.Calls())
			}
		})
	}
}

// TestRestartLoopFailsKeptSwap: image_rollback swaps the kept container back
// (rollbackKept). A restart loop fails that wait at once, and the result
// carries the kept digest and the log tail. There is no older generation,
// so nothing else is rolled back.
func TestRestartLoopFailsKeptSwap(t *testing.T) {
	healthy := "healthy"
	for _, tc := range []struct {
		name   string
		spec   []byte
		after  images.ImageStatus
		health *string
	}{
		{"stable wait", serviceRollbackSpec(), runningStatus(1), nil},
		{"stable wait restarting", serviceRollbackSpec(), stateStatus("restarting"), nil},
		{"docker wait healthy but restarted", dockerRollbackSpec(hcFast), runningStatus(1), &healthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h, clock, seq := imageRun(t)
			seq.prev = prevDigest(imgDigest)
			// The fake reports pre until a container_deploy, so after the
			// swap container_status reads tc.after.
			seq.pre = tc.after
			seq.health = tc.health
			seq.logs = []string{"crash"}
			res := mustImage(t, a, "image_rollback", tc.spec, 300)
			m := assertDeploy(t, res, "failed", "failed")
			if dig(m) != imgDigest || clock.sleeps() != 0 || seq.rollbackStarts != 1 || seq.pullRef != "" || seq.deployed || len(seq.logReqs) != 1 {
				t.Fatalf("%v sleeps %d rolls %d ref %q logs %d calls %v", m, clock.sleeps(), seq.rollbackStarts, seq.pullRef, len(seq.logReqs), h.Calls())
			}
			if lines, _ := m["lines"].([]any); len(lines) != 1 || lines[0] != "crash" {
				t.Fatalf("lines %v", m["lines"])
			}
		})
	}
}

// TestRollbackPullReserve: on the image_rollback pull path, a pull that runs
// past budget - 60 s leaves the wait no time. The wait ends at once and the
// rollback runs in the reserve.
func TestRollbackPullReserve(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec []byte
	}{
		{"stable wait", serviceRollbackSpec()},
		{"docker wait", dockerRollbackSpec(hcFast)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h, clock, seq := imageRun(t)
			start := clock.now()
			seq.prev = prevDigest(otherDigest)
			seq.pre = runningStatus(0)
			seq.preHash = strings.Repeat("ab", 32)
			seq.post = runningStatus(0)
			h.fn = func(verb string, body []byte) ([]byte, error) {
				if verb == images.VerbPull {
					clock.mu.Lock()
					clock.t = clock.t.Add(70 * time.Second)
					clock.mu.Unlock()
				}
				return seq.call(verb, body)
			}
			res := mustImage(t, a, "image_rollback", tc.spec, 120)
			assertDeploy(t, res, "rolled_back", "failed")
			// Status reads: rollbackKept, deployService, and after the
			// rollback. None from the wait.
			if clock.sleeps() != 0 || !clock.now().Equal(start.Add(70*time.Second)) || seq.listN != 0 ||
				countVerb(h, images.VerbStatus) != 3 || seq.rollbackStarts != 1 || len(seq.logReqs) != 1 {
				t.Fatalf("sleeps %d now %s lists %d rolls %d logs %d calls %v", clock.sleeps(), clock.now(), seq.listN, seq.rollbackStarts, len(seq.logReqs), h.Calls())
			}
		})
	}
}
