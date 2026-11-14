package policy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const sharedBase = "enabled = true\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"daikou\", \"defect-drainer\", \"other\"]\n"

func sharedPolicy(t *testing.T, extra string) Policy {
	t.Helper()
	return loadBytes(t, []byte(sharedBase+extra), 0o644, rootUID)
}

// v1.13 item 1: the two keys parse into networks with members, in file order.
func TestSharedNetworksValid(t *testing.T) {
	p := sharedPolicy(t, ""+
		"shared_networks = [\"dd=10.200.10.0/24\", \"z=192.168.4.0/28\", \"empty=172.16.0.0/16\"]\n"+
		"shared_network_projects = [\"dd:defect-drainer\", \"z:other\", \"dd:daikou\"]\n")
	if !p.Valid {
		t.Fatalf("reason %s", p.Reason)
	}
	want := []SharedNetwork{
		{Name: "dd", Subnet: "10.200.10.0/24", Projects: []string{"defect-drainer", "daikou"}},
		{Name: "z", Subnet: "192.168.4.0/28", Projects: []string{"other"}},
		{Name: "empty", Subnet: "172.16.0.0/16", Projects: []string{}},
	}
	if !reflect.DeepEqual(p.SharedNetworks, want) {
		t.Fatalf("%+v", p.SharedNetworks)
	}
	if !p.NetworkAllowed("dd", "daikou") || !p.NetworkAllowed("dd", "defect-drainer") {
		t.Fatal("members refused")
	}
	if p.NetworkAllowed("dd", "other") || p.NetworkAllowed("empty", "other") || p.NetworkAllowed("nope", "daikou") {
		t.Fatal("non-member allowed")
	}
	if n, ok := p.Network("z"); !ok || n.Subnet != "192.168.4.0/28" {
		t.Fatalf("%+v %v", n, ok)
	}
}

func TestSharedNetworksUnset(t *testing.T) {
	p := sharedPolicy(t, "")
	if !p.Valid || p.SharedNetworks == nil || len(p.SharedNetworks) != 0 {
		t.Fatalf("%v %#v", p.Valid, p.SharedNetworks)
	}
	p = sharedPolicy(t, "shared_networks = []\nshared_network_projects = []\n")
	if !p.Valid || len(p.SharedNetworks) != 0 {
		t.Fatalf("%v %s", p.Valid, p.Reason)
	}
}

// Every violation makes the whole file invalid with reason parse.
func TestSharedNetworksViolations(t *testing.T) {
	cases := map[string]string{
		"no separator":         `shared_networks = ["dd"]`,
		"empty name":           `shared_networks = ["=10.200.10.0/24"]`,
		"upper name":           `shared_networks = ["DD=10.200.10.0/24"]`,
		"leading dash":         `shared_networks = ["-dd=10.200.10.0/24"]`,
		"name too long":        `shared_networks = ["` + strings.Repeat("a", 33) + `=10.200.10.0/24"]`,
		"duplicate name":       `shared_networks = ["dd=10.200.10.0/24", "dd=10.200.11.0/24"]`,
		"not a cidr":           `shared_networks = ["dd=10.200.10.0"]`,
		"host bits":            `shared_networks = ["dd=10.200.10.1/24"]`,
		"leading zero":         `shared_networks = ["dd=10.200.010.0/24"]`,
		"public range":         `shared_networks = ["dd=8.8.8.0/24"]`,
		"cgnat range":          `shared_networks = ["dd=100.64.0.0/24"]`,
		"172 outside /12":      `shared_networks = ["dd=172.32.0.0/24"]`,
		"just below 172.16/12": `shared_networks = ["dd=172.15.255.0/24"]`,
		"just below 192.168":   `shared_networks = ["dd=192.167.255.0/24"]`,
		"ipv6":                 `shared_networks = ["dd=fd00::/64"]`,
		"prefix too short":     `shared_networks = ["dd=10.0.0.0/15"]`,
		"prefix too long":      `shared_networks = ["dd=10.200.10.0/29"]`,
		"wider than range":     `shared_networks = ["dd=192.168.0.0/15"]`,
		"overlap":              `shared_networks = ["a=10.200.0.0/16", "b=10.200.10.0/24"]`,
		"equal subnets":        `shared_networks = ["a=10.200.10.0/24", "b=10.200.10.0/24"]`,
		"member no separator":  "shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = [\"dd\"]",
		"member unknown net":   "shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = [\"xx:daikou\"]",
		"member no networks":   `shared_network_projects = ["dd:daikou"]`,
		"member not projected": "shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = [\"dd:stranger\"]",
		"member duplicate":     "shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = [\"dd:daikou\", \"dd:daikou\"]",
		"member not array":     "shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = \"dd:daikou\"",
		"networks not array":   `shared_networks = "dd=10.200.10.0/24"`,
	}
	for name, extra := range cases {
		p := sharedPolicy(t, extra+"\n")
		if p.Valid || p.Reason != "parse" {
			t.Errorf("%s: valid %v reason %q", name, p.Valid, p.Reason)
		}
		if p.SharedNetworks != nil {
			t.Errorf("%s: networks on an invalid policy", name)
		}
	}
	// The network cap: 8 is valid, 9 is not.
	nets := make([]string, 8)
	for i := range nets {
		nets[i] = fmt.Sprintf("%q", fmt.Sprintf("n%d=10.%d.0.0/16", i, i))
	}
	if p := sharedPolicy(t, "shared_networks = ["+strings.Join(nets, ", ")+"]\n"); !p.Valid || len(p.SharedNetworks) != 8 {
		t.Fatalf("8 networks: %v %s", p.Valid, p.Reason)
	}
	nets = append(nets, `"n8=10.8.0.0/16"`)
	if p := sharedPolicy(t, "shared_networks = ["+strings.Join(nets, ", ")+"]\n"); p.Valid || p.Reason != "parse" {
		t.Fatalf("9 networks: %v %s", p.Valid, p.Reason)
	}
}

// RFC 1918 edges: the last /28 of 172.16.0.0/12 is valid.
func TestSharedNetworkRangeEdges(t *testing.T) {
	for _, cidr := range []string{"172.31.255.240/28", "10.255.255.240/28", "192.168.255.240/28"} {
		p := sharedPolicy(t, `shared_networks = ["dd=`+cidr+`"]`+"\n")
		if !p.Valid || len(p.SharedNetworks) != 1 || p.SharedNetworks[0].Subnet != cidr {
			t.Errorf("%s: valid %v reason %q", cidr, p.Valid, p.Reason)
		}
	}
}

// At most 32 member items in all.
func TestSharedNetworkMembersCap(t *testing.T) {
	members := func(n int) string {
		projects := make([]string, n)
		items := make([]string, n)
		for i := range items {
			projects[i] = fmt.Sprintf("%q", fmt.Sprintf("p%d", i))
			items[i] = fmt.Sprintf("%q", fmt.Sprintf("dd:p%d", i))
		}
		return "enabled = true\nprojects = [" + strings.Join(projects, ", ") + "]\n" +
			"shared_networks = [\"dd=10.200.10.0/24\"]\n" +
			"shared_network_projects = [" + strings.Join(items, ", ") + "]\n"
	}
	if p := loadBytes(t, []byte(members(32)), 0o644, rootUID); !p.Valid || len(p.SharedNetworks[0].Projects) != 32 {
		t.Fatalf("32: %v %s", p.Valid, p.Reason)
	}
	// 33 projects already break the projects cap, so test the member cap
	// with two networks over 32 projects.
	projects := make([]string, 32)
	var items []string
	for i := range projects {
		projects[i] = fmt.Sprintf("%q", fmt.Sprintf("p%d", i))
		items = append(items, fmt.Sprintf("%q", fmt.Sprintf("a:p%d", i)))
	}
	items = append(items, `"b:p0"`)
	body := "enabled = true\nprojects = [" + strings.Join(projects, ", ") + "]\n" +
		"shared_networks = [\"a=10.200.10.0/24\", \"b=10.200.11.0/24\"]\n" +
		"shared_network_projects = [" + strings.Join(items, ", ") + "]\n"
	if p := loadBytes(t, []byte(body), 0o644, rootUID); p.Valid || p.Reason != "parse" {
		t.Fatalf("33: %v %s", p.Valid, p.Reason)
	}
}
