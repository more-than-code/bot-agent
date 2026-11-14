package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func withNetworks(spec []byte, nets string) []byte {
	return []byte(strings.Replace(string(spec), `"kind":"`, `"networks":`+nets+`,"kind":"`, 1))
}

// v1.13 item 3: the agent pre-check. Shape errors are bad_spec; a name not
// defined or not listing the project is policy_denied.
func TestSharedNetworksPrecheck(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	on := policy.Policy{
		Valid: true, Enabled: true,
		Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme", "other"},
		SharedNetworks: []policy.SharedNetwork{
			{Name: "dd", Subnet: "10.200.10.0/24", Projects: []string{"acme"}},
			{Name: "ee", Subnet: "10.200.11.0/24", Projects: []string{"other"}},
			{Name: "ff", Subnet: "10.200.12.0/24", Projects: []string{"acme"}},
		},
	}
	good := deploySpec(false, "")
	cases := []struct {
		name string
		kind string
		spec []byte
		want string
	}{
		{"absent", "image_deploy", good, ""},
		{"null is absent", "image_deploy", withNetworks(good, `null`), ""},
		{"member", "image_deploy", withNetworks(good, `["dd"]`), ""},
		{"two members", "image_deploy", withNetworks(good, `["dd","ff"]`), ""},
		{"rollback member", "image_rollback", withNetworks(rollbackSpec(imgDigest), `["dd"]`), ""},
		{"not a member", "image_deploy", withNetworks(good, `["ee"]`), "policy_denied"},
		{"one not a member", "image_deploy", withNetworks(good, `["dd","ee"]`), "policy_denied"},
		{"undefined", "image_deploy", withNetworks(good, `["zz"]`), "policy_denied"},
		{"rollback not a member", "image_rollback", withNetworks(rollbackSpec(imgDigest), `["ee"]`), "policy_denied"},
		{"empty array", "image_deploy", withNetworks(good, `[]`), "bad_spec"},
		{"duplicate", "image_deploy", withNetworks(good, `["dd","dd"]`), "bad_spec"},
		{"bad name", "image_deploy", withNetworks(good, `["DD"]`), "bad_spec"},
		{"name too long", "image_deploy", withNetworks(good, `["`+strings.Repeat("a", 33)+`"]`), "bad_spec"},
		{"five", "image_deploy", withNetworks(good, `["a","b","c","d","e"]`), "bad_spec"},
		{"not strings", "image_deploy", withNetworks(good, `[1]`), "bad_spec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := Decide(task(host, tc.kind, nil, live, tc.spec), host, now, nil, Gate{Policy: on})
			if code != tc.want {
				t.Fatalf("got %q", code)
			}
		})
	}
	// A policy without shared networks refuses any name.
	off := on
	off.SharedNetworks = []policy.SharedNetwork{}
	if code, _ := Decide(task(host, "image_deploy", nil, live, withNetworks(good, `["dd"]`)), host, now, nil, Gate{Policy: off}); code != "policy_denied" {
		t.Fatalf("no networks: %q", code)
	}
}
