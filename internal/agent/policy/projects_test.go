package policy

import (
	"fmt"
	"strings"
	"testing"
)

// v1.12 item 5: at most 32 projects; more makes the file invalid (parse).
func TestProjectsCap(t *testing.T) {
	list := func(n int) string {
		xs := make([]string, n)
		for i := range xs {
			xs[i] = fmt.Sprintf("%q", fmt.Sprintf("p%d", i))
		}
		return "enabled = true\nprojects = [" + strings.Join(xs, ", ") + "]\n"
	}
	if p := loadBytes(t, []byte(list(32)), 0o644, rootUID); !p.Valid || len(p.Projects) != 32 {
		t.Fatalf("32: valid %v reason %s", p.Valid, p.Reason)
	}
	if p := loadBytes(t, []byte(list(33)), 0o644, rootUID); p.Valid || p.Reason != "parse" {
		t.Fatalf("33: valid %v reason %s", p.Valid, p.Reason)
	}
}

func TestProjectAllowed(t *testing.T) {
	cases := []struct {
		projects []string
		project  string
		want     bool
	}{
		{[]string{"home-lab"}, "home-lab", true},
		{[]string{"home-lab"}, "other", false},
		{[]string{}, "home-lab", false},
		{nil, "home-lab", false},
		{[]string{"acme", "acme-web"}, "acme", false},
		{[]string{"acme", "acme-web"}, "acme-web", false},
		{[]string{"acme", "acmeweb"}, "acme", true},
	}
	for _, tc := range cases {
		if got := (Policy{Projects: tc.projects}).ProjectAllowed(tc.project); got != tc.want {
			t.Errorf("%v %s: got %v", tc.projects, tc.project, got)
		}
	}
}
