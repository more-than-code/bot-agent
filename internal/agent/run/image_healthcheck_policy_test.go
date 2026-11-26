package run

import (
	"bytes"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// TestImageHealthcheckPolicy covers agent-protocol v1.28: a service whose
// on_unhealthy acts and that has no healthcheck of its own deploys on the
// image's HEALTHCHECK; a container with no Docker health at all fails the
// wait with whyNoDockerHealth, and on_deploy_failure applies.
func TestImageHealthcheckPolicy(t *testing.T) {
	restart := `{"on_unhealthy":"restart"}`
	starting, healthy := "starting", "healthy"
	cases := []struct {
		name     string
		hp       string
		pre      string
		health   *string
		outcome  string
		status   string
		rollback bool
		stop     bool
		why      string
	}{
		{"no check, previous running", restart, "running", nil, "rolled_back", "failed", true, false, whyNoDockerHealth},
		{"no check, nothing before", restart, "missing", nil, "failed", "failed", false, true, whyNoDockerHealth},
		{"no check, on_deploy_failure stop", `{"on_unhealthy":"rollback","on_deploy_failure":"stop"}`, "running", nil, "failed", "failed", false, true, whyNoDockerHealth},
		{"image check starting", restart, "missing", &starting, "succeeded", "succeeded", false, false, ""},
		{"image check healthy", restart, "missing", &healthy, "succeeded", "succeeded", false, false, ""},
		// Without an action the check does not apply.
		{"no action, no check", `{"on_unhealthy":"none"}`, "missing", nil, "succeeded", "succeeded", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			if tc.pre == "running" {
				seq.pre = runningStatus(0)
				seq.preHash = strings.Repeat("ab", 32)
			} else {
				seq.pre = missingStatus()
			}
			seq.post = runningStatus(0)
			seq.health = tc.health
			spec := withRunPolicy(serviceSpec(false, 30, 8080), tc.hp)
			res := mustImage(t, a, "image_deploy", spec, 120)
			assertDeploy(t, res, tc.outcome, tc.status)
			if h.saw(images.VerbRollback) != tc.rollback || h.saw(images.VerbStop) != tc.stop {
				t.Fatalf("rollback %v stop %v calls %v", h.saw(images.VerbRollback), h.saw(images.VerbStop), h.Calls())
			}
			why, ok := reasonOf(t, res)
			if tc.why == "" {
				if ok {
					t.Fatalf("reason %q on success", why)
				}
				return
			}
			if why != tc.why {
				t.Fatalf("reason %q", why)
			}
		})
	}
}

// The entry's own healthcheck keeps the v1.9 Docker wait; the check does
// not run before it.
func TestImageHealthcheckPolicyOwnCheck(t *testing.T) {
	a, _, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	healthy := "healthy"
	seq.health = &healthy
	res := mustImage(t, a, "image_deploy", withRunPolicy(specWithHealth(false, hcFast), `{"on_unhealthy":"restart"}`), 120)
	assertDeploy(t, res, "succeeded", "succeeded")
}

func TestHelloImageHealthcheckPolicy(t *testing.T) {
	a := &agent{opt: Options{Log: stdLog(&bytes.Buffer{})}}
	a.policy = policy.Policy{Exists: true, Valid: true}
	p, _ := a.helloPrograms()
	if p == nil || !p.ImageHealthcheckPolicy {
		t.Fatal("hello programs lacks image_healthcheck_policy")
	}
}
