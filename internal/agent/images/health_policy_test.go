package images

import (
	"encoding/json"
	"testing"
)

const testHC = `{"test":["wget","-q","http://127.0.0.1/"],"interval_sec":10,"timeout_sec":2,"retries":3,"start_period_sec":0}`

// policySpec builds a spec; a service gets a healthcheck so on_unhealthy
// actions are allowed (v1.9 rev 2).
func policySpec(t *testing.T, kind, hp string) ImageSpec {
	t.Helper()
	hc := ""
	if kind == "service" {
		hc = testHC
	}
	return policySpecHC(t, kind, hp, hc)
}

func policySpecHC(t *testing.T, kind, hp, hc string) ImageSpec {
	t.Helper()
	raw := `{"kind":"` + kind + `","image":"ghcr.io/acme/web:1"`
	if hc != "" {
		raw += `,"healthcheck":` + hc
	}
	if hp != "" {
		raw += `,"health_policy":` + hp
	}
	raw += `}`
	var s ImageSpec
	if err := Decode([]byte(raw), &s); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return s
}

func TestHealthPolicyValidate(t *testing.T) {
	cases := []struct {
		name string
		kind string
		hp   string
		want string
	}{
		{"absent", "service", "", ""},
		{"null", "service", "null", ""},
		{"empty object", "service", "{}", ""},
		{"full", "service", `{"on_deploy_failure":"stop","on_unhealthy":"restart","unhealthy_after_sec":15,"max_actions_per_hour":10}`, ""},
		{"keep with none", "service", `{"on_deploy_failure":"keep","on_unhealthy":"none"}`, ""},
		{"keep alone", "service", `{"on_deploy_failure":"keep"}`, ""},
		{"keep with restart", "service", `{"on_deploy_failure":"keep","on_unhealthy":"restart"}`, "health_policy"},
		{"keep with rollback", "service", `{"on_deploy_failure":"keep","on_unhealthy":"rollback"}`, "health_policy"},
		{"stop rollback", "service", `{"on_deploy_failure":"stop","on_unhealthy":"rollback","unhealthy_after_sec":600,"max_actions_per_hour":1}`, ""},
		{"bad on_deploy_failure", "service", `{"on_deploy_failure":"restart"}`, "health_policy.on_deploy_failure"},
		{"empty on_deploy_failure", "service", `{"on_deploy_failure":""}`, "health_policy.on_deploy_failure"},
		{"bad on_unhealthy", "service", `{"on_unhealthy":"stop"}`, "health_policy.on_unhealthy"},
		{"after low", "service", `{"unhealthy_after_sec":14}`, "health_policy.unhealthy_after_sec"},
		{"after zero", "service", `{"unhealthy_after_sec":0}`, "health_policy.unhealthy_after_sec"},
		{"after high", "service", `{"unhealthy_after_sec":601}`, "health_policy.unhealthy_after_sec"},
		{"actions low", "service", `{"max_actions_per_hour":0}`, "health_policy.max_actions_per_hour"},
		{"actions high", "service", `{"max_actions_per_hour":11}`, "health_policy.max_actions_per_hour"},
		{"job", "job", `{}`, "health_policy"},
		{"job null", "job", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := policySpec(t, tc.kind, tc.hp)
			if got := s.Validate(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestHealthPolicyNeedsHealthcheck(t *testing.T) {
	cases := []struct {
		hp   string
		want string
	}{
		{`{"on_unhealthy":"restart"}`, "health_policy.on_unhealthy"},
		{`{"on_unhealthy":"rollback"}`, "health_policy.on_unhealthy"},
		{`{"on_unhealthy":"none"}`, ""},
		{`{"on_deploy_failure":"stop"}`, ""},
		{`{"on_deploy_failure":"keep"}`, ""},
	}
	for _, tc := range cases {
		s := policySpecHC(t, "service", tc.hp, "")
		if got := s.Validate(); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.hp, got, tc.want)
		}
	}
}

func TestHealthPolicyDecodeStrict(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"service","image":"ghcr.io/acme/web:1","health_policy":{"extra":1}}`,
		`{"kind":"service","image":"ghcr.io/acme/web:1","health_policy":{"unhealthy_after_sec":45.5}}`,
		`{"kind":"service","image":"ghcr.io/acme/web:1","health_policy":{"on_unhealthy":1}}`,
	} {
		var s ImageSpec
		if err := Decode([]byte(raw), &s); err == nil {
			t.Fatalf("decoded %s", raw)
		}
	}
}

func TestHealthDefaults(t *testing.T) {
	want := Policy{OnDeployFailure: "rollback", OnUnhealthy: "none", UnhealthyAfterSec: 45, MaxActionsPerHour: 3}
	for _, hp := range []string{"", "null", "{}"} {
		if got := policySpec(t, "service", hp).Health(); got != want {
			t.Fatalf("%q: %+v", hp, got)
		}
	}
	got := policySpec(t, "service", `{"on_unhealthy":"restart","max_actions_per_hour":5}`).Health()
	if got != (Policy{OnDeployFailure: "rollback", OnUnhealthy: "restart", UnhealthyAfterSec: 45, MaxActionsPerHour: 5}) {
		t.Fatalf("%+v", got)
	}
}

func TestHealthPolicyAbsentKeepsSpecJSON(t *testing.T) {
	s := policySpec(t, "service", "")
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["health_policy"]; ok {
		t.Fatalf("%s", raw)
	}
	s = policySpec(t, "service", `{"on_unhealthy":"restart"}`)
	raw, _ = json.Marshal(s)
	var back ImageSpec
	if err := Decode(raw, &back); err != nil || back.Health() != s.Health() {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestPolicyLabelsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		kind string
		hp   string
		want *ContainerPolicy
	}{
		{"absent", "service", "", nil},
		{"none", "service", `{"on_unhealthy":"none","on_deploy_failure":"stop"}`, nil},
		{"restart defaults", "service", `{"on_unhealthy":"restart"}`, &ContainerPolicy{"restart", 45, 3}},
		{"rollback values", "service", `{"on_unhealthy":"rollback","unhealthy_after_sec":120,"max_actions_per_hour":1}`, &ContainerPolicy{"rollback", 120, 1}},
		{"job", "job", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := PolicyLabels(policySpec(t, tc.kind, tc.hp))
			if tc.want == nil {
				if labels != nil {
					t.Fatalf("%v", labels)
				}
				return
			}
			if len(labels) != 3 {
				t.Fatalf("%v", labels)
			}
			got := PolicyFromLabels(labels)
			if got == nil || *got != *tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestPolicyFromLabelsInvalid(t *testing.T) {
	good := map[string]string{LabelOnUnhealthy: "restart", LabelUnhealthyAfterSec: "45", LabelMaxActionsPerHour: "3"}
	if PolicyFromLabels(good) == nil {
		t.Fatal("good")
	}
	mutate := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == "<del>" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, m := range []map[string]string{
		mutate(LabelOnUnhealthy, "none"),
		mutate(LabelOnUnhealthy, "stop"),
		mutate(LabelOnUnhealthy, "<del>"),
		mutate(LabelUnhealthyAfterSec, "14"),
		mutate(LabelUnhealthyAfterSec, "601"),
		mutate(LabelUnhealthyAfterSec, "045"),
		mutate(LabelUnhealthyAfterSec, "+45"),
		mutate(LabelUnhealthyAfterSec, "45s"),
		mutate(LabelUnhealthyAfterSec, ""),
		mutate(LabelUnhealthyAfterSec, "<del>"),
		mutate(LabelMaxActionsPerHour, "0"),
		mutate(LabelMaxActionsPerHour, "11"),
		mutate(LabelMaxActionsPerHour, "<del>"),
		nil,
	} {
		if got := PolicyFromLabels(m); got != nil {
			t.Fatalf("%v -> %+v", m, got)
		}
	}
}

func TestOwnsLabels(t *testing.T) {
	good := func() map[string]string {
		return map[string]string{
			LabelOwner: OwnerValue, LabelHost: "h1", LabelProject: "acme", LabelEntry: "web",
			LabelSpecHash: "x",
		}
	}
	cases := []struct {
		name string
		key  string
		val  string // "<del>" removes the key
		want bool
	}{
		{"all correct", "", "", true},
		{"wrong owner", LabelOwner, "someone", false},
		{"no owner", LabelOwner, "<del>", false},
		{"wrong host", LabelHost, "h2", false},
		{"no host", LabelHost, "<del>", false},
		{"wrong project", LabelProject, "other", false},
		{"no project", LabelProject, "<del>", false},
		{"wrong entry", LabelEntry, "api", false},
		{"no entry", LabelEntry, "<del>", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := good()
			if tc.key != "" {
				if tc.val == "<del>" {
					delete(l, tc.key)
				} else {
					l[tc.key] = tc.val
				}
			}
			if got := OwnsLabels(l, "acme", "web", "h1"); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
	if OwnsLabels(nil, "acme", "web", "h1") {
		t.Fatal("nil labels")
	}
	// An empty host never matches a labelled container, nor a missing label.
	if OwnsLabels(good(), "acme", "web", "") {
		t.Fatal("empty host")
	}
}
