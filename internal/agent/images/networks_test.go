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
