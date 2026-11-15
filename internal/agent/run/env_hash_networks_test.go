package run

import (
	"context"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// agent-protocol v1.13 and v1.14 together: an entry on a shared network
// whose task carries env_hash.

func withNetworks(spec []byte) []byte {
	return []byte(strings.Replace(string(spec), `"healthy_after_sec"`, `"networks":["dd"],"healthy_after_sec"`, 1))
}

func openDD(a *agent) {
	a.policy.SharedNetworks = []policy.SharedNetwork{{Name: "dd", Subnet: "10.200.10.0/24", Projects: []string{"acme"}}}
}

// A deploy with a matching env_hash label and an allowed network is
// unchanged: no pull of a new container, no container_deploy.
func TestEnvHashNetworksUnchanged(t *testing.T) {
	a, _, _, seq := imageRun(t)
	openDD(a)
	seq.pre = runningStatus(0)
	seq.preHash = imgHash
	seq.preEnv = envA
	seq.post = runningStatus(0)
	res := mustImage(t, a, "image_deploy", withTaskEnvHash(withNetworks(serviceSpec(false, 5, 3031)), envA), 120)
	assertDeploy(t, res, "unchanged", "succeeded")
	if seq.deployed {
		t.Fatal("unchanged deploy called container_deploy")
	}

	// A different env_hash on the same network recreates, and the deploy
	// carries both the networks and the env_hash.
	a, _, _, seq = imageRun(t)
	openDD(a)
	seq.pre = runningStatus(0)
	seq.preHash = imgHash
	seq.preEnv = envA
	seq.post = runningStatus(0)
	res = mustImage(t, a, "image_deploy", withTaskEnvHash(withNetworks(serviceSpec(false, 5, 3031)), envB), 120)
	assertDeploy(t, res, "succeeded", "succeeded")
	var req images.DeployRequest
	if err := images.Decode(seq.deployBody, &req); err != nil || req.EnvHash != envB || strings.Join(req.Spec.Networks, ",") != "dd" {
		t.Fatalf("deploy body %s %v", seq.deployBody, err)
	}
}

// A rollback whose network was revoked from the policy after the API sent
// the task: the agent's v1.13 pre-check refuses the task while parsing it,
// before the env_hash swap-or-pull decision, so both a matching and a
// mismatching env_hash are a task_rejected policy_denied with no helper
// call at all.
func TestEnvHashNetworkRevokedRollback(t *testing.T) {
	for name, label := range map[string]string{"matching": envA, "mismatching": envB} {
		t.Run(name, func(t *testing.T) {
			a, h, _, seq := imageRun(t) // no shared networks in the policy
			seq.pre = runningStatus(0)
			seq.prev = &images.PrevView{Digest: protocolStr(imgDigest), SpecHash: imgHash, EnvHash: label}
			seq.post = runningStatus(0)
			rej, _, ok := a.execute(context.Background(), imageTask("image_rollback", withTaskEnvHash(withNetworks(serviceRollbackSpec()), envA), 120))
			if ok || rej.ErrorCode != "policy_denied" {
				t.Fatalf("ok %v rejection %+v", ok, rej)
			}
			if calls := h.Calls(); len(calls) != 0 || seq.deployed || seq.pullRef != "" {
				t.Fatalf("helper calls %v deployed %v pull %q", calls, seq.deployed, seq.pullRef)
			}
		})
	}
}
