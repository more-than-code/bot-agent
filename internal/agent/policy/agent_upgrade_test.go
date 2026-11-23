package policy

import (
	"strings"
	"testing"
)

// v1.20 item 1: agent_upgrade is a bool key, default false. It does not
// depend on enabled, images or projects, and an invalid file is false.
func TestAgentUpgradeKey(t *testing.T) {
	for _, tc := range []struct {
		body    string
		valid   bool
		upgrade bool
		reason  string
	}{
		{"enabled = true\n", true, false, ""},
		{"agent_upgrade = true\n", true, true, ""},
		{"agent_upgrade = false\n", true, false, ""},
		{"enabled = false\nedge = true\nagent_upgrade = true\n", true, true, ""},
		{"agent_upgrade = \"true\"\n", false, false, "parse"},
		{"agent_upgrade = 1\n", false, false, "parse"},
		{"agent_upgrade = true\nagent_upgrade = true\n", false, false, "parse"},
		{"agent_upgrades = true\n", false, false, "unknown_key"},
		{"agent_upgrade = true\nimages = [\"bad pattern\"]\n", false, false, "parse"},
	} {
		p := loadBytes(t, []byte(tc.body), 0o644, rootUID)
		if p.Valid != tc.valid || p.AgentUpgradeEnabled() != tc.upgrade || p.Reason != tc.reason {
			t.Fatalf("%q: valid=%v upgrade=%v reason=%q", tc.body, p.Valid, p.AgentUpgradeEnabled(), p.Reason)
		}
	}
}

// v1.20 item 1: agent_upgrade_https_proxy is http(s)://host:port with no
// userinfo, path, query or fragment, at most 256 bytes; anything else makes
// the whole file invalid with parse.
func TestAgentUpgradeProxyKey(t *testing.T) {
	for _, ok := range []string{
		"http://proxy.lan:3128", "https://10.0.0.1:8443", "http://[fd00::1]:3128",
	} {
		p := loadBytes(t, []byte("agent_upgrade = true\nagent_upgrade_https_proxy = \""+ok+"\"\n"), 0o644, rootUID)
		if !p.Valid || p.AgentUpgradeProxy != ok {
			t.Fatalf("%q: valid=%v %q %s", ok, p.Valid, p.AgentUpgradeProxy, p.Reason)
		}
	}
	long := "http://" + strings.Repeat("a", 250) + ":3128"
	for _, bad := range []string{
		"", "proxy.lan:3128", "socks5://proxy:1080", "http://proxy.lan", "http://user:pw@proxy:3128",
		"http://proxy:3128/", "http://proxy:3128/x", "http://proxy:3128?a=b", "http://proxy:3128#f",
		"http://proxy:0", "http://proxy:65536", "http://proxy:03128", "http://:3128", "http://pro xy:3128", long,
	} {
		p := loadBytes(t, []byte("agent_upgrade_https_proxy = \""+bad+"\"\n"), 0o644, rootUID)
		if p.Valid || p.Reason != "parse" || p.AgentUpgradeProxy != "" {
			t.Fatalf("%q accepted: valid=%v reason=%q", bad, p.Valid, p.Reason)
		}
	}
	for _, bad := range []string{"agent_upgrade_https_proxy = http://proxy:3128\n", "agent_upgrade_https_proxy = [\"http://proxy:3128\"]\n"} {
		if p := loadBytes(t, []byte(bad), 0o644, rootUID); p.Valid || p.Reason != "parse" {
			t.Fatalf("%q accepted", bad)
		}
	}
}
