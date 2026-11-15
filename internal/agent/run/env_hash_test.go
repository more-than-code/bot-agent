package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// agent-protocol v1.14: env_hash in image_deploy and image_rollback.

const (
	envA = "1111111111111111111111111111111111111111111111111111111111111111"
	envB = "2222222222222222222222222222222222222222222222222222222222222222"
)

func withTaskEnvHash(spec []byte, v string) []byte {
	return []byte(strings.TrimSuffix(string(spec), "}") + `,"env_hash":"` + v + `"}`)
}

func deployedEnvHash(t *testing.T, body []byte) string {
	t.Helper()
	var req images.DeployRequest
	if err := images.Decode(body, &req); err != nil {
		t.Fatalf("deploy body: %v", err)
	}
	return req.EnvHash
}

func TestEnvHashUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		task    string // "" means the task has no env_hash
		label   string // the current container's env_hash label
		outcome string
	}{
		{"value change recreates", envB, envA, "succeeded"},
		{"same env_hash unchanged", envA, envA, "unchanged"},
		{"no label counts as changed", envA, "", "succeeded"},
		{"no env_hash in task ignores label", "", envA, "unchanged"},
		{"no env_hash and no label", "", "", "unchanged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, seq := imageRun(t)
			seq.pre = runningStatus(0)
			seq.preHash = imgHash
			seq.preEnv = tc.label
			seq.post = runningStatus(0)
			spec := serviceSpec(false, 5, 3031)
			if tc.task != "" {
				spec = withTaskEnvHash(spec, tc.task)
			}
			res := mustImage(t, a, "image_deploy", spec, 120)
			assertDeploy(t, res, tc.outcome, "succeeded")
			if seq.deployed != (tc.outcome == "succeeded") {
				t.Fatalf("deployed %v", seq.deployed)
			}
			if seq.deployed {
				if got := deployedEnvHash(t, seq.deployBody); got != tc.task {
					t.Fatalf("container_deploy env_hash %q want %q", got, tc.task)
				}
			}
		})
	}

	t.Run("no env_hash keeps the v1.9 deploy body", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if strings.Contains(string(seq.deployBody), "env_hash") {
			t.Fatalf("%s", seq.deployBody)
		}
	})

	t.Run("job carries env_hash", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(0)
		res := mustImage(t, a, "image_deploy", withTaskEnvHash(jobSpec(10), envA), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if got := deployedEnvHash(t, seq.deployBody); got != envA {
			t.Fatalf("env_hash %q", got)
		}
	})
}

func TestEnvHashRollback(t *testing.T) {
	prev := func(env string) *images.PrevView {
		return &images.PrevView{Digest: protocolStr(imgDigest), SpecHash: imgHash, EnvHash: env}
	}
	cases := []struct {
		name  string
		task  string
		label string
		swap  bool
	}{
		{"matching label swaps kept", envA, envA, true},
		{"mismatching label creates by digest", envA, envB, false},
		{"kept without label creates by digest", envA, "", false},
		{"no env_hash in task swaps as before", "", envB, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			// Running with no spec_hash label: the pull path is never unchanged.
			seq.pre = runningStatus(0)
			seq.prev = prev(tc.label)
			seq.post = runningStatus(0)
			spec := serviceRollbackSpec()
			if tc.task != "" {
				spec = withTaskEnvHash(spec, tc.task)
			}
			res := mustImage(t, a, "image_rollback", spec, 120)
			m := assertDeploy(t, res, "succeeded", "succeeded")
			if dig(m) != imgDigest {
				t.Fatalf("%v", m)
			}
			calls := strings.Join(h.Calls(), " ")
			if tc.swap {
				if !strings.Contains(calls, images.VerbRollback) || seq.deployed || seq.pullRef != "" {
					t.Fatalf("calls %s deployed %v pull %q", calls, seq.deployed, seq.pullRef)
				}
				return
			}
			if strings.Contains(calls, images.VerbRollback) || !seq.deployed || seq.pullRef != "ghcr.io/acme/web@"+imgDigest {
				t.Fatalf("calls %s deployed %v pull %q", calls, seq.deployed, seq.pullRef)
			}
			if got := deployedEnvHash(t, seq.deployBody); got != tc.task {
				t.Fatalf("env_hash %q", got)
			}
		})
	}
}

func TestEnvHashMalformedRefused(t *testing.T) {
	for _, kind := range []string{"image_deploy", "image_rollback"} {
		for _, v := range []string{"", "zz", strings.Repeat("A", 64), envA[:63], envA + "1"} {
			a, h, _, _ := imageRun(t)
			spec := serviceSpec(false, 5, 3031)
			if kind == "image_rollback" {
				spec = serviceRollbackSpec()
			}
			rej, _, ok := a.execute(context.Background(), imageTask(kind, withTaskEnvHash(spec, v), 120))
			if ok || rej.ErrorCode != "bad_spec" {
				t.Fatalf("%s %q: ok %v code %s", kind, v, ok, rej.ErrorCode)
			}
			if len(h.Calls()) != 0 {
				t.Fatalf("%s %q: helper called %v", kind, v, h.Calls())
			}
		}
	}
}

// v1.6.1 still applies with env_hash: a matching label is unchanged only
// when the healthcheck spec's container_list health is healthy.
func TestEnvHashUnchangedNeedsHealthy(t *testing.T) {
	healthy, unhealthy := "healthy", "unhealthy"
	cases := []struct {
		name   string
		health *string
		keep   bool
	}{
		{"matching label healthy", &healthy, true},
		{"matching label unhealthy", &unhealthy, false},
		{"matching label null health", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			seq.pre = runningStatus(0)
			seq.preHash = imgHash
			seq.preEnv = envA
			seq.post = runningStatus(0)
			seq.health = tc.health
			res := mustImage(t, a, "image_deploy", withTaskEnvHash(specWithHealth(false, hcFast), envA), 4)
			if tc.keep {
				assertDeploy(t, res, "unchanged", "succeeded")
				if seq.deployed || seq.listN != 1 {
					t.Fatalf("deployed %v lists %d", seq.deployed, seq.listN)
				}
				return
			}
			m := assertDeploy(t, res, "rolled_back", "failed")
			if !seq.deployed || !h.saw(images.VerbRollback) {
				t.Fatalf("outcome %v deployed %v calls %v", m["outcome"], seq.deployed, h.Calls())
			}
			if got := deployedEnvHash(t, seq.deployBody); got != envA {
				t.Fatalf("env_hash %q", got)
			}
		})
	}
}

// Contract v1.14 item 7: the supervisor's automatic rollback has no task
// env_hash and still swaps -prev back, whatever that container's label.
func TestEnvHashAutoRollbackIgnoresLabel(t *testing.T) {
	b := prevDigestB
	env := digestEnv{policy: images.ContainerPolicy{OnUnhealthy: "rollback", UnhealthyAfterSec: 15, MaxActionsPerHour: 3}, prev: true, prevDig: &b}
	a, c := digestRun(t, &env)
	labels := ownedTestLabels(env.host, "acme", "web")
	labels[images.LabelEnvHash] = envB
	env.prevLabels = labels
	poll(a, c, 0)
	poll(a, c, 15*time.Second)
	if got := strings.Join(actions(a), ","); got != "rollback:ok" {
		t.Fatalf("%s", got)
	}
	if env.rollbacks != 1 {
		t.Fatalf("rollback calls %d", env.rollbacks)
	}
}
