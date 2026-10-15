package programs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

func TestDirContainment(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	app := filepath.Join(ws, "app")
	outside := filepath.Join(root, "outside")
	sibling := filepath.Join(root, "workspace-evil")
	for _, d := range []string{app, outside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(app, "escape")); err != nil {
		t.Fatal(err)
	}
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	okDir := func(dir, want string) {
		t.Helper()
		_, code := Check("program_start", spec(t, startSpec{Dir: dir, Exec: "/usr/bin/pnpm"}), Gate{Policy: pol, Files: true})
		if code != want {
			t.Fatalf("dir %s code %s want %s", dir, code, want)
		}
	}
	okDir(app, "")
	okDir(app+string(os.PathSeparator), "")
	okDir(outside, ReasonPolicy)
	okDir(sibling, ReasonPolicy)
	okDir(filepath.Join(app, "escape"), ReasonPolicy)
	// ".." that resolves back inside must still be bad_spec. Realpath alone would allow it.
	// Join would Clean ".." away. The request string must still contain the segment.
	okDir(app+"/../app", ReasonBadSpec)
}

func TestExecResolution(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	binDir := filepath.Join(app, "node_modules", ".bin")
	local := filepath.Join(app, "node_modules", "pnpm", "bin", "pnpm")
	outside := filepath.Join(root, "outside", "pnpm")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(local, filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "workspace")
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm", "/usr/bin/node"}, 20)
	files := Gate{Policy: pol, Files: true}
	got, code := Check("program_start", spec(t, startSpec{Dir: app, Exec: "pnpm"}), files)
	if code != "" {
		t.Fatal(code)
	}
	wantLocal, err := filepath.EvalSymlinks(local)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec != wantLocal {
		t.Fatalf("bare name preferred %s over local %s", got.Exec, wantLocal)
	}
	absBin := filepath.Join(binDir, "pnpm")
	got, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: absBin}), files)
	if code != "" || got.Exec != wantLocal {
		t.Fatalf("absolute bin %s %s", got.Exec, code)
	}

	// Remove the inside bin and the same bare name must fall through to exec_allow.
	if err := os.Remove(filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	got, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "pnpm"}), files)
	if code != "" || got.Exec != "/usr/bin/pnpm" {
		t.Fatalf("fallback %s %s", got.Exec, code)
	}

	// A bin symlink that leaves dir is not used, even when exec_allow has no match.
	if err := os.Symlink(outside, filepath.Join(binDir, "evil")); err != nil {
		t.Fatal(err)
	}
	_, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "evil"}), files)
	if code != ReasonPolicy {
		t.Fatalf("escaping bin %s", code)
	}
	absEvil := filepath.Join(binDir, "evil")
	_, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: absEvil}), files)
	if code != ReasonPolicy {
		t.Fatalf("absolute escaping bin %s", code)
	}
	// The same escaping link must not win when exec_allow can satisfy the basename.
	if err := os.Remove(filepath.Join(binDir, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	got, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "pnpm"}), files)
	if code != "" || got.Exec != "/usr/bin/pnpm" {
		t.Fatalf("escape must fall through, got %s %s", got.Exec, code)
	}
	_, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "/tmp/not-allowed"}), files)
	if code != ReasonPolicy {
		t.Fatalf("exec not allowed %s", code)
	}
}

func TestFieldAndPolicyRules(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "workspace")
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm", "/bin/sh"}, 20)
	base := startSpec{Dir: app, Exec: "/usr/bin/pnpm"}
	cases := []struct {
		name string
		edit func(*startSpec)
		want string
	}{
		{"ok", func(*startSpec) {}, ""},
		{"run_as root", func(s *startSpec) { s.RunAs = "root" }, ReasonBadSpec},
		{"run_as not listed", func(s *startSpec) { s.RunAs = "mallory" }, ReasonPolicy},
		{"low uid not listed", func(s *startSpec) { s.RunAs = "daemon" }, ReasonPolicy},
		{"exec metacharacters", func(s *startSpec) { s.Exec = "pnpm;rm" }, ReasonBadSpec},
		{"exec space", func(s *startSpec) { s.Exec = "/usr/bin/pn pm" }, ReasonBadSpec},
		{"shell without allow", func(s *startSpec) {
			s.Shell = true
			s.Exec = "/bin/sh"
			s.Args = []string{"-c", "pnpm run dev"}
		}, ReasonPolicy},
		{"bad unit", func(s *startSpec) { s.Entry = "Bad_Name" }, ReasonBadSpec},
		{"long entry", func(s *startSpec) { s.Entry = strings.Repeat("a", 49) }, ReasonBadSpec},
		{"arg nul", func(s *startSpec) { s.Args = []string{"a\x00b"} }, ReasonBadSpec},
		{"arg newline", func(s *startSpec) { s.Args = []string{"a\nb"} }, ReasonBadSpec},
		{"env secret", func(s *startSpec) { s.Env = map[string]string{"PASSWORD": "x"} }, ReasonBadSpec},
		{"env path", func(s *startSpec) { s.Env = map[string]string{"PATH": "/bin"} }, ReasonBadSpec},
		{"env home", func(s *startSpec) { s.Env = map[string]string{"HOME": "/tmp"} }, ReasonBadSpec},
		{"env lower", func(s *startSpec) { s.Env = map[string]string{"port": "1"} }, ReasonBadSpec},
		{"env newline", func(s *startSpec) { s.Env = map[string]string{"PORT": "1\n2"} }, ReasonBadSpec},
		{"bash name is not an agent check", func(s *startSpec) { s.Exec = "/bin/bash" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Args = append([]string(nil), base.Args...)
			s.Env = map[string]string{}
			tc.edit(&s)
			_, code := Check("program_start", spec(t, s), Gate{Policy: pol})
			if code != tc.want {
				t.Fatalf("got %s want %s", code, tc.want)
			}
		})
	}
}

func TestShellModeUsesSh(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	pol := joePolicy(filepath.Join(root, "workspace"), true, []string{"/bin/sh"}, 20)
	got, code := Check("program_start", spec(t, startSpec{
		Dir: app, Exec: "/bin/sh", Args: []string{"-c", "pnpm run dev"}, Shell: true,
	}), Gate{Policy: pol, Files: true})
	if code != "" || got.Exec != "/bin/sh" || !got.Shell {
		t.Fatalf("%+v %s", got, code)
	}
	noSh := pol
	noSh.ExecAllow = []string{"/usr/bin/pnpm"}
	_, code = Check("program_start", spec(t, startSpec{
		Dir: app, Exec: "/bin/sh", Args: []string{"-c", "true"}, Shell: true,
	}), Gate{Policy: noSh, Files: true})
	if code != ReasonPolicy {
		t.Fatalf("sh not allowed %s", code)
	}
}

func TestMaxPrograms(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	pol := joePolicy(filepath.Join(root, "workspace"), false, []string{"/usr/bin/pnpm"}, 1)
	raw := spec(t, startSpec{Dir: app, Exec: "/usr/bin/pnpm"})
	unit, ok := UnitName("tour-agent", "tra-backend")
	if !ok {
		t.Fatal("unit")
	}
	_, code := Check("program_start", raw, Gate{Policy: pol, CountKnown: true, Running: 1})
	if code != ReasonPolicy {
		t.Fatalf("at cap %s", code)
	}
	_, code = Check("program_start", raw, Gate{
		Policy: pol, CountKnown: true, Running: 1, Active: map[string]bool{unit: true},
	})
	if code != "" {
		t.Fatalf("already running %s", code)
	}
	_, code = Check("program_start", raw, Gate{Policy: pol, CountKnown: false, Running: 100})
	if code != "" {
		t.Fatalf("unknown count must not deny %s", code)
	}
}

func TestDisabledPolicyBeforeFieldRules(t *testing.T) {
	raw := spec(t, startSpec{Dir: "/tmp/app", Exec: "pnpm;rm"})
	_, code := Check("program_start", raw, Gate{Policy: policy.Policy{}})
	if code != ReasonPolicy {
		t.Fatalf("missing policy %s", code)
	}
	_, code = Check("program_start", []byte(`{"extra":true}`), Gate{Policy: policy.Policy{Valid: true, Enabled: true}})
	if code != ReasonBadSpec {
		t.Fatalf("closed keys %s", code)
	}
}

func TestTooManyEnvKeys(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for i := 0; i < 65; i++ {
		env["K"+string(rune('A'+i/26))+string(rune('A'+i%26))] = "v"
	}
	// 65 keys of the form KAA, KAB, ... which match the env key rule.
	env = map[string]string{}
	for i := 0; i < 65; i++ {
		env[keyName(i)] = "v"
	}
	pol := joePolicy(filepath.Join(root, "workspace"), false, []string{"/usr/bin/pnpm"}, 20)
	_, code := Check("program_start", spec(t, startSpec{Dir: app, Exec: "/usr/bin/pnpm", Env: env}), Gate{Policy: pol})
	if code != ReasonBadSpec {
		t.Fatalf("65 keys %s", code)
	}
}

func keyName(i int) string {
	return "K" + string(rune('A'+i/26)) + string(rune('A'+i%26))
}

type startSpec struct {
	Project string
	Entry   string
	Dir     string
	Exec    string
	Args    []string
	Shell   bool
	RunAs   string
	Env     map[string]string
	Restart string
}

func spec(t *testing.T, s startSpec) []byte {
	t.Helper()
	if s.Project == "" {
		s.Project = "tour-agent"
	}
	if s.Entry == "" {
		s.Entry = "tra-backend"
	}
	if s.RunAs == "" {
		s.RunAs = "joe"
	}
	if s.Restart == "" {
		s.Restart = "on-failure"
	}
	if s.Args == nil {
		s.Args = []string{}
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	b, err := json.Marshal(map[string]any{
		"project": s.Project, "entry": s.Entry, "dir": s.Dir, "exec": s.Exec,
		"args": s.Args, "shell": s.Shell, "run_as": s.RunAs, "env": s.Env, "restart": s.Restart,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func joePolicy(ws string, shell bool, allow []string, max int) policy.Policy {
	return policy.Policy{
		Exists: true, Valid: true, Enabled: true, AllowShell: shell, MaxPrograms: max,
		RunAs: []string{"joe"}, Dirs: []string{ws}, ExecAllow: allow,
		Accounts: map[string]policy.Account{
			"joe": {Name: "joe", UID: 1000, GID: 1000, Home: "/home/joe", Group: "joe"},
		},
	}
}
