package runner

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// agent-protocol v1.13 and v1.14 together in the root helper.

// A create body with shared networks and env_hash has both the label and
// the shared endpoint, built directly and through container_deploy.
func TestEnvHashWithNetworksCreate(t *testing.T) {
	spec := literalSpec()
	spec.Networks = []string{"dd"}
	body, code := buildCreate("mohiguide", "web", digestA, hashC, envHashA, "0123456789abcdef", spec,
		map[string]string{"A": "b"}, nil, []string{"ghcr.io/joe-and-his-friends/*"}, false)
	if code != "" {
		t.Fatal(code)
	}
	assertLabelAndEndpoint(t, body)

	e, _ := sharedEnv(t)
	req := sharedDeploy("dd")
	req.EnvHash = envHashA
	if raw := e.call(images.VerbDeploy, mustJSON(t, req)); imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	assertLabelAndEndpoint(t, e.fake.body(http.MethodPost, "/containers/create"))
}

func assertLabelAndEndpoint(t *testing.T, body []byte) {
	t.Helper()
	if got := createLabels(t, body)[images.LabelEnvHash]; got != envHashA {
		t.Fatalf("env_hash label %v in %s", got, body)
	}
	var create struct {
		NetworkingConfig struct {
			EndpointsConfig map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &create); err != nil {
		t.Fatal(err)
	}
	if _, ok := create.NetworkingConfig.EndpointsConfig["deploybot-shared-dd"]; !ok {
		t.Fatalf("no shared endpoint in %s", body)
	}
}

// With dd revoked from the policy: container_rollback swaps the kept
// container (labelled with an env_hash) back without any network call, and
// the pull-and-create path the agent takes on an env_hash mismatch is
// refused policy_denied before any Engine call.
func TestEnvHashNetworkRevoked(t *testing.T) {
	t.Run("swap keeps endpoints and label", func(t *testing.T) {
		e, _ := sharedEnv(t)
		labels := e.labels("web")
		labels[images.LabelEnvHash] = envHashA
		e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
		prev := e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, labels)
		e.fake.byID[prev].networks = []string{"deploybot-mohiguide", "deploybot-shared-dd"}
		e.setPolicy(imagePolicy) // dd revoked
		raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if imgCode(t, raw) != "" {
			t.Fatal(string(raw))
		}
		for _, c := range e.fake.Calls() {
			if strings.Contains(c.Path, "/networks/") {
				t.Fatalf("network call %s %s", c.Method, c.Path)
			}
		}
		c := e.fake.byID[prev]
		if c.name != "mohiguide-web" || c.state != "running" || c.labels[images.LabelEnvHash] != envHashA {
			t.Fatalf("%+v", c)
		}
		if strings.Join(c.networks, ",") != "deploybot-mohiguide,deploybot-shared-dd" {
			t.Fatalf("networks %v", c.networks)
		}
	})

	t.Run("create path refused", func(t *testing.T) {
		e, _ := sharedEnv(t)
		e.setPolicy(imagePolicy) // dd revoked
		req := sharedDeploy("dd")
		req.Digest = digestB
		req.EnvHash = envHashA
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonPolicy {
			t.Fatalf("code %q", code)
		}
		if n := len(e.fake.Calls()); n != 0 {
			t.Fatalf("%d engine calls", n)
		}
	})
}
