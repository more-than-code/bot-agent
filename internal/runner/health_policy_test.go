package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func withPolicyRaw(t *testing.T, req images.DeployRequest, hp string) []byte {
	t.Helper()
	raw := mustJSON(t, req)
	needle := []byte(`"kind":"` + req.Spec.Kind + `"`)
	if !bytes.Contains(raw, needle) {
		t.Fatalf("kind missing in %s", raw)
	}
	return bytes.Replace(raw, needle, []byte(`"kind":"`+req.Spec.Kind+`","health_policy":`+hp), 1)
}

func createLabels(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var got struct{ Labels map[string]string }
	if json.Unmarshal(body, &got) != nil {
		t.Fatal(string(body))
	}
	return got.Labels
}

func TestCreatePolicyLabels(t *testing.T) {
	cases := []struct {
		name string
		hp   string
		want map[string]string
	}{
		{"absent", "", nil},
		{"none", `{"on_unhealthy":"none","on_deploy_failure":"keep"}`, nil},
		{"restart defaults", `{"on_unhealthy":"restart"}`, map[string]string{
			images.LabelOnUnhealthy: "restart", images.LabelUnhealthyAfterSec: "45", images.LabelMaxActionsPerHour: "3",
		}},
		{"rollback values", `{"on_unhealthy":"rollback","unhealthy_after_sec":90,"max_actions_per_hour":2}`, map[string]string{
			images.LabelOnUnhealthy: "rollback", images.LabelUnhealthyAfterSec: "90", images.LabelMaxActionsPerHour: "2",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			req.Spec.Healthcheck = &images.Healthcheck{Test: []string{"wget"}, IntervalSec: 10, TimeoutSec: 2, Retries: 3}
			body := mustJSON(t, req)
			if tc.hp != "" {
				body = withPolicyRaw(t, req, tc.hp)
			}
			if raw := e.call(images.VerbDeploy, body); imgCode(t, raw) != "" {
				t.Fatal(string(raw))
			}
			labels := createLabels(t, e.fake.body(http.MethodPost, "/containers/create"))
			if labels[images.LabelOwner] != images.OwnerValue || labels[images.LabelEntry] != "web" {
				t.Fatalf("owner labels %v", labels)
			}
			for _, k := range []string{images.LabelOnUnhealthy, images.LabelUnhealthyAfterSec, images.LabelMaxActionsPerHour} {
				v, ok := labels[k]
				if tc.want == nil {
					if ok {
						t.Fatalf("%s set: %v", k, labels)
					}
					continue
				}
				if v != tc.want[k] {
					t.Fatalf("%s=%q want %q", k, v, tc.want[k])
				}
			}
			// The create labels read back as the list policy.
			pol := images.PolicyFromLabels(labels)
			if (pol == nil) != (tc.want == nil) {
				t.Fatalf("policy %+v", pol)
			}
		})
	}
}

func TestHealthPolicyRefusedBeforeEngine(t *testing.T) {
	cases := []struct {
		name string
		kind string
		hp   string
	}{
		{"unknown key", "service", `{"on_unhealthy":"restart","extra":1}`},
		{"bad enum", "service", `{"on_deploy_failure":"retry"}`},
		{"after low", "service", `{"unhealthy_after_sec":5}`},
		{"actions high", "service", `{"max_actions_per_hour":20}`},
		{"on job", "job", `{}`},
		{"action without healthcheck", "service", `{"on_unhealthy":"restart"}`},
		{"keep with action", "service", `{"on_deploy_failure":"keep","on_unhealthy":"rollback"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if tc.kind == "job" {
				req = jobDeploy()
			}
			raw := e.call(images.VerbDeploy, withPolicyRaw(t, req, tc.hp))
			if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
				t.Fatal(string(raw))
			}
			if len(e.fake.Calls()) != 0 {
				t.Fatal("engine called")
			}
		})
	}
}

func TestListPolicy(t *testing.T) {
	owned := func(extra map[string]string) map[string]string {
		m := map[string]string{
			images.LabelOwner: images.OwnerValue, images.LabelHost: "h", images.LabelProject: "acme",
			images.LabelEntry: "web", images.LabelSpecHash: hashC,
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	good := map[string]string{images.LabelOnUnhealthy: "restart", images.LabelUnhealthyAfterSec: "60", images.LabelMaxActionsPerHour: "2"}
	bad := map[string]string{images.LabelOnUnhealthy: "restart", images.LabelUnhealthyAfterSec: "5", images.LabelMaxActionsPerHour: "2"}
	rows := []listed{
		{ID: "a", Names: []string{"/acme-web"}, State: "running", Labels: owned(good)},
		{ID: "b", Names: []string{"/acme-api"}, State: "running", Labels: owned(nil)},
		{ID: "c", Names: []string{"/acme-bad"}, State: "running", Labels: owned(bad)},
		{ID: "d", Names: []string{"/foreign"}, State: "running", Labels: good},
	}
	raw := images.Encode(projectRows(rows))
	var resp images.ListResponse
	if err := images.Decode(raw, &resp); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	byName := map[string]images.ContainerInfo{}
	for _, it := range resp.Items {
		byName[it.Name] = it
	}
	if p := byName["/acme-web"].Policy; p == nil || *p != (images.ContainerPolicy{OnUnhealthy: "restart", UnhealthyAfterSec: 60, MaxActionsPerHour: 2}) {
		t.Fatalf("web %+v", p)
	}
	for _, n := range []string{"/acme-api", "/acme-bad", "/foreign"} {
		if byName[n].Policy != nil {
			t.Fatalf("%s %+v", n, byName[n].Policy)
		}
	}
	if !bytes.Contains(raw, []byte(`"policy":null`)) || !bytes.Contains(raw, []byte(`"policy":{"on_unhealthy":"restart","unhealthy_after_sec":60,"max_actions_per_hour":2}`)) {
		t.Fatalf("%s", raw)
	}
	// keepLabels still returns only the owner labels; the policy travels as its own object.
	if _, ok := byName["/acme-web"].Labels[images.LabelOnUnhealthy]; ok {
		t.Fatalf("labels %v", byName["/acme-web"].Labels)
	}
}
