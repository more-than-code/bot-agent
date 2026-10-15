package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// TestProgramDecideTable is the agent entry for checks 5–7.
// It calls Decide, so a program task that no longer reaches programs.Check
// stops failing these cases.
func TestProgramDecideTable(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	app := filepath.Join(ws, "app")
	outside := filepath.Join(root, "outside")
	sibling := filepath.Join(root, "workspace-evil")
	binDir := filepath.Join(app, "node_modules", ".bin")
	local := filepath.Join(app, "node_modules", "pnpm", "bin", "pnpm")
	escaped := filepath.Join(root, "outside", "pnpm")
	for _, d := range []string{app, outside, sibling, filepath.Dir(local), binDir, filepath.Dir(escaped)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(local, []byte("local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escaped, []byte("out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(app, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(local, filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, filepath.Join(binDir, "evil")); err != nil {
		t.Fatal(err)
	}
	pol := loadPolicy(t, ws, false, 1, []string{"/usr/bin/pnpm", "/usr/bin/node", "/bin/sh"})
	if len(pol.RunAs) != 1 || pol.RunAs[0] != "joe" {
		t.Fatalf("run_as filter %+v dropped %+v", pol.RunAs, pol.Dropped)
	}
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	unit, ok := programs.UnitName("tour-agent", "tra-backend")
	if !ok {
		t.Fatal("unit")
	}
	base := progSpec{Dir: app, Exec: "/usr/bin/pnpm"}
	gate := Gate{Policy: pol, Known: true}
	check := func(name string, tk protocol.Task, g Gate, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			code, _ := Decide(tk, host, now, nil, g)
			if code != want {
				t.Fatalf("got %s want %s", code, want)
			}
		})
	}
	okSpec := specOf(t, base)
	check("ok", progTask(host, "program_start", nil, live, okSpec), gate, "")
	check("wrong host", progTask("other", "program_start", nil, live, okSpec), gate, "wrong_host")
	check("expired", progTask(host, "program_start", nil, protocol.FormatTime(now), okSpec), gate, "expired")
	check("kind logs", progTask(host, "logs", nil, live, []byte(`{}`)), gate, "forbidden")
	check("service set", progTask(host, "program_start", protocol.Str("web"), live, okSpec), gate, "bad_spec")
	check("disabled beats bad exec", progTask(host, "program_start", nil, live, specOf(t, progSpec{Dir: app, Exec: "pnpm;rm"})), Gate{}, "policy_denied")
	check("extra key", progTask(host, "program_start", nil, live, []byte(`{"extra":true}`)), gate, "bad_spec")
	check("run_as root", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.RunAs = "root" }))), gate, "bad_spec")
	check("uid under 1000", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.RunAs = "daemon" }))), gate, "policy_denied")
	check("run_as not listed", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.RunAs = "mallory" }))), gate, "policy_denied")
	check("dir outside is not an agent check", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Dir = outside }))), gate, "")
	check("dir symlink is not an agent check", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Dir = filepath.Join(app, "escape") }))), gate, "")
	check("dir dotdot", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Dir = app + "/../app" }))), gate, "bad_spec")
	check("dir prefix sibling is not an agent check", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Dir = sibling }))), gate, "")
	check("exec not allowed is not an agent check", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "/tmp/not-allowed" }))), gate, "")
	check("bare name in dir", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "pnpm" }))), gate, "")
	narrow := pol
	narrow.ExecAllow = []string{"/usr/bin/node"}
	check("bare name only in dir", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "pnpm" }))), Gate{Policy: narrow, Known: true}, "")
	// The in-dir pnpm link is inside dir. Replace it with an escaping link:
	// the bare name must still be allowed because exec_allow has pnpm.
	if err := os.Remove(filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escaped, filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	check("escaping bin falls through", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "pnpm" }))), gate, "")
	check("escaping bin is not an agent check", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "evil" }))), gate, "")
	check("shell without allow", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) {
		s.Shell = true
		s.Exec = "/bin/sh"
		s.Args = []string{"-c", "pnpm run dev"}
	}))), gate, "policy_denied")
	check("exec metacharacters", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Exec = "pnpm;rm" }))), gate, "bad_spec")
	check("bad unit", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Entry = "Bad_Name" }))), gate, "bad_spec")
	check("arg nul", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Args = []string{"a\x00b"} }))), gate, "bad_spec")
	check("arg newline", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Args = []string{"a\nb"} }))), gate, "bad_spec")
	check("env secret", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{"PASSWORD": "x"} }))), gate, "bad_spec")
	check("env path", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{"PATH": "/bin"} }))), gate, "bad_spec")
	check("env home", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{"HOME": "/tmp"} }))), gate, "bad_spec")
	check("env lower", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{"port": "1"} }))), gate, "bad_spec")
	check("env newline", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{"PORT": "1\n2"} }))), gate, "bad_spec")
	for _, key := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "NODE_PATH", "BASH_ENV", "ENV", "IFS", "PYTHONPATH", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB"} {
		k := key
		check("env "+k, progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Env = map[string]string{k: "x"} }))), gate, "bad_spec")
	}
	check("hoisted NODE_OPTIONS", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) { s.Args = []string{"NODE_OPTIONS=--inspect"} }))), gate, "bad_spec")
	check("at cap", progTask(host, "program_start", nil, live, okSpec), Gate{Policy: pol, Known: true, Running: 1}, "policy_denied")
	check("already active", progTask(host, "program_start", nil, live, okSpec), Gate{Policy: pol, Known: true, Running: 1, Active: map[string]bool{unit: true}}, "")
	check("count unknown skips max", progTask(host, "program_start", nil, live, okSpec), Gate{Policy: pol, Running: 100}, "")
	shell := pol
	shell.AllowShell = true
	check("shell allowed", progTask(host, "program_start", nil, live, specOf(t, edit(base, func(s *progSpec) {
		s.Shell = true
		s.Exec = "/bin/sh"
		s.Args = []string{"-c", "pnpm run dev"}
	}))), Gate{Policy: shell, Known: true}, "")
}

func loadPolicy(t *testing.T, ws string, shell bool, max int, allow []string) policy.Policy {
	t.Helper()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\ndaemon:x:1:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(allow))
	for i, a := range allow {
		quoted[i] = `"` + a + `"`
	}
	flag := "false"
	if shell {
		flag = "true"
	}
	body := fmt.Sprintf("enabled = true\nallow_shell = %s\nmax_programs = %d\nrun_as = [\"joe\", \"root\", \"daemon\", \"mallory\"]\ndirs = [%q]\nexec_allow = [%s]\n", flag, max, ws, strings.Join(quoted, ", "))
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Load(policy.Files{
		Policy: path, Passwd: passwd, Group: group,
		UID: func(os.FileInfo) (uint32, bool) { return 0, true },
	})
	if !pol.Valid || !pol.Enabled {
		t.Fatalf("policy %s", pol.Reason)
	}
	return pol
}

type progSpec struct {
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

func edit(base progSpec, fn func(*progSpec)) progSpec {
	if base.Args != nil {
		base.Args = append([]string(nil), base.Args...)
	}
	fn(&base)
	return base
}

func specOf(t *testing.T, s progSpec) []byte {
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

func progTask(host, kind string, service *string, exp string, spec []byte) protocol.Task {
	return protocol.Task{
		V: protocol.Version, Type: "task", TaskID: "task-1", HostID: host, Kind: kind,
		Service: service, ExpiresAt: exp, TimeoutSec: 30, Spec: spec,
	}
}
