package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// agent-protocol v1.21 item 1: policy check prints host_network_projects
// and one line per host env file, never a value.

const checkEnvSecret = "postgres://deploybot:hunter2-long-secret@127.0.0.1/deploybot"

// withEnvRoot points the check's host env files at root and treats them
// as root:root (the test cannot chown).
func withEnvRoot(t *testing.T, root string, owner policy.HostEnvOwner) {
	t.Helper()
	old := checkPolicy
	checkPolicy = func(p string) policy.Policy {
		return policy.Load(policy.Files{Policy: p, SkipOwner: true, EnvRoot: root, EnvOwner: owner})
	}
	t.Cleanup(func() { checkPolicy = old })
}

func writeEnvFile(t *testing.T, root, rel, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestPolicyCheckHostEnv(t *testing.T) {
	root := t.TempDir()
	apiBody := "DATABASE_URL=" + checkEnvSecret + "\nBOT_SECRETS_KEY=abcdefabcdef\n# comment\n"
	apiSum := writeEnvFile(t, root, "etc/deploybot/api.env", apiBody, 0o600)
	mcpSum := writeEnvFile(t, root, "etc/deploybot/mcp.env", "MCP_CLIENT_SECRET=zzzzzzzzzzzz\n", 0o600)
	withEnvRoot(t, root, func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true })
	path := writePolicy(t, ""+
		"enabled = true\n"+
		"images = [\"ghcr.io/more-than-code/*\"]\n"+
		"projects = [\"deploybot\", \"other\"]\n"+
		"host_network_projects = [\"deploybot\", \"other\"]\n"+
		"host_env_files = [\"deploybot/api=/etc/deploybot/api.env\", \"deploybot/mcp=/etc/deploybot/mcp.env\"]\n"+
		"host_env_images = [\"deploybot/api=ghcr.io/more-than-code/deploybot-server\", \"deploybot/mcp=ghcr.io/more-than-code/*\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatalf("%v %q", err, out)
	}
	want := "agent_upgrade_https_proxy=unset\n" +
		"host_network_projects=deploybot,other\n" +
		"host_env_file deploybot/api keys=2 sha256=" + apiSum[:12] + " image=ghcr.io/more-than-code/deploybot-server\n" +
		"host_env_file deploybot/mcp keys=1 sha256=" + mcpSum[:12] + " image=ghcr.io/more-than-code/*\n" +
		"console_policy=false\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("got %q", out)
	}
	for _, leak := range []string{checkEnvSecret, "hunter2", "abcdefabcdef", "zzzzzzzzzzzz", apiSum, mcpSum, "/etc/deploybot"} {
		if strings.Contains(out, leak) {
			t.Fatalf("output leaks %q: %q", leak, out)
		}
	}
}

// A file that is not root 0600 makes the policy invalid with the entry
// name; the path and the values are not printed.
func TestPolicyCheckHostEnvInvalid(t *testing.T) {
	root := t.TempDir()
	writeEnvFile(t, root, "etc/deploybot/api.env", "DATABASE_URL="+checkEnvSecret+"\n", 0o644)
	withEnvRoot(t, root, func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true })
	path := writePolicy(t, ""+
		"enabled = true\n"+
		"images = [\"ghcr.io/more-than-code/*\"]\n"+
		"projects = [\"deploybot\"]\n"+
		"host_env_files = [\"deploybot/api=/etc/deploybot/api.env\"]\n"+
		"host_env_images = [\"deploybot/api=ghcr.io/more-than-code/deploybot-server\"]\n")
	out, err := runPolicyCheck(t, path)
	if err == nil || out != "invalid: host_env_file deploybot/api\n" {
		t.Fatalf("%v %q", err, out)
	}

	// Wrong owner, through the hook.
	writeEnvFile(t, root, "etc/deploybot/api.env", "DATABASE_URL="+checkEnvSecret+"\n", 0o600)
	withEnvRoot(t, root, func(os.FileInfo) (uint32, uint32, bool) { return 1000, 1000, true })
	out, err = runPolicyCheck(t, path)
	if err == nil || out != "invalid: host_env_file deploybot/api\n" {
		t.Fatalf("%v %q", err, out)
	}

	// A host network slug outside projects is parse.
	// No host_env_images line for the file.
	path = writePolicy(t, "projects = [\"deploybot\"]\nhost_env_files = [\"deploybot/api=/etc/deploybot/api.env\"]\n")
	out, err = runPolicyCheck(t, path)
	if err == nil || out != "invalid: host_env_file deploybot/api\n" {
		t.Fatalf("%v %q", err, out)
	}

	path = writePolicy(t, "projects = [\"deploybot\"]\nhost_network_projects = [\"nope\"]\n")
	out, err = runPolicyCheck(t, path)
	if err == nil || out != "invalid: parse\n" {
		t.Fatalf("%v %q", err, out)
	}
}
