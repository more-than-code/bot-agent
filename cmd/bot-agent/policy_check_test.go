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
