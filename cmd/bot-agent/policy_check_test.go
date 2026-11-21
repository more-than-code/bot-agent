package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
	return path
}

func runPolicyCheck(t *testing.T, path string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := dispatch([]string{"policy", "check", path}, &buf)
	return buf.String(), err
}

func TestPolicyCheckValid(t *testing.T) {
	path := writePolicy(t, ""+
		"enabled = true\n"+
		"images = [\"ghcr.io/acme/*\"]\n"+
		"projects = [\"mohiguide\"]\n"+
		"max_containers = 4\n"+
		"allow_public_ports = true\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"valid\n",
		"enabled=true\n",
		"programs=true\n",
		"images=true\n",
		"projects=1\n",
		"max_containers=4\n",
		"allow_public_ports=true\n",
		"image_patterns=1\n",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("missing %q in %q", line, out)
		}
	}
	if strings.Contains(out, "ghcr.io") || strings.Contains(out, "mohiguide") {
		t.Fatalf("printed file content: %q", out)
	}
}

func TestPolicyCheckMultiLineArray(t *testing.T) {
	path := writePolicy(t, "enabled = true\nimages = [\n  \"ghcr.io/acme/*\"\n]\n")
	out, err := runPolicyCheck(t, path)
	if err == nil {
		t.Fatal("expected invalid")
	}
	if out != "invalid: parse\n" {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckTrailingComma(t *testing.T) {
	path := writePolicy(t, "enabled = true\nimages = [\"ghcr.io/acme/*\",]\n")
	out, err := runPolicyCheck(t, path)
	if err == nil {
		t.Fatal("expected invalid")
	}
	if out != "invalid: parse\n" {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckOverlappingProjects(t *testing.T) {
	path := writePolicy(t, ""+
		"enabled = true\n"+
		"images = [\"ghcr.io/acme/*\"]\n"+
		"projects = [\"acme\", \"acme-web\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "valid\n") || !strings.Contains(out, "images=false\n") || !strings.Contains(out, "projects=2\n") || !strings.Contains(out, "programs=true\n") {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckUnknownKey(t *testing.T) {
	path := writePolicy(t, "enabled = true\nnope = true\n")
	out, err := runPolicyCheck(t, path)
	if err == nil {
		t.Fatal("expected invalid")
	}
	if out != "invalid: unknown_key\n" {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckEnabledFalse(t *testing.T) {
	path := writePolicy(t, "enabled = false\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "valid\n") || !strings.Contains(out, "enabled=false\n") || !strings.Contains(out, "programs=false\n") || !strings.Contains(out, "images=false\n") {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckImagesWithoutProjects(t *testing.T) {
	path := writePolicy(t, "enabled = true\nimages = [\"ghcr.io/acme/*\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "valid\n") || !strings.Contains(out, "images=false\n") || !strings.Contains(out, "image_patterns=1\n") || !strings.Contains(out, "projects=0\n") {
		t.Fatalf("%q", out)
	}
}

func TestPolicyCheckUsage(t *testing.T) {
	var buf bytes.Buffer
	if err := dispatch([]string{"policy"}, &buf); err == nil || !strings.Contains(err.Error(), "policy check") {
		t.Fatalf("%v %q", err, buf.String())
	}
	buf.Reset()
	if err := dispatch([]string{"policy", "check"}, &buf); err == nil {
		t.Fatal("missing file")
	}
	buf.Reset()
	if err := dispatch([]string{"policy", "check", "a", "b"}, &buf); err == nil {
		t.Fatal("extra arg")
	}
}

func TestPolicyCheckOmitsAccountNames(t *testing.T) {
	const kept = "kept-account-name"
	const missing = "missing-account-name"
	path := writePolicy(t, "enabled = true\nrun_as = [\""+kept+"\", \""+missing+"\"]\n")
	out, err := runPolicyCheck(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "valid\n") {
		t.Fatalf("%q", out)
	}
	if strings.Contains(out, kept) || strings.Contains(out, missing) || strings.Contains(out, "run_as") {
		t.Fatalf("account names in output: %q", out)
	}
}

// v1.12 item 6: one added line, projects_overlap, after the existing lines.
func TestPolicyCheckProjectsOverlap(t *testing.T) {
	for _, tc := range []struct {
		projects string
		want     string
	}{
		{`["acme", "acme-web"]`, "projects_overlap=true\n"},
		{`["acme", "acmeweb"]`, "projects_overlap=false\n"},
		{`[]`, "projects_overlap=false\n"},
	} {
		out, err := runPolicyCheck(t, writePolicy(t, "enabled = true\nprojects = "+tc.projects+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(out, "image_patterns=0\n"+tc.want+"shared_networks=0\nedge=false\nhost_files=0\n") {
			t.Fatalf("%s: %q", tc.projects, out)
		}
		if strings.Contains(out, "acme") {
			t.Fatalf("printed a slug: %q", out)
		}
	}
}

// v1.13 item 6: shared_networks=<count> after projects_overlap, then one
// line per network in file order, "-" when it has no members.
func TestPolicyCheckSharedNetworks(t *testing.T) {
	out, err := runPolicyCheck(t, writePolicy(t, ""+
		"enabled = true\n"+
		"projects = [\"daikou\", \"defect-drainer\"]\n"+
		"shared_networks = [\"dd=10.200.10.0/24\", \"spare=192.168.50.0/28\"]\n"+
		"shared_network_projects = [\"dd:daikou\", \"dd:defect-drainer\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "projects_overlap=false\n" +
		"shared_networks=2\n" +
		"shared_network dd 10.200.10.0/24 daikou,defect-drainer\n" +
		"shared_network spare 192.168.50.0/28 -\n" +
		"edge=false\n" +
		"host_files=0\n"
	if !strings.HasSuffix(out, "image_patterns=0\n"+want) {
		t.Fatalf("%q", out)
	}
	out, err = runPolicyCheck(t, writePolicy(t, "enabled = true\n"))
	if err != nil || !strings.HasSuffix(out, "projects_overlap=false\nshared_networks=0\nedge=false\nhost_files=0\n") {
		t.Fatalf("%q %v", out, err)
	}
	out, err = runPolicyCheck(t, writePolicy(t, "enabled = true\nshared_networks = [\"dd=8.8.8.0/24\"]\n"))
	if err == nil || out != "invalid: parse\n" {
		t.Fatalf("%q %v", out, err)
	}
}

// v1.15 item 1: edge=<true|false> follows the shared networks; the
// v1.18 host_files lines come after it. An edge key that is not a bool makes the file invalid.
func TestPolicyCheckEdge(t *testing.T) {
	out, err := runPolicyCheck(t, writePolicy(t, ""+
		"projects = [\"daikou\"]\n"+
		"shared_networks = [\"dd=10.200.10.0/24\"]\n"+
		"shared_network_projects = [\"dd:daikou\"]\n"+
		"edge = true\n"))
	if err != nil || !strings.HasSuffix(out, "shared_network dd 10.200.10.0/24 daikou\nedge=true\nhost_files=0\n") {
		t.Fatalf("%q %v", out, err)
	}
	out, err = runPolicyCheck(t, writePolicy(t, "edge = yes\n"))
	if err == nil || out != "invalid: parse\n" {
		t.Fatalf("%q %v", out, err)
	}
}
