package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestUnknownKeyIsInvalid(t *testing.T) {
	body := []byte("enabled = true\nnot_a_key = [\"ghcr.io/joe/*\"]\n")
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

func TestImagePolicy(t *testing.T) {
	const base = "enabled = true\nimages = [\"ghcr.io/joe-and-his-friends/*\"]\nprojects = [\"mohiguide\"]\n"
	cases := []struct {
		name    string
		body    string
		reason  string
		enabled bool
		max     int
		public  bool
		images  int
		projs   int
		on      bool
	}{
		{"defaults", base, "", true, 20, false, 1, 1, true},
		{"explicit", base + "max_containers = 200\nallow_public_ports = true\n", "", true, 200, true, 1, 1, true},
		{"floor", base + "max_containers = 1\n", "", true, 1, false, 1, 1, true},
		{"zero", base + "max_containers = 0\n", "parse", false, 0, false, 0, 0, false},
		{"high", base + "max_containers = 201\n", "parse", false, 0, false, 0, 0, false},
		{"star", "enabled = true\nimages = [\"*\"]\nprojects = [\"mohiguide\"]\n", "parse", false, 0, false, 0, 0, false},
		{"host star", "enabled = true\nimages = [\"*.io/a/*\"]\nprojects = [\"a\"]\n", "parse", false, 0, false, 0, 0, false},
		{"upper", "enabled = true\nimages = [\"GHCR.IO/a/*\"]\nprojects = [\"a\"]\n", "parse", false, 0, false, 0, 0, false},
		{"bad project", "enabled = true\nimages = [\"ghcr.io/a/*\"]\nprojects = [\"../x\"]\n", "parse", false, 0, false, 0, 0, false},
		{"dup project", "enabled = true\nimages = [\"ghcr.io/a/*\"]\nprojects = [\"mohiguide\", \"mohiguide\"]\n", "parse", false, 0, false, 0, 0, false},
		{"empty images", "enabled = true\nimages = []\nprojects = [\"mohiguide\"]\n", "", true, 20, false, 0, 1, false},
		{"empty projects", "enabled = true\nimages = [\"ghcr.io/a/*\"]\nprojects = []\n", "", true, 20, false, 1, 0, false},
		{"disabled", "enabled = false\nimages = [\"ghcr.io/a/*\"]\nprojects = [\"mohiguide\"]\n", "", false, 20, false, 1, 1, false},
		{"bad bool", "allow_public_ports = yes\n", "parse", false, 0, false, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := loadBytes(t, []byte(tc.body), 0o644, rootUID)
			if p.Reason != tc.reason || p.Valid != (tc.reason == "") || p.Enabled != tc.enabled {
				t.Fatalf("valid %v enabled %v reason %s", p.Valid, p.Enabled, p.Reason)
			}
			if p.MaxContainers != tc.max || p.AllowPublicPorts != tc.public {
				t.Fatalf("max %d public %v", p.MaxContainers, p.AllowPublicPorts)
			}
			if len(p.Images) != tc.images || len(p.Projects) != tc.projs || p.ImagesEnabled() != tc.on {
				t.Fatalf("images %v projects %v on %v", p.Images, p.Projects, p.ImagesEnabled())
			}
		})
	}
}

func TestOverlappingProjects(t *testing.T) {
	const head = "enabled = true\nimages = [\"ghcr.io/acme/*\"]\n"
	cases := []struct {
		name     string
		projects string
		on       bool
		a, b     string
	}{
		{"acme and acme-web", `["acme", "acme-web"]`, false, "acme", "acme-web"},
		{"longer suffix", `["acme", "acme-web-api"]`, false, "acme", "acme-web-api"},
		{"reversed", `["acme-web", "acme"]`, false, "acme-web", "acme"},
		{"acme and acmeweb", `["acme", "acmeweb"]`, true, "", ""},
		{"acme-web and web", `["acme-web", "web"]`, true, "", ""},
		{"trailing hyphen", `["acme", "acme-"]`, true, "", ""},
		{"three, one pair", `["web", "acme", "acme-web"]`, false, "acme", "acme-web"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := head + "projects = " + tc.projects + "\n"
			p := loadBytes(t, []byte(body), 0o644, rootUID)
			if !p.Valid || !p.Enabled || p.Reason != "" {
				t.Fatalf("valid %v enabled %v reason %s", p.Valid, p.Enabled, p.Reason)
			}
			if p.ImagesEnabled() != tc.on {
				t.Fatalf("images on %v", p.ImagesEnabled())
			}
			a, b, ok := OverlappingProjects(p.Projects)
			if ok != !tc.on || a != tc.a || b != tc.b {
				t.Fatalf("pair %q %q ok %v", a, b, ok)
			}
		})
	}
	// The agent keeps a Policy value. The same function decides images.
	built := Policy{
		Valid: true, Enabled: true,
		Images:   []string{"ghcr.io/acme/*"},
		Projects: []string{"acme", "other", "acme-web"},
	}
	if built.ImagesEnabled() {
		t.Fatal("struct literal stayed on")
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
