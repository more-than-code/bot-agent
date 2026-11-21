package policy

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeEntry is one path in fakeFS. link is set for a symlink.
type fakeEntry struct {
	mode os.FileMode
	uid  uint32
	link string
}

type fakeFS map[string]fakeEntry

type fakeInfo struct {
	name string
	e    fakeEntry
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() os.FileMode  { return i.e.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.e.mode.IsDir() }
func (i fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: i.e.uid} }

func (f fakeFS) Lstat(name string) (os.FileInfo, error) {
	e, ok := f[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fakeInfo{name: filepath.Base(name), e: e}, nil
}

func (f fakeFS) Readlink(name string) (string, error) {
	e, ok := f[name]
	if !ok || e.mode&os.ModeSymlink == 0 {
		return "", fs.ErrInvalid
	}
	return e.link, nil
}

// ddFS is DD's layout: /usr/local/bin/dd-worker is a root symlink to a
// versioned binary under /opt/dd-worker.
func ddFS() fakeFS {
	dir := os.ModeDir | 0o755
	return fakeFS{
		"/":                            {mode: dir},
		"/usr":                         {mode: dir},
		"/usr/local":                   {mode: dir},
		"/usr/local/bin":               {mode: dir},
		"/usr/local/bin/dd-worker":     {mode: os.ModeSymlink | 0o777, link: "../../../opt/dd-worker/rc6/dd-worker"},
		"/opt":                         {mode: dir},
		"/opt/dd-worker":               {mode: dir},
		"/opt/dd-worker/rc6":           {mode: dir},
		"/opt/dd-worker/rc6/dd-worker": {mode: 0o755},
		"/usr/bin":                     {mode: dir},
		"/usr/bin/true":                {mode: 0o755},
	}
}

const ddPolicy = `enabled = false
host_files        = ["dd-policy=/etc/dd-worker/policy.toml", "dd-secrets=/etc/dd-worker/secrets.env"]
host_file_modes   = ["dd-policy:0644", "dd-secrets:0600"]
host_file_secret  = ["dd-secrets"]
host_file_units   = ["dd-policy:dd-worker.service", "dd-secrets:dd-worker.service"]
host_file_run_as  = ["dd-policy:dd-worker", "dd-secrets:dd-worker"]
host_file_checks  = ["dd-policy:/usr/local/bin/dd-worker policy check --file -", "dd-secrets:/usr/local/bin/dd-worker secrets check --file -"]
host_file_health  = ["dd-policy:/usr/local/bin/dd-worker health --wait 30s", "dd-secrets:/usr/local/bin/dd-worker health --wait 30s"]
host_file_idle    = ["dd-policy:/usr/local/bin/dd-worker health --idle", "dd-secrets:/usr/local/bin/dd-worker health --idle"]
`

func loadHostPolicy(t *testing.T, body string, fsys ExecFS) Policy {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\ndaemon:x:1:1::/:/bin/false\ndd-worker:x:1500:1500::/var/lib/dd-worker:/usr/sbin/nologin\nlowgid:x:1600:100::/:/bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\nusers:x:100:\ndd-worker:x:1500:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(Files{Policy: path, Passwd: passwd, Group: group, SkipOwner: true, Exec: fsys})
}

func TestHostFilesValidDD(t *testing.T) {
	p := loadHostPolicy(t, ddPolicy, ddFS())
	if !p.Valid {
		t.Fatalf("invalid: %s", p.Reason)
	}
	if len(p.HostFiles) != 2 {
		t.Fatalf("files %+v", p.HostFiles)
	}
	a, b := p.HostFiles[0], p.HostFiles[1]
	if a.Name != "dd-policy" || a.Dir != "dd-worker" || a.File != "policy.toml" || a.Mode != 0o644 || a.Secret ||
		a.Unit != "dd-worker.service" || a.RunAs.UID != 1500 || a.RunAs.GID != 1500 ||
		strings.Join(a.Check, " ") != "/usr/local/bin/dd-worker policy check --file -" ||
		len(a.Health) != 4 || len(a.Idle) != 3 {
		t.Fatalf("dd-policy %+v", a)
	}
	if b.Name != "dd-secrets" || b.Mode != 0o600 || !b.Secret {
		t.Fatalf("dd-secrets %+v", b)
	}
	if _, ok := p.HostFile("dd-secrets"); !ok {
		t.Fatal("lookup")
	}
}

func TestHostFilesAbsentIsEmpty(t *testing.T) {
	p := loadHostPolicy(t, "enabled = true\n", ddFS())
	if !p.Valid || p.HostFiles == nil || len(p.HostFiles) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestHostFilesOptionalHealthAndIdle(t *testing.T) {
	body := `host_files = ["a=/etc/app/a.conf"]
host_file_modes = ["a:0640"]
host_file_units = ["a:app.service"]
host_file_run_as = ["a:dd-worker"]
host_file_checks = ["a:/usr/bin/true"]
`
	p := loadHostPolicy(t, body, ddFS())
	if !p.Valid || len(p.HostFiles) != 1 || p.HostFiles[0].Health != nil || p.HostFiles[0].Idle != nil {
		t.Fatalf("%+v", p)
	}
}

func TestHostFilesInvalid(t *testing.T) {
	base := map[string]string{
		"host_files":       `["a=/etc/app/a.conf"]`,
		"host_file_modes":  `["a:0644"]`,
		"host_file_units":  `["a:app.service"]`,
		"host_file_run_as": `["a:dd-worker"]`,
		"host_file_checks": `["a:/usr/bin/true"]`,
	}
	render := func(over map[string]string) string {
		var b strings.Builder
		keys := []string{"host_files", "host_file_modes", "host_file_secret", "host_file_units",
			"host_file_run_as", "host_file_checks", "host_file_health", "host_file_idle"}
		for _, k := range keys {
			v, ok := over[k]
			if !ok {
				v, ok = base[k]
			}
			if ok && v != "-" {
				b.WriteString(k + " = " + v + "\n")
			}
		}
		return b.String()
	}
	if p := loadHostPolicy(t, render(nil), ddFS()); !p.Valid {
		t.Fatalf("base invalid: %s", p.Reason)
	}
	groupWritable := ddFS()
	groupWritable["/opt/dd-worker/rc6/dd-worker"] = fakeEntry{mode: 0o775}
	linkDirWritable := ddFS()
	linkDirWritable["/usr/local/bin"] = fakeEntry{mode: os.ModeDir | 0o777}
	targetDirNotRoot := ddFS()
	targetDirNotRoot["/opt/dd-worker/rc6"] = fakeEntry{mode: os.ModeDir | 0o755, uid: 1500}
	linkNotRoot := ddFS()
	linkNotRoot["/usr/local/bin/dd-worker"] = fakeEntry{mode: os.ModeSymlink | 0o777, uid: 1500, link: "/opt/dd-worker/rc6/dd-worker"}
	notExec := ddFS()
	notExec["/usr/bin/true"] = fakeEntry{mode: 0o644}
	loop := ddFS()
	loop["/usr/local/bin/dd-worker"] = fakeEntry{mode: os.ModeSymlink | 0o777, link: "/usr/local/bin/dd-worker"}

	cases := []struct {
		name string
		over map[string]string
		fs   fakeFS
	}{
		{"reserved dir", map[string]string{"host_files": `["a=/etc/systemd/a.conf"]`}, nil},
		{"reserved dir sudoers", map[string]string{"host_files": `["a=/etc/sudoers.d/a"]`}, nil},
		{"three-level path", map[string]string{"host_files": `["a=/etc/app/sub/a.conf"]`}, nil},
		{"one-level path", map[string]string{"host_files": `["a=/etc/a.conf"]`}, nil},
		{"not etc", map[string]string{"host_files": `["a=/opt/app/a.conf"]`}, nil},
		{"dotdot", map[string]string{"host_files": `["a=/etc/../app/a.conf"]`}, nil},
		{"double slash", map[string]string{"host_files": `["a=/etc//app/a.conf"]`}, nil},
		{"trailing slash", map[string]string{"host_files": `["a=/etc/app/"]`}, nil},
		{"bad name", map[string]string{"host_files": `["A=/etc/app/a.conf"]`}, nil},
		{"duplicate path", map[string]string{"host_files": `["a=/etc/app/a.conf", "b=/etc/app/a.conf"]`}, nil},
		{"unknown name in modes", map[string]string{"host_file_modes": `["a:0644", "b:0644"]`}, nil},
		{"unknown name in secret", map[string]string{"host_file_secret": `["b"]`}, nil},
		{"missing mode", map[string]string{"host_file_modes": "-"}, nil},
		{"bad mode", map[string]string{"host_file_modes": `["a:0666"]`}, nil},
		{"missing unit", map[string]string{"host_file_units": "-"}, nil},
		{"missing run_as", map[string]string{"host_file_run_as": "-"}, nil},
		{"missing check", map[string]string{"host_file_checks": "-"}, nil},
		{"root run_as", map[string]string{"host_file_run_as": `["a:root"]`}, nil},
		{"system run_as", map[string]string{"host_file_run_as": `["a:daemon"]`}, nil},
		{"low gid run_as", map[string]string{"host_file_run_as": `["a:lowgid"]`}, nil},
		{"unknown run_as", map[string]string{"host_file_run_as": `["a:nosuch"]`}, nil},
		{"reserved unit deploybot", map[string]string{"host_file_units": `["a:deploybot-x.service"]`}, nil},
		{"reserved unit sshd", map[string]string{"host_file_units": `["a:sshd.service"]`}, nil},
		{"reserved unit bot-agent", map[string]string{"host_file_units": `["a:bot-agent-runner@1.service"]`}, nil},
		{"unit not service", map[string]string{"host_file_units": `["a:app.timer"]`}, nil},
		{"relative executable", map[string]string{"host_file_checks": `["a:dd-worker check"]`}, nil},
		{"double space", map[string]string{"host_file_checks": `["a:/usr/bin/true  x"]`}, nil},
		{"too many args", map[string]string{"host_file_checks": `["a:/usr/bin/true 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17"]`}, nil},
		{"secret 0644", map[string]string{"host_file_secret": `["a"]`}, nil},
		{"health wait 170s", map[string]string{"host_file_health": `["a:/usr/bin/true health --wait 170s"]`}, nil},
		{"health wait unreadable", map[string]string{"host_file_health": `["a:/usr/bin/true health --wait soon"]`}, nil},
		{"unit starts with dash", map[string]string{"host_file_units": `["a:-app.service"]`}, nil},
		{"unit starts with dot", map[string]string{"host_file_units": `["a:.app.service"]`}, nil},
		{"reserved unit getty", map[string]string{"host_file_units": `["a:getty@tty1.service"]`}, nil},
		{"reserved unit NetworkManager", map[string]string{"host_file_units": `["a:NetworkManager.service"]`}, nil},
		{"reserved unit user@", map[string]string{"host_file_units": `["a:user@1000.service"]`}, nil},
		{"reserved unit tailscaled", map[string]string{"host_file_units": `["a:tailscaled.service"]`}, nil},
		{"reserved dir tmpfiles.d", map[string]string{"host_files": `["a=/etc/tmpfiles.d/a.conf"]`}, nil},
		{"reserved dir cron.daily", map[string]string{"host_files": `["a=/etc/cron.daily/a"]`}, nil},
		{"reserved dir default", map[string]string{"host_files": `["a=/etc/default/a"]`}, nil},
		{"reserved dir udev", map[string]string{"host_files": `["a=/etc/udev/a"]`}, nil},
		{"dot dir", map[string]string{"host_files": `["a=/etc/.app/a"]`}, nil},
		{"dot file", map[string]string{"host_files": `["a=/etc/app/.a"]`}, nil},
		{"duplicate entry", map[string]string{"host_file_units": `["a:app.service", "a:app.service"]`}, nil},
		{"nine files", map[string]string{"host_files": `["a=/etc/app/a", "b=/etc/app/b", "c=/etc/app/c", "d=/etc/app/d", "e=/etc/app/e", "f=/etc/app/f", "g=/etc/app/g", "h=/etc/app/h", "i=/etc/app/i"]`}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := c.fs
			if fsys == nil {
				fsys = ddFS()
			}
			p := loadHostPolicy(t, render(c.over), fsys)
			if p.Valid || p.Reason != "parse" || p.HostFiles != nil {
				t.Fatalf("got valid=%t reason=%q", p.Valid, p.Reason)
			}
		})
	}
}

// Spec rev 1a A2: an unsafe or missing executable leaves the policy valid
// and marks only that file ExecSafe false.
func TestHostFilesUnsafeExecKeepsPolicyValid(t *testing.T) {
	groupWritable := ddFS()
	groupWritable["/opt/dd-worker/rc6/dd-worker"] = fakeEntry{mode: 0o775}
	linkDirWritable := ddFS()
	linkDirWritable["/usr/local/bin"] = fakeEntry{mode: os.ModeDir | 0o777}
	targetDirNotRoot := ddFS()
	targetDirNotRoot["/opt/dd-worker/rc6"] = fakeEntry{mode: os.ModeDir | 0o755, uid: 1500}
	linkNotRoot := ddFS()
	linkNotRoot["/usr/local/bin/dd-worker"] = fakeEntry{mode: os.ModeSymlink | 0o777, uid: 1500, link: "/opt/dd-worker/rc6/dd-worker"}
	notExec := ddFS()
	notExec["/opt/dd-worker/rc6/dd-worker"] = fakeEntry{mode: 0o644}
	loop := ddFS()
	loop["/usr/local/bin/dd-worker"] = fakeEntry{mode: os.ModeSymlink | 0o777, link: "/usr/local/bin/dd-worker"}
	missing := ddFS()
	delete(missing, "/opt/dd-worker/rc6/dd-worker")
	body := ddPolicy + "projects = [\"acme\"]\nimages = [\"ghcr.io/acme/*\"]\nedge = true\n"
	body = strings.Replace(body, "enabled = false", "enabled = true", 1)
	for name, fsys := range map[string]fakeFS{
		"group-writable executable": groupWritable, "world-writable link dir": linkDirWritable,
		"target dir not root": targetDirNotRoot, "link not root": linkNotRoot,
		"not executable": notExec, "link loop": loop, "missing executable": missing,
	} {
		t.Run(name, func(t *testing.T) {
			p := loadHostPolicy(t, body, fsys)
			if !p.Valid || !p.ImagesEnabled() || !p.EdgeEnabled() || len(p.HostFiles) != 2 {
				t.Fatalf("valid=%t reason=%q images=%t edge=%t", p.Valid, p.Reason, p.ImagesEnabled(), p.EdgeEnabled())
			}
			for _, f := range p.HostFiles {
				if f.ExecSafe {
					t.Fatalf("%s marked safe", f.Name)
				}
			}
		})
	}
	p := loadHostPolicy(t, body, ddFS())
	if !p.Valid || !p.HostFiles[0].ExecSafe || !p.HostFiles[1].ExecSafe {
		t.Fatal("safe layout marked unsafe")
	}
	// Only the file whose command is unsafe is marked.
	mixed := strings.Replace(ddPolicy, "dd-secrets:/usr/local/bin/dd-worker secrets check --file -", "dd-secrets:/usr/bin/nosuch", 1)
	p = loadHostPolicy(t, mixed, ddFS())
	if !p.Valid || !p.HostFiles[0].ExecSafe || p.HostFiles[1].ExecSafe {
		t.Fatalf("mixed %+v", p.HostFiles)
	}
}

func TestHostFilesAcceptsLimits(t *testing.T) {
	name := "a" + strings.Repeat("b", 31)
	var files, modes, units, runAs, checks []string
	for i := 0; i < 8; i++ {
		n := name[:31] + string(rune('a'+i))
		files = append(files, `"`+n+`=/etc/app/f`+string(rune('a'+i))+`"`)
		modes = append(modes, `"`+n+`:0640"`)
		units = append(units, `"`+n+`:app.service"`)
		runAs = append(runAs, `"`+n+`:dd-worker"`)
		checks = append(checks, `"`+n+`:/usr/bin/true"`)
	}
	j := func(xs []string) string { return "[" + strings.Join(xs, ", ") + "]" }
	body := "host_files = " + j(files) + "\nhost_file_modes = " + j(modes) + "\nhost_file_units = " + j(units) +
		"\nhost_file_run_as = " + j(runAs) + "\nhost_file_checks = " + j(checks) +
		"\nhost_file_secret = [\"" + name[:31] + "a\"]\nhost_file_health = [\"" + name[:31] + "a:/usr/bin/true --wait 165s\"]\n"
	p := loadHostPolicy(t, body, ddFS())
	if !p.Valid || len(p.HostFiles) != 8 || len(p.HostFiles[0].Name) != 32 || !p.HostFiles[0].Secret {
		t.Fatalf("valid=%t reason=%q n=%d", p.Valid, p.Reason, len(p.HostFiles))
	}
}

func TestSafeExecAbsoluteLink(t *testing.T) {
	f := ddFS()
	f["/usr/local/bin/dd-worker"] = fakeEntry{mode: os.ModeSymlink | 0o777, link: "/opt/dd-worker/rc6/dd-worker"}
	if !SafeExec(f, "/usr/local/bin/dd-worker") {
		t.Fatal("absolute link refused")
	}
	if SafeExec(f, "/usr/local/bin/../bin/dd-worker") {
		t.Fatal("unclean path accepted")
	}
}
