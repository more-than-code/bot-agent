package run

import (
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

func TestUnchangedRequiresHealthy(t *testing.T) {
	healthy := "healthy"
	unhealthy := "unhealthy"
	starting := "starting"
	cases := []struct {
		name   string
		health *string
		check  bool
		keep   bool
	}{
		{"healthy with check", &healthy, true, true},
		{"unhealthy with check", &unhealthy, true, false},
		{"starting with check", &starting, true, false},
		{"null with check", nil, true, false},
		{"healthy without check", &healthy, false, true},
		{"unhealthy without check", &unhealthy, false, true},
		{"starting without check", &starting, false, true},
		{"null without check", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			seq.pre = runningStatus(0)
			seq.preHash = imgHash
			seq.post = runningStatus(0)
			seq.health = tc.health
			spec := serviceSpec(false, 5, 3031)
			if tc.check {
				spec = specWithHealth(false, hcFast)
			}
			res := mustImage(t, a, "image_deploy", spec, 4)
			if tc.keep {
				assertDeploy(t, res, "unchanged", "succeeded")
				if seq.deployed {
					t.Fatal("deployed")
				}
				wantList := 0
				if tc.check {
					wantList = 1
				}
				if seq.listN != wantList {
					t.Fatalf("lists %d calls %v", seq.listN, h.Calls())
				}
				return
			}
			m := assertDeploy(t, res, "rolled_back", "failed")
			if m["outcome"] == "unchanged" || !seq.deployed || !h.saw(images.VerbRollback) || h.saw(images.VerbStop) || seq.listN < 1 {
				t.Fatalf("outcome %v deployed %v lists %d calls %v", m["outcome"], seq.deployed, seq.listN, h.Calls())
			}
		})
	}
}

func TestUnchangedHealthMisses(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.listErr = true
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
		assertDeploy(t, res, "failed", "failed")
		if seq.deployed || seq.listN != 1 || h.saw(images.VerbRollback) || h.saw(images.VerbStop) {
			t.Fatalf("deployed %v lists %d calls %v", seq.deployed, seq.listN, h.Calls())
		}
	})

	t.Run("row absent", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.listEmpty = true
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 4)
		assertDeploy(t, res, "rolled_back", "failed")
		if !seq.deployed || !h.saw(images.VerbRollback) || clock.sleeps() == 0 || !clock.now().After(start) {
			t.Fatalf("deployed %v sleeps %d calls %v", seq.deployed, clock.sleeps(), h.Calls())
		}
	})
}

func TestFirstDeployFailureStops(t *testing.T) {
	t.Run("unhealthy", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		bad := "unhealthy"
		seq.health = &bad
		seq.stopped = exitedStatus(7)
		seq.stoppedSet = true
		seq.logs = []string{"line sekret-env-VALUE-9f3"}
		spec := strings.Replace(string(specWithHealth(false, hcFast)), `"env":{}`, `"env":{"TOKEN":"sekret-env-VALUE-9f3"}`, 1)
		res := mustImage(t, a, "image_deploy", []byte(spec), 120)
		m := assertDeploy(t, res, "failed", "failed")
		st, _ := m["status"].(map[string]any)
		if st["state"] != "exited" || st["exit_code"] != float64(7) {
			t.Fatalf("status %v", st)
		}
		raw := string(marshalPayload(t, res.Payload))
		if strings.Contains(raw, "sekret-env-VALUE-9f3") || !strings.Contains(raw, "sekr\u2026") {
			t.Fatalf("lines %s", raw)
		}
		if !h.saw(images.VerbStop) || h.saw(images.VerbRollback) || h.saw(images.VerbRemove) || seq.stops != 1 {
			t.Fatalf("calls %v", h.Calls())
		}
	})

	t.Run("wait limit", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		starting := "starting"
		seq.health = &starting
		seq.stopped = exitedStatus(7)
		seq.stoppedSet = true
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 4)
		m := assertDeploy(t, res, "failed", "failed")
		st, _ := m["status"].(map[string]any)
		if st["state"] != "exited" || st["exit_code"] != float64(7) {
			t.Fatalf("status %v", st)
		}
		if clock.sleeps() != 5 || !clock.now().Equal(start.Add(10*time.Second)) || seq.stops != 1 || h.saw(images.VerbRollback) {
			t.Fatalf("sleeps %d now %s calls %v", clock.sleeps(), clock.now(), h.Calls())
		}
	})

	t.Run("no healthcheck", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(1)
		seq.stopped = exitedStatus(9)
		seq.stoppedSet = true
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		m := assertDeploy(t, res, "failed", "failed")
		st, _ := m["status"].(map[string]any)
		if st["exit_code"] != float64(9) || seq.stops != 1 || h.saw(images.VerbRollback) {
			t.Fatalf("status %v calls %v", st, h.Calls())
		}
	})

	t.Run("orphan prev", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.prev = prevDigest(otherDigest)
		seq.post = runningStatus(0)
		bad := "unhealthy"
		seq.health = &bad
		seq.stopped = exitedStatus(7)
		seq.stoppedSet = true
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
		m := assertDeploy(t, res, "failed", "failed")
		st, _ := m["status"].(map[string]any)
		if st["exit_code"] != float64(7) || !h.saw(images.VerbStop) || h.saw(images.VerbRollback) {
			t.Fatalf("status %v calls %v", st, h.Calls())
		}
	})
}

func TestFailedWaitRollsBackWhenCurrentExists(t *testing.T) {
	t.Run("rolled back", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		bad := "unhealthy"
		seq.health = &bad
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
		assertDeploy(t, res, "rolled_back", "failed")
		if !h.saw(images.VerbRollback) || h.saw(images.VerbStop) {
			t.Fatalf("calls %v", h.Calls())
		}
	})

	t.Run("rollback failed", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		bad := "unhealthy"
		seq.health = &bad
		seq.rollback = "conflict"
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
		assertDeploy(t, res, "rollback_failed", "failed")
		if !h.saw(images.VerbRollback) || h.saw(images.VerbStop) {
			t.Fatalf("calls %v", h.Calls())
		}
	})

	t.Run("image rollback still rolls back", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.prev = prevDigest(otherDigest)
		seq.post = runningStatus(0)
		bad := "unhealthy"
		seq.health = &bad
		body := strings.TrimSuffix(string(specWithHealth(false, hcFast)), "}") + `,"digest":"` + imgDigest + `"}`
		res := mustImage(t, a, "image_rollback", []byte(body), 120)
		assertDeploy(t, res, "rolled_back", "failed")
		if !h.saw(images.VerbRollback) || h.saw(images.VerbStop) {
			t.Fatalf("calls %v", h.Calls())
		}
	})

	t.Run("image rollback unchanged ignores health", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.prev = prevDigest(otherDigest)
		bad := "unhealthy"
		seq.health = &bad
		body := strings.TrimSuffix(string(specWithHealth(false, hcFast)), "}") + `,"digest":"` + imgDigest + `"}`
		res := mustImage(t, a, "image_rollback", []byte(body), 120)
		assertDeploy(t, res, "unchanged", "succeeded")
		if seq.deployed || seq.listN != 0 || h.saw(images.VerbRollback) {
			t.Fatalf("deployed %v lists %d calls %v", seq.deployed, seq.listN, h.Calls())
		}
	})
}

func TestSecondDeployDoesNotStartParkedContainer(t *testing.T) {
	a, h, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.preHash = imgHash
	seq.post = runningStatus(0)
	bad := "unhealthy"
	seq.health = &bad
	seq.stopped = exitedStatus(7)
	seq.stoppedSet = true
	spec := specWithHealth(false, hcFast)

	first := mustImage(t, a, "image_deploy", spec, 120)
	m := assertDeploy(t, first, "failed", "failed")
	st, _ := m["status"].(map[string]any)
	if st["state"] != "exited" || seq.stops != 1 || seq.rollbackStarts != 0 || h.saw(images.VerbRollback) {
		t.Fatalf("first status %v starts %d calls %v", st, seq.rollbackStarts, h.Calls())
	}

	second := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
	m = assertDeploy(t, second, "failed", "failed")
	st, _ = m["status"].(map[string]any)
	deploys, stops, rolls := 0, 0, 0
	for _, c := range h.Calls() {
		switch c {
		case images.VerbDeploy:
			deploys++
		case images.VerbStop:
			stops++
		case images.VerbRollback:
			rolls++
		}
	}
	if st["state"] != "exited" || deploys != 2 || stops != 2 || rolls != 0 || seq.rollbackStarts != 0 {
		t.Fatalf("second would start parked container: status %v deploys %d stops %d rolls %d starts %d", st, deploys, stops, rolls, seq.rollbackStarts)
	}
}

func TestExitedPreDeployStops(t *testing.T) {
	for _, state := range []string{"exited", "created", "dead", "paused"} {
		t.Run(state, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			pre := exitedStatus(1)
			pre.State = state
			if state != "exited" {
				pre.ExitCode = nil
			}
			seq.pre = pre
			seq.preHash = strings.Repeat("ab", 32)
			bad := "unhealthy"
			seq.health = &bad
			seq.stopped = exitedStatus(7)
			seq.stoppedSet = true
			res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 120)
			m := assertDeploy(t, res, "failed", "failed")
			st, _ := m["status"].(map[string]any)
			if st["exit_code"] != float64(7) || seq.stops != 1 || seq.rollbackStarts != 0 || h.saw(images.VerbRollback) || !h.saw(images.VerbStop) {
				t.Fatalf("status %v starts %d calls %v", st, seq.rollbackStarts, h.Calls())
			}
		})
	}
}

func specWithHealth(force bool, hc string) []byte {
	flag := "false"
	if force {
		flag = "true"
	}
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":3031,"container":3031,"public":false}],"healthy_after_sec":5,"healthcheck":` + hc + `},"env":{},"registry_auth":null,"force":` + flag + `,"spec_hash":"` + imgHash + `"}`)
}
