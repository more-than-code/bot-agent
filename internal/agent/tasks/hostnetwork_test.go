package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func withNetwork(spec []byte, value string) []byte {
	return []byte(strings.Replace(string(spec), `"kind":"`, `"network":`+value+`,"kind":"`, 1))
}

// agent-protocol v1.21 item 3: network "host" only for a project in
// host_network_projects (policy_denied otherwise); any other value, or host
// with shared networks, is bad_spec.
func TestHostNetworkPrecheck(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	on := policy.Policy{
		Valid: true, Enabled: true,
		Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme", "other"},
		SharedNetworks: []policy.SharedNetwork{
			{Name: "dd", Subnet: "10.200.10.0/24", Projects: []string{"acme"}},
		},
		HostNetworkProjects: []string{"acme"},
	}
	good := deploySpec(false, "")
	cases := []struct {
		name string
		kind string
		pol  policy.Policy
		spec []byte
		want string
	}{
		{"absent", "image_deploy", on, good, ""},
		{"host allowed", "image_deploy", on, withNetwork(good, `"host"`), ""},
		{"rollback host allowed", "image_rollback", on, withNetwork(rollbackSpec(imgDigest), `"host"`), ""},
		{"null is absent", "image_deploy", on, withNetwork(good, `null`), ""},
		{"not listed", "image_deploy", func() policy.Policy { p := on; p.HostNetworkProjects = []string{"other"}; return p }(), withNetwork(good, `"host"`), "policy_denied"},
		{"no list", "image_deploy", func() policy.Policy { p := on; p.HostNetworkProjects = []string{}; return p }(), withNetwork(good, `"host"`), "policy_denied"},
		{"bridge", "image_deploy", on, withNetwork(good, `"bridge"`), "bad_spec"},
		{"empty", "image_deploy", on, withNetwork(good, `""`), "bad_spec"},
		{"upper", "image_deploy", on, withNetwork(good, `"HOST"`), "bad_spec"},
		{"with shared networks", "image_deploy", on, withNetworks(withNetwork(good, `"host"`), `["dd"]`), "bad_spec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := Decide(task(host, tc.kind, nil, live, tc.spec), host, now, nil, Gate{Policy: tc.pol})
			if code != tc.want {
				t.Fatalf("got %q", code)
			}
		})
	}
}
