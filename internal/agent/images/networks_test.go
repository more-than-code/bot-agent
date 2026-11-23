package images

import (
	"encoding/json"
	"strings"
	"testing"
)

// v1.13 item 3: networks is optional; 1–4 unique names with the shared
// network grammar. An empty array is refused.
func TestSpecNetworks(t *testing.T) {
	cases := []struct {
		nets []string
		want string
	}{
		{nil, ""},
		{[]string{"dd"}, ""},
		{[]string{"a", "b-2", "c", strings.Repeat("d", 32)}, ""},
		{[]string{}, "networks"},
		{[]string{"dd", "dd"}, "networks"},
		{[]string{"a", "b", "c", "d", "e"}, "networks"},
		{[]string{"DD"}, "networks"},
		{[]string{"-dd"}, "networks"},
		{[]string{""}, "networks"},
		{[]string{strings.Repeat("d", 33)}, "networks"},
		{[]string{"d_d"}, "networks"},
	}
	for _, tc := range cases {
		s := ImageSpec{Kind: "service", Image: "ghcr.io/acme/web:1", Networks: tc.nets}
		if got := s.Validate(); got != tc.want {
			t.Errorf("%q: got %q", tc.nets, got)
		}
		j := ImageSpec{Kind: "job", Image: "ghcr.io/acme/web:1", Networks: tc.nets}
		if got := j.Validate(); got != tc.want {
			t.Errorf("job %q: got %q", tc.nets, got)
		}
	}
}

// An absent networks key is not marshalled, so specs without it are the
// same bytes as before v1.13.
func TestSpecNetworksOmitted(t *testing.T) {
	b, err := json.Marshal(ImageSpec{Kind: "service", Image: "x"})
	if err != nil || strings.Contains(string(b), "networks") {
		t.Fatalf("%s %v", b, err)
	}
	b, err = json.Marshal(ImageSpec{Kind: "service", Image: "x", Networks: []string{"dd"}})
	if err != nil || !strings.Contains(string(b), `"networks":["dd"]`) {
		t.Fatalf("%s %v", b, err)
	}
	if n, ok := SharedNetworkName("dd"); !ok || n != "deploybot-shared-dd" {
		t.Fatal(n)
	}
	if _, ok := SharedNetworkName("Bad"); ok {
		t.Fatal("bad name accepted")
	}
}

// agent-protocol v1.21 item 3: network is absent or "host"; host needs an
// empty networks list. An absent key is not marshalled.
func TestSpecNetworkHost(t *testing.T) {
	host, bridge, empty := "host", "bridge", ""
	cases := []struct {
		network *string
		nets    []string
		want    string
	}{
		{nil, nil, ""},
		{&host, nil, ""},
		{&bridge, nil, "network"},
		{&empty, nil, "network"},
		{&host, []string{"dd"}, "networks"},
	}
	for _, tc := range cases {
		for _, kind := range []string{"service", "job"} {
			s := ImageSpec{Kind: kind, Image: "ghcr.io/acme/web:1", Network: tc.network, Networks: tc.nets}
			if got := s.Validate(); got != tc.want {
				t.Errorf("%s %v %v: got %q", kind, tc.network, tc.nets, got)
			}
		}
	}
	b, err := json.Marshal(ImageSpec{Kind: "service", Image: "x"})
	if err != nil || strings.Contains(string(b), `"network"`) {
		t.Fatalf("%s %v", b, err)
	}
	s := ImageSpec{Kind: "service", Image: "x", Network: &host}
	b, err = json.Marshal(s)
	if err != nil || !strings.Contains(string(b), `"network":"host"`) || !s.HostNetwork() {
		t.Fatalf("%s %v", b, err)
	}
}

// v1.21 host_env_images: a pattern within the images list.
func TestPatternWithin(t *testing.T) {
	pats := []string{"ghcr.io/acme/*", "docker.io/library/caddy"}
	for p, want := range map[string]bool{
		"ghcr.io/acme/api":        true,
		"ghcr.io/acme/*":          true,
		"docker.io/library/caddy": true,
		"ghcr.io/*/api":           false,
		"ghcr.io/other/api":       false,
		"ghcr.io/acme/api/sub":    false,
		"quay.io/acme/api":        false,
		"bad pattern@":            false,
	} {
		if got := PatternWithin(p, pats); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}
