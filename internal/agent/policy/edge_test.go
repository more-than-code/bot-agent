package policy

import "testing"

// v1.15 item 1: edge is a bool key, default false. It does not depend on
// enabled, images or projects.
func TestEdgeKey(t *testing.T) {
	for _, tc := range []struct {
		body   string
		valid  bool
		edge   bool
		reason string
	}{
		{"enabled = true\n", true, false, ""},
		{"edge = true\n", true, true, ""},
		{"edge = false\n", true, false, ""},
		{"enabled = false\nedge = true\n", true, true, ""},
		{sharedBase + "edge = true\n", true, true, ""},
		{"edge = \"true\"\n", false, false, "parse"},
		{"edge = 1\n", false, false, "parse"},
		{"edge = [true]\n", false, false, "parse"},
		{"edge = true\nedge = true\n", false, false, "parse"},
		{"edge_v2 = true\n", false, false, "unknown_key"},
		// An invalid file never turns the edge on.
		{"edge = true\nimages = [\"bad pattern\"]\n", false, false, "parse"},
	} {
		p := loadBytes(t, []byte(tc.body), 0o644, rootUID)
		if p.Valid != tc.valid || p.EdgeEnabled() != tc.edge || p.Reason != tc.reason {
			t.Fatalf("%q: valid=%v edge=%v reason=%q", tc.body, p.Valid, p.EdgeEnabled(), p.Reason)
		}
	}
}

// Adding the key leaves every other key as it was: a file valid before is
// valid now with the same flags, and edge = true changes only Edge.
func TestEdgeKeyKeepsOtherKeys(t *testing.T) {
	base := sharedBase + "shared_networks = [\"dd=10.200.10.0/24\"]\n" +
		"shared_network_projects = [\"dd:daikou\"]\nmax_containers = 5\nallow_public_ports = true\n"
	before := loadBytes(t, []byte(base), 0o644, rootUID)
	after := loadBytes(t, []byte(base+"edge = true\n"), 0o644, rootUID)
	if !before.Valid || !after.Valid {
		t.Fatalf("%s %s", before.Reason, after.Reason)
	}
	if before.Edge || !after.Edge {
		t.Fatal("edge flag")
	}
	if before.ImagesEnabled() != after.ImagesEnabled() || before.MaxContainers != after.MaxContainers ||
		before.AllowPublicPorts != after.AllowPublicPorts || len(before.SharedNetworks) != len(after.SharedNetworks) {
		t.Fatal("other keys changed")
	}
}
