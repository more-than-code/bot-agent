package policy

import (
	"net/netip"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// Shared network bounds (agent-protocol v1.13 item 1).
const (
	MaxSharedNetworks       = 8
	MaxSharedNetworkMembers = 32
	minSharedPrefix         = 16
	maxSharedPrefix         = 28
)

// rfc1918 are the only ranges a shared subnet may sit in.
var rfc1918 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// SharedNetwork is one shared_networks item with its members from
// shared_network_projects, both in file order. Projects is never nil.
type SharedNetwork struct {
	Name     string
	Subnet   string
	Projects []string
}

// NetworkAllowed is the v1.13 deploy gate: the network is defined and lists
// project. It does not check the projects list; callers already have.
func (p Policy) NetworkAllowed(name, project string) bool {
	for _, n := range p.SharedNetworks {
		if n.Name != name {
			continue
		}
		for _, x := range n.Projects {
			if x == project {
				return true
			}
		}
		return false
	}
	return false
}

// Network returns the defined network called name.
func (p Policy) Network(name string) (SharedNetwork, bool) {
	for _, n := range p.SharedNetworks {
		if n.Name == name {
			return n, true
		}
	}
	return SharedNetwork{}, false
}

// validateSharedNetworks builds the shared network list from the two keys.
// projects must already be validated. Any violation is "parse".
func validateSharedNetworks(nets, members, projects []string) ([]SharedNetwork, string) {
	if len(nets) > MaxSharedNetworks || len(members) > MaxSharedNetworkMembers {
		return nil, "parse"
	}
	out := make([]SharedNetwork, 0, len(nets))
	index := map[string]int{}
	var prefixes []netip.Prefix
	for _, item := range nets {
		name, cidr, ok := strings.Cut(item, "=")
		if !ok || !images.ValidSharedName(name) {
			return nil, "parse"
		}
		if _, dup := index[name]; dup {
			return nil, "parse"
		}
		pfx, ok := sharedSubnet(cidr)
		if !ok {
			return nil, "parse"
		}
		for _, other := range prefixes {
			if other.Overlaps(pfx) {
				return nil, "parse"
			}
		}
		prefixes = append(prefixes, pfx)
		index[name] = len(out)
		out = append(out, SharedNetwork{Name: name, Subnet: cidr, Projects: []string{}})
	}
	listed := map[string]bool{}
	for _, p := range projects {
		listed[p] = true
	}
	seen := map[string]bool{}
	for _, item := range members {
		name, slug, ok := strings.Cut(item, ":")
		if !ok || seen[item] {
			return nil, "parse"
		}
		seen[item] = true
		i, defined := index[name]
		if !defined || !listed[slug] {
			return nil, "parse"
		}
		out[i].Projects = append(out[i].Projects, slug)
	}
	return out, ""
}

// sharedSubnet accepts a canonical IPv4 CIDR inside RFC 1918 with a prefix
// length from 16 to 28. Canonical means host bits zero and the exact text
// that netip prints back.
func sharedSubnet(s string) (netip.Prefix, bool) {
	pfx, err := netip.ParsePrefix(s)
	if err != nil || !pfx.Addr().Is4() || pfx.Masked() != pfx || pfx.String() != s {
		return netip.Prefix{}, false
	}
	if pfx.Bits() < minSharedPrefix || pfx.Bits() > maxSharedPrefix {
		return netip.Prefix{}, false
	}
	for _, r := range rfc1918 {
		if r.Bits() <= pfx.Bits() && r.Contains(pfx.Addr()) {
			return pfx, true
		}
	}
	return netip.Prefix{}, false
}
