package policy

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// agent-protocol v1.21 item 1: host_network_projects and host_env_files.

const hostEnvBase = "enabled = true\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"deploybot\", \"other\"]\n"

const hostEnvSecret = "s3cr3t-database-url-value"

// apiImages binds the entries the tests use to ghcr.io/acme/api.
const apiImages = `host_env_images = ["deploybot/api=ghcr.io/acme/api", "deploybot/migrate=ghcr.io/acme/*"]` + "\n"

// rootOwner makes every host env file root:root for the loader.
func rootOwner(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true }

// hostEnvFixture writes policy plus an env file at root/etc/deploybot/api.env.
type hostEnvFixture struct {
	t      *testing.T
	dir    string
	root   string
	policy string
	file   string
}

func newHostEnvFixture(t *testing.T) *hostEnvFixture {
	t.Helper()
	dir := t.TempDir()
	f := &hostEnvFixture{t: t, dir: dir, root: filepath.Join(dir, "root"), policy: filepath.Join(dir, "runner.toml")}
	f.file = filepath.Join(f.root, "etc", "deploybot", "api.env")
	if err := os.MkdirAll(filepath.Dir(f.file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.writeEnv("# api\nDATABASE_URL="+hostEnvSecret+"\n\nsecrets_key=abc=def\n", 0o600)
	return f
}

func (f *hostEnvFixture) writeEnv(body string, mode os.FileMode) {
	f.t.Helper()
	_ = os.Remove(f.file)
	if err := os.WriteFile(f.file, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(f.file, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hostEnvFixture) load(extra string, owner HostEnvOwner, skip bool) Policy {
	f.t.Helper()
	if err := os.WriteFile(f.policy, []byte(hostEnvBase+extra), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return Load(Files{Policy: f.policy, UID: rootUID, EnvRoot: f.root, EnvOwner: owner, SkipEnvFiles: skip})
}

func TestHostNetworkProjects(t *testing.T) {
	f := newHostEnvFixture(t)
	p := f.load("", rootOwner, false)
	if !p.Valid || p.HostNetworkProjects == nil || len(p.HostNetworkProjects) != 0 || p.HostNetworkEnabled() {
		t.Fatalf("absent: %+v", p)
	}
	p = f.load(`host_network_projects = ["deploybot"]`+"\n", rootOwner, false)
	if !p.Valid || !p.HostNetworkEnabled() || !p.HostNetworkAllowed("deploybot") || p.HostNetworkAllowed("other") {
		t.Fatalf("listed: %+v", p)
	}
	cases := map[string]string{
		"not in projects": `host_network_projects = ["nope"]`,
		"duplicate":       `host_network_projects = ["deploybot", "deploybot"]`,
		"not an array":    `host_network_projects = "deploybot"`,
		"too many":        `host_network_projects = ["a","b","c","d","e","f","g","h","i"]`,
	}
	for name, line := range cases {
		p := f.load(line+"\n", rootOwner, false)
		if p.Valid || p.Reason != "parse" || p.HostNetworkEnabled() || p.HostNetworkAllowed("deploybot") {
			t.Fatalf("%s: %+v", name, p)
		}
	}
}

func TestHostEnvFilesValid(t *testing.T) {
	f := newHostEnvFixture(t)
	p := f.load(`host_env_files = ["deploybot/api=/etc/deploybot/api.env", "deploybot/migrate=/etc/deploybot/api.env"]`+"\n"+apiImages, rootOwner, false)
	if !p.Valid {
		t.Fatalf("reason %s %s", p.Reason, p.ReasonEntry)
	}
	if len(p.HostEnvFiles) != 2 {
		t.Fatalf("%+v", p.HostEnvFiles)
	}
	got, ok := p.HostEnv("deploybot", "api")
	if !ok || got.Keys != 2 || len(got.SHA256) != 64 || got.Path != "/etc/deploybot/api.env" || got.Label() != "deploybot/api" || got.Image != "ghcr.io/acme/api" {
		t.Fatalf("%+v", got)
	}
	if _, ok := p.HostEnv("deploybot", "web"); ok {
		t.Fatal("unlisted entry found")
	}
	env, sum, err := ReadHostEnv(f.root, "/etc/deploybot/api.env", rootOwner)
	if err != nil || sum != got.SHA256 || env["DATABASE_URL"] != hostEnvSecret || env["secrets_key"] != "abc=def" || len(env) != 2 {
		t.Fatalf("%v %v", err, len(env))
	}
}

// The agent cannot read the file: SkipEnvFiles checks the lines only.
func TestHostEnvFilesAgentSkipsContents(t *testing.T) {
	f := newHostEnvFixture(t)
	f.writeEnv("not a valid line\n", 0o644)
	p := f.load(`host_env_files = ["deploybot/api=/etc/deploybot/api.env"]`+"\n"+`host_env_images = ["deploybot/api=ghcr.io/acme/api"]`+"\n", nil, true)
	if !p.Valid || len(p.HostEnvFiles) != 1 || p.HostEnvFiles[0].Keys != 0 || p.HostEnvFiles[0].SHA256 != "" {
		t.Fatalf("%+v", p)
	}
	p = f.load(`host_env_files = ["nope/api=/etc/deploybot/api.env"]`+"\n"+`host_env_images = ["nope/api=ghcr.io/acme/api"]`+"\n", nil, true)
	if p.Valid || p.Reason != ReasonHostEnvFile {
		t.Fatalf("line still checked: %+v", p)
	}
}

func TestHostEnvFilesInvalid(t *testing.T) {
	const line = `host_env_files = ["deploybot/api=/etc/deploybot/api.env"]` + "\n"
	type tc struct {
		policy string
		images string
		setup  func(f *hostEnvFixture)
		owner  HostEnvOwner
		reason string
		entry  string
	}
	notRoot := func(os.FileInfo) (uint32, uint32, bool) { return 1000, 0, true }
	notRootGroup := func(os.FileInfo) (uint32, uint32, bool) { return 0, 1000, true }
	cases := map[string]tc{
		"no image line":        {policy: line, images: "-", reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"image line only":      {policy: "", images: `host_env_images = ["deploybot/api=ghcr.io/acme/api"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"image outside images": {policy: line, images: `host_env_images = ["deploybot/api=ghcr.io/other/api"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"image wider":          {policy: line, images: `host_env_images = ["deploybot/api=ghcr.io/*/api"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"bad image pattern":    {policy: line, images: `host_env_images = ["deploybot/api=ghcr.io/acme/api@sha256"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"duplicate image":      {policy: line, images: `host_env_images = ["deploybot/api=ghcr.io/acme/api", "deploybot/api=ghcr.io/acme/web"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"image no equals":      {policy: line, images: `host_env_images = ["deploybot/api"]`, reason: "parse"},
		"etc writable": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			if err := os.Chmod(filepath.Join(f.root, "etc"), 0o777); err != nil {
				f.t.Fatal(err)
			}
		}},
		"dir group writable": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			if err := os.Chmod(filepath.Dir(f.file), 0o770); err != nil {
				f.t.Fatal(err)
			}
		}},
		"dir is a link": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			real := filepath.Join(f.dir, "real")
			if err := os.Rename(filepath.Dir(f.file), real); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Dir(f.file)); err != nil {
				f.t.Fatal(err)
			}
		}},
		"dir not root": {policy: line, owner: func(fi os.FileInfo) (uint32, uint32, bool) {
			if fi.IsDir() {
				return 1000, 0, true
			}
			return 0, 0, true
		}, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"slug not in projects": {policy: `host_env_files = ["nope/api=/etc/deploybot/api.env"]`, reason: ReasonHostEnvFile, entry: "nope/api"},
		"bad entry name":       {policy: `host_env_files = ["deploybot/API=/etc/deploybot/api.env"]`, reason: ReasonHostEnvFile},
		"entry -prev":          {policy: `host_env_files = ["deploybot/api-prev=/etc/deploybot/api.env"]`, reason: ReasonHostEnvFile},
		"no equals":            {policy: `host_env_files = ["deploybot/api"]`, reason: "parse"},
		"no slash":             {policy: `host_env_files = ["api=/etc/deploybot/api.env"]`, reason: "parse"},
		"duplicate entry":      {policy: `host_env_files = ["deploybot/api=/etc/deploybot/api.env", "deploybot/api=/etc/deploybot/api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"relative path":        {policy: `host_env_files = ["deploybot/api=etc/deploybot/api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"not under etc":        {policy: `host_env_files = ["deploybot/api=/var/deploybot/api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"directly in etc":      {policy: `host_env_files = ["deploybot/api=/etc/api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"too deep":             {policy: `host_env_files = ["deploybot/api=/etc/a/b/api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"dotdot":               {policy: `host_env_files = ["deploybot/api=/etc/../api.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"dotdot name":          {policy: `host_env_files = ["deploybot/api=/etc/deploybot/..env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"not .env":             {policy: `host_env_files = ["deploybot/api=/etc/deploybot/api.conf"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"missing file":         {policy: `host_env_files = ["deploybot/api=/etc/deploybot/gone.env"]`, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"mode 0644": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A=b\n", 0o644)
		}},
		"mode 0400": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A=b\n", 0o400)
		}},
		"wrong owner":        {policy: line, owner: notRoot, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"wrong group":        {policy: line, owner: notRootGroup, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"real owner is user": {policy: line, owner: nil, reason: ReasonHostEnvFile, entry: "deploybot/api"},
		"symlink": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			target := filepath.Join(f.dir, "target.env")
			if err := os.WriteFile(target, []byte("A=b\n"), 0o600); err != nil {
				f.t.Fatal(err)
			}
			_ = os.Remove(f.file)
			if err := os.Symlink(target, f.file); err != nil {
				f.t.Fatal(err)
			}
		}},
		"hard link": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			if err := os.Link(f.file, filepath.Join(f.dir, "second")); err != nil {
				f.t.Fatal(err)
			}
		}},
		"fifo": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			_ = os.Remove(f.file)
			if err := syscall.Mkfifo(f.file, 0o600); err != nil {
				f.t.Fatal(err)
			}
		}},
		"directory": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			_ = os.Remove(f.file)
			if err := os.Mkdir(f.file, 0o700); err != nil {
				f.t.Fatal(err)
			}
		}},
		"too big": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A="+strings.Repeat("x", MaxHostEnvBytes)+"\n", 0o600)
		}},
		"bad key": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("1BAD="+hostEnvSecret+"\n", 0o600)
		}},
		"key with space": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A B="+hostEnvSecret+"\n", 0o600)
		}},
		"no equals in file": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv(hostEnvSecret+"\n", 0o600)
		}},
		"duplicate key": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A="+hostEnvSecret+"\nA=other\n", 0o600)
		}},
		"carriage return": {policy: line, reason: ReasonHostEnvFile, entry: "deploybot/api", setup: func(f *hostEnvFixture) {
			f.writeEnv("A="+hostEnvSecret+"\r\n", 0o600)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHostEnvFixture(t)
			if c.setup != nil {
				c.setup(f)
			}
			owner := c.owner
			if owner == nil && name != "real owner is user" {
				owner = rootOwner
			}
			if name == "real owner is user" && os.Geteuid() == 0 {
				t.Skip("running as root")
			}
			imgs := c.images
			if imgs == "" {
				imgs = `host_env_images = ["deploybot/api=ghcr.io/acme/api", "nope/api=ghcr.io/acme/api"]`
			}
			if imgs == "-" {
				imgs = ""
			}
			p := f.load(c.policy+"\n"+imgs+"\n", owner, false)
			if p.Valid || p.Reason != c.reason || p.ReasonEntry != c.entry {
				t.Fatalf("valid=%v reason=%q entry=%q", p.Valid, p.Reason, p.ReasonEntry)
			}
			if p.HostEnvFiles != nil || p.HostNetworkProjects != nil {
				t.Fatalf("invalid policy kept lists %+v", p)
			}
			if strings.Contains(p.Reason+p.ReasonEntry, hostEnvSecret) || strings.Contains(p.ReasonEntry, "/etc") {
				t.Fatalf("reason leaks: %q %q", p.Reason, p.ReasonEntry)
			}
		})
	}
	t.Run("too many", func(t *testing.T) {
		f := newHostEnvFixture(t)
		items := make([]string, MaxHostEnvFiles+1)
		for i := range items {
			items[i] = `"deploybot/e` + strings.Repeat("a", i+1) + `=/etc/deploybot/api.env"`
		}
		p := f.load("host_env_files = ["+strings.Join(items, ", ")+"]\n"+apiImages, rootOwner, false)
		if p.Valid || p.Reason != "parse" {
			t.Fatalf("%+v", p.Reason)
		}
	})
}

func TestHostEnvKeyDenied(t *testing.T) {
	for _, k := range []string{"PATH", "HOME", "NODE_OPTIONS", "LD_PRELOAD", "PYTHONPATH"} {
		if !HostEnvKeyDenied(k) {
			t.Fatalf("%s allowed", k)
		}
	}
	for _, k := range []string{"DATABASE_URL", "node_env", "_X"} {
		if HostEnvKeyDenied(k) {
			t.Fatalf("%s denied", k)
		}
	}
}

// An older agent knows neither key: unknown_key, the existing path.
func TestHostEnvKeysAreKnown(t *testing.T) {
	for _, k := range []string{"host_network_projects", "host_env_files", "host_env_images"} {
		if !knownKey(k) {
			t.Fatal(k)
		}
	}
}

// hostFileLines is a v1.18 host file at path, with the keys it requires.
func hostFileLines(path string) string {
	return "host_files = [\"cfg=" + path + "\"]\n" +
		"host_file_modes = [\"cfg:0600\"]\n" +
		"host_file_units = [\"cfg:app.service\"]\n" +
		"host_file_run_as = [\"cfg:svc\"]\n" +
		"host_file_checks = [\"cfg:/bin/true\"]\n"
}

// A host env file in another /etc directory than a host file is fine.
func TestHostEnvNoOverlapOtherDir(t *testing.T) {
	f := newHostEnvFixture(t)
	passwd := filepath.Join(f.dir, "passwd")
	group := filepath.Join(f.dir, "group")
	if err := os.WriteFile(passwd, []byte("svc:x:994:989::/:/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("svc:x:989:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := hostEnvBase + `host_env_files = ["deploybot/api=/etc/deploybot/api.env"]` + "\n" +
		`host_env_images = ["deploybot/api=ghcr.io/acme/api"]` + "\n" + hostFileLines("/etc/app/app.conf")
	if err := os.WriteFile(f.policy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(Files{Policy: f.policy, Passwd: passwd, Group: group, UID: rootUID, EnvRoot: f.root, EnvOwner: rootOwner})
	if !p.Valid {
		t.Fatalf("%s %s", p.Reason, p.ReasonEntry)
	}
	// A host file in /etc/deploybot, or at the env file's own path,
	// overlaps (v1.21 item 7), also for a line-only load.
	for _, hf := range []string{"/etc/deploybot/app.conf", "/etc/deploybot/api.env"} {
		over := strings.Replace(body, "/etc/app/app.conf", hf, 1)
		if err := os.WriteFile(f.policy, []byte(over), 0o644); err != nil {
			t.Fatal(err)
		}
		p = Load(Files{Policy: f.policy, Passwd: passwd, Group: group, UID: rootUID, EnvRoot: f.root, EnvOwner: rootOwner, SkipEnvFiles: true})
		if p.Valid || p.Reason != ReasonHostEnvFile || p.ReasonEntry != "deploybot/api" {
			t.Fatalf("%s: %v %s %s", hf, p.Valid, p.Reason, p.ReasonEntry)
		}
	}
}
