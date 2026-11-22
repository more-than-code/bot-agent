package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// withAccounts points policy check at a passwd and group holding dd-worker
// (uid 994, gid 989: a system account, spec A8). The executables are
// /bin/sh, which is root-owned on every host the suite runs on.
func withAccounts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0::/root:/bin/sh\ndd-worker:x:994:989::/:/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\ndd-worker:x:989:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := checkPolicy
	checkPolicy = func(p string) policy.Policy { return policy.Check(p, passwd, group) }
	t.Cleanup(func() { checkPolicy = old })
}

func TestPolicyCheckHostFiles(t *testing.T) {
	withAccounts(t)
	path := writePolicy(t, ""+
		"shared_networks = [\"core=10.90.0.0/24\"]\n"+
		"host_files = [\"dd-policy=/etc/dd-worker/policy.toml\", \"dd-secrets=/etc/dd-worker/secrets.env\"]\n"+
		"host_file_modes = [\"dd-policy:0644\", \"dd-secrets:0600\"]\n"+
		"host_file_secret = [\"dd-secrets\"]\n"+
		"host_file_units = [\"dd-policy:dd-worker.service\", \"dd-secrets:dd-worker.service\"]\n"+
		"host_file_run_as = [\"dd-policy:dd-worker\", \"dd-secrets:dd-worker\"]\n"+
		"host_file_checks = [\"dd-policy:/bin/sh policy check --file -\", \"dd-secrets:/bin/sh secrets check --file -\"]\n"+
		"host_file_health = [\"dd-policy:/bin/sh health --wait 30s\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatalf("%v %q", err, out)
	}
	want := "shared_network core 10.90.0.0/24 -\n" +
		"edge=false\n" +
		"host_files=2\n" +
		"host_file dd-policy /etc/dd-worker/policy.toml 0644 dd-worker.service dd-worker secret=false health=true\n" +
		"host_file dd-secrets /etc/dd-worker/secrets.env 0600 dd-worker.service dd-worker secret=true health=false\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(out, "/bin/sh") || strings.Contains(out, "--wait") {
		t.Fatalf("printed a command line: %q", out)
	}
}

// The installer tests' stub prints this golden file as policy check
// output; this test proves the real command prints exactly it. A missing
// executable is exec=unsafe on that line only (rev 1a A2).
func TestPolicyCheckHostFilesGolden(t *testing.T) {
	withAccounts(t)
	path := writePolicy(t, ""+
		"host_files = [\"dd-policy=/etc/dd-worker/policy.toml\", \"dd-secrets=/etc/dd-worker/secrets.env\", \"app=/etc/app/app.conf\"]\n"+
		"host_file_modes = [\"dd-policy:0644\", \"dd-secrets:0600\", \"app:0640\"]\n"+
		"host_file_secret = [\"dd-secrets\"]\n"+
		"host_file_units = [\"dd-policy:dd-worker.service\", \"dd-secrets:dd-worker.service\", \"app:app.service\"]\n"+
		"host_file_run_as = [\"dd-policy:dd-worker\", \"dd-secrets:dd-worker\", \"app:dd-worker\"]\n"+
		"host_file_checks = [\"dd-policy:/bin/sh policy check --file -\", \"dd-secrets:/bin/sh secrets check --file -\", \"app:/nonexistent/app-check\"]\n"+
		"host_file_health = [\"dd-policy:/bin/sh health --wait 30s\", \"dd-secrets:/bin/sh health --wait 30s\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatalf("%v %q", err, out)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "packaging", "testdata", "hostfiles-policy-check.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestPolicyCheckHostFilesNone(t *testing.T) {
	out, err := runPolicyCheck(t, writePolicy(t, "enabled = true\n"))
	if err != nil || !strings.HasSuffix(out, "edge=false\nhost_files=0\n") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestPolicyCheckHostFilesInvalid(t *testing.T) {
	withAccounts(t)
	for name, body := range map[string]string{
		"reserved dir":  "host_files = [\"a=/etc/ssh/a\"]\nhost_file_modes = [\"a:0644\"]\nhost_file_units = [\"a:x.service\"]\nhost_file_run_as = [\"a:dd-worker\"]\nhost_file_checks = [\"a:/bin/sh\"]\n",
		"root run_as":   "host_files = [\"a=/etc/x/a\"]\nhost_file_modes = [\"a:0644\"]\nhost_file_units = [\"a:x.service\"]\nhost_file_run_as = [\"a:root\"]\nhost_file_checks = [\"a:/bin/sh\"]\n",
		"missing check": "host_files = [\"a=/etc/x/a\"]\nhost_file_modes = [\"a:0644\"]\nhost_file_units = [\"a:x.service\"]\nhost_file_run_as = [\"a:dd-worker\"]\n",
	} {
		out, err := runPolicyCheck(t, writePolicy(t, body))
		if err == nil || out != "invalid: parse\n" {
			t.Fatalf("%s: %v %q", name, err, out)
		}
	}
}
