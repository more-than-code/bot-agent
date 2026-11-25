package policy

import "testing"

// v1.24: container_cgroupns is "private" (the default) or "host"; any other
// value, an unquoted or a repeated key makes the whole file invalid with parse.
func TestContainerCgroupnsKey(t *testing.T) {
	for _, tc := range []struct {
		body   string
		valid  bool
		mode   string
		reason string
	}{
		{"enabled = true\n", true, CgroupnsPrivate, ""},
		{"container_cgroupns = \"private\"\n", true, CgroupnsPrivate, ""},
		{"container_cgroupns = \"host\"\n", true, CgroupnsHost, ""},
		{"images = [\"ghcr.io/a/b\"]\ncontainer_cgroupns = \"host\"\n", true, CgroupnsHost, ""},
		{"container_cgroupns = \"Host\"\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupns = \"none\"\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupns = \"\"\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupns = host\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupns = \"host\" \"private\"\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupns = \"host\"\ncontainer_cgroupns = \"host\"\n", false, CgroupnsPrivate, "parse"},
		{"container_cgroupn = \"host\"\n", false, CgroupnsPrivate, "unknown_key"},
	} {
		p := loadBytes(t, []byte(tc.body), 0o644, rootUID)
		if p.Valid != tc.valid || p.CgroupnsMode() != tc.mode || p.Reason != tc.reason {
			t.Fatalf("%q: valid=%v mode=%q reason=%q", tc.body, p.Valid, p.CgroupnsMode(), p.Reason)
		}
	}
}

// The console's runner.toml rewrite (v1.22) keeps the root's
// container_cgroupns line as it is: the console can never change it.
func TestRewriteKeepsContainerCgroupns(t *testing.T) {
	in := "console_policy = true\ncontainer_cgroupns = \"host\"\nedge = false\n"
	out := string(Rewrite([]byte(in), Managed{Edge: true, Images: []string{}, Projects: []string{}}))
	want := "console_policy = true\ncontainer_cgroupns = \"host\"\nedge = true\nimages = []\nprojects = []\n"
	if out != want {
		t.Fatalf("%q", out)
	}
}
