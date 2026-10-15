package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestUnknownKeyIsInvalid(t *testing.T) {
	body := []byte("enabled = true\nimages = [\"ghcr.io/joe/*\"]\n")
	p := loadBytes(t, body, 0o644, rootUID)
	if p.Valid || p.Enabled || p.Reason != "unknown_key" {
		t.Fatalf("valid=%v enabled=%v reason=%s", p.Valid, p.Enabled, p.Reason)
	}
	if len(p.RunAs) != 0 || len(p.Dirs) != 0 {
		t.Fatalf("invalid policy kept contents %+v", p)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
}

func TestNonRootOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	body := []byte("enabled = true\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: path, Passwd: filepath.Join(dir, "missing"), Group: filepath.Join(dir, "missing")})
	if !p.Exists || p.Valid || p.Reason != "not_root" {
		t.Fatalf("%+v", p)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
}

func TestGroupAndWorldWritable(t *testing.T) {
	cases := []struct {
		mode os.FileMode
		want string
	}{
		{0o664, "group_writable"},
		{0o642, "world_writable"},
	}
	for _, tc := range cases {
		p := loadBytes(t, []byte("enabled = true\n"), tc.mode, rootUID)
		if p.Valid || p.Enabled || p.Reason != tc.want {
			t.Fatalf("mode %o: %+v", tc.mode, p)
		}
	}
}

func TestRunAsDropsRootLowUIDAndMissing(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(passwd, ""+
		"root:x:0:0:root:/root:/bin/bash\n"+
		"daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n"+
		"joe:x:1000:1000:Joe:/home/joe:/bin/bash\n")
	write(group, "root:x:0:\ndaemon:x:1:\njoe:x:1000:\n")
	body := []byte("enabled = true\nrun_as = [\"root\", \"daemon\", \"ghost\", \"joe\", \"joe\"]\n")
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: path, Passwd: passwd, Group: group, UID: rootUID})
	if !p.Valid || !p.Enabled {
		t.Fatalf("%+v", p)
	}
	if len(p.RunAs) != 1 || p.RunAs[0] != "joe" {
		t.Fatalf("kept %v", p.RunAs)
	}
	got := map[string]string{}
	for _, d := range p.Dropped {
		got[d.Name] = d.Reason
	}
	if got["root"] != "root" || got["daemon"] != "uid" || got["ghost"] != "missing" {
		t.Fatalf("dropped %+v", p.Dropped)
	}
	if _, ok := p.Accounts["joe"]; !ok || p.Accounts["joe"].Home != "/home/joe" || p.Accounts["joe"].Group != "joe" {
		t.Fatalf("account %+v", p.Accounts["joe"])
	}
}

func TestRunAsDropsLowGID(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\nbuilder:x:1000:0:B:/home/builder:/bin/bash\nwheel:x:1000:10:W:/home/wheel:/bin/bash\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\nusers:x:10:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := []byte("enabled = true\nrun_as = [\"builder\", \"wheel\", \"joe\"]\n")
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: path, Passwd: passwd, Group: group, UID: rootUID})
	if len(p.RunAs) != 1 || p.RunAs[0] != "joe" {
		t.Fatalf("kept %v", p.RunAs)
	}
	got := map[string]string{}
	for _, d := range p.Dropped {
		got[d.Name] = d.Reason
	}
	if got["builder"] != "gid" || got["wheel"] != "gid" {
		t.Fatalf("dropped %+v", p.Dropped)
	}
}

func TestMissingFileIsAbsent(t *testing.T) {
	p := Load(Files{Policy: filepath.Join(t.TempDir(), "no-such.toml")})
	if p.Exists || p.Valid || p.Enabled || p.Hash != "" || p.Reason != "" {
		t.Fatalf("%+v", p)
	}
}

func TestValidDisabledKeepsHash(t *testing.T) {
	body := []byte("# disabled by default\nenabled = false\nrun_as = [\"joe\"]\nallow_shell = true\nmax_programs = 20\n")
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte("joe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: path, Passwd: passwd, Group: filepath.Join(dir, "group"), UID: rootUID})
	if !p.Valid || p.Enabled || !p.AllowShell || p.MaxPrograms != 20 {
		t.Fatalf("%+v", p)
	}
	if len(p.RunAs) != 1 || p.RunAs[0] != "joe" {
		t.Fatalf("run_as %v", p.RunAs)
	}
	if p.Hash != hexHash(body) {
		t.Fatalf("hash %s", p.Hash)
	}
}

func TestParseRejectsWrongTypes(t *testing.T) {
	for _, body := range []string{
		"enabled = yes\n",
		"max_programs = -1\n",
		"run_as = \"joe\"\n",
		"enabled = true\nenabled = false\n",
		"dirs = [joe]\n",
	} {
		if _, reason := parseTOML([]byte(body)); reason != "parse" {
			t.Fatalf("%q reason %s", body, reason)
		}
	}
}

func rootUID(os.FileInfo) (uint32, bool) { return 0, true }

func loadBytes(t *testing.T, body []byte, mode os.FileMode, uid func(os.FileInfo) (uint32, bool)) Policy {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return Load(Files{Policy: path, Passwd: filepath.Join(dir, "passwd"), Group: filepath.Join(dir, "group"), UID: uid})
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
