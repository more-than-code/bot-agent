package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestHelperRefusesForeignUnit(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	path := unitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const foreign = "[Service]\nExecStart=/bin/sh\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	body := h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
	for _, verb := range []string{"start", "stop", "restart", "status", "logs", "sockets"} {
		in := body
		if verb == "logs" {
			in = h.logs(t, "demo", "web", 10)
		}
		if verb != "start" && verb != "logs" {
			in = h.ref(t, "demo", "web")
		}
		raw := Handle(context.Background(), verb, in, h.sys())
		reason := refusalReason(t, raw)
		if reason != programs.ReasonNotOwned {
			t.Fatalf("%s reason %s body %s", verb, reason, raw)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != foreign {
			t.Fatalf("%s touched the foreign unit: %s", verb, got)
		}
	}
	if h.ctl.called("stop") || h.ctl.called("restart") {
		t.Fatalf("systemd was called: %v", h.ctl.calls)
	}
}

func TestUnitFileGoldenAndModes(t *testing.T) {
	h := newHarness(t, true, false, 20)
	args := []string{`100%`, `$HOME`, `say "hi"`, `a\b`, `hello world`}
	env := map[string]string{"PORT": `100% $HOME`}
	raw := Handle(context.Background(), "start", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, args, env), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	unit := unitFile(h.root, "deploybot-demo-web.service")
	envPath := envFile(h.root, "deploybot-demo-web.service")
	got, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(h.app)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(real, "\\$%\"") {
		t.Fatalf("fixture path is not literal-quotable: %s", real)
	}
	wantExec := `ExecStart="/usr/bin/pnpm" "100%%" "$$HOME" "say \"hi\"" "a\\b" "hello world"`
	want := "[Unit]\n" +
		"X-Deploybot-Project=demo\n" +
		"X-Deploybot-Entry=web\n" +
		"\n" +
		"[Service]\n" +
		"User=joe\n" +
		"Group=joe\n" +
		"WorkingDirectory=" + real + "\n" +
		wantExec + "\n" +
		"Environment=\"HOME=/home/joe\"\n" +
		`Environment="PATH=` + real + `/node_modules/.bin:/usr/local/bin:/usr/bin:/bin"` + "\n" +
		"EnvironmentFile=/run/bot-agent/env/deploybot-demo-web.service.env\n" +
		"Restart=on-failure\n" +
		"NoNewPrivileges=yes\n" +
		"PrivateTmp=yes\n" +
		"ProtectSystem=full\n" +
		"ProtectHome=read-only\n" +
		`ReadWritePaths="` + real + "\"\n" +
		"PrivateDevices=yes\n" +
		"ProtectKernelTunables=yes\n" +
		"ProtectKernelModules=yes\n" +
		"ProtectControlGroups=yes\n" +
		"InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/containerd/containerd.sock -/run/podman/podman.sock\n" +
		"RestrictSUIDSGID=yes\n" +
		"LockPersonality=yes\n"
	if string(got) != want {
		t.Fatalf("unit\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(string(got), `100% $HOME`) {
		t.Fatal("env value leaked into the unit")
	}
	envBody, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(envBody) != "PORT=\"100% $HOME\"\n" {
		t.Fatalf("env file %q", envBody)
	}
	assertMode(t, unit, 0o644)
	assertMode(t, envPath, 0o600)
	if !h.ctl.called("daemon-reload") || !h.ctl.called("restart") {
		t.Fatalf("calls %v", h.ctl.calls)
	}
	if !programs.StatusFresh(raw) {
		t.Fatal("start did not record a new activation")
	}
}

func TestStopRemovesFilesAndMissingSucceeds(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	st := mustStatus(t, raw)
	if st.ActiveState != "inactive" {
		t.Fatalf("%+v", st)
	}
	if h.ctl.called("stop") {
		t.Fatal("stop of a missing unit called systemctl")
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	unit := unitFile(h.root, "deploybot-demo-web.service")
	envPath := envFile(h.root, "deploybot-demo-web.service")
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	st = mustStatus(t, raw)
	if st.ActiveState != "inactive" {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit file remains")
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("env file remains")
	}
	if _, err := os.Stat(specFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("spec file remains")
	}
}

func TestStopFailureLeavesFiles(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	h.ctl.stopErr = true
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatal(string(raw))
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); err != nil {
		t.Fatal("stop failure removed the unit")
	}
}

// Each connection is its own helper process; concurrent starts must not exceed max_programs.
func TestConcurrentStartsRespectMaxPrograms(t *testing.T) {
	h := newHarness(t, true, false, 1)
	h.ctl.reloadWait = 20 * time.Millisecond
	var wg sync.WaitGroup
	results := make([][]byte, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := h.start(t, "demo", fmt.Sprintf("app%d", i), h.app, "/usr/bin/pnpm", false, nil)
			results[i] = Handle(context.Background(), "start", in, h.sys())
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, raw := range results {
		if refusalReason(t, raw) == "" {
			ok++
		}
	}
	units, _ := filepath.Glob(filepath.Join(filepath.Dir(unitFile(h.root, "deploybot-demo-app0.service")), "deploybot-demo-app*.service"))
	if ok != 1 || len(units) != 1 {
		t.Fatalf("started %d, unit files %d; want 1 and 1", ok, len(units))
	}
}

func TestRestartDoesNotRewrite(t *testing.T) {
	h := newHarness(t, true, false, 1)
	unit := "deploybot-demo-api.service"
	path := unitFile(h.root, unit)
	body := "[Unit]\nX-Deploybot-Project=demo\nX-Deploybot-Entry=api\nSENTINEL\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	plantSpec(t, h, "demo", "api")
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "api"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("rewritten: %s", got)
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("max_programs %s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("second unit was written")
	}
}

func TestLogsCapAndRedact(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	var b strings.Builder
	b.WriteString("OLDEST\n")
	for i := 0; i < 80; i++ {
		b.WriteString(strings.Repeat("x", 1000))
		b.WriteByte('\n')
	}
	b.WriteString(strings.Repeat("y", 3000))
	b.WriteByte('\n')
	b.WriteString("password=hunter2 NEWEST\n")
	h.ctl.journal = b.String()
	raw = Handle(context.Background(), "logs", h.logs(t, "demo", "web", 100), h.sys())
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	if !batch.Truncated {
		t.Fatal("expected truncated")
	}
	joined := strings.Join(batch.Lines, "\n")
	if strings.Contains(joined, "hunter2") || strings.Contains(joined, "OLDEST") {
		t.Fatalf("cap or redaction failed: %s", joined[:min(200, len(joined))])
	}
	if !strings.Contains(joined, "NEWEST") || !strings.Contains(joined, "[REDACTED]") {
		t.Fatal("newest line was dropped or not redacted")
	}
	total := 0
	for _, line := range batch.Lines {
		if len([]rune(line)) > 2048 {
			t.Fatalf("line %d", len([]rune(line)))
		}
		total += len(line)
	}
	if total > 64*1024 {
		t.Fatalf("bytes %d", total)
	}
	if !h.ctl.journalArgsOK("deploybot-demo-web.service", "100") {
		t.Fatalf("journalctl %v", h.ctl.calls)
	}
}

func TestSocketsMapsCgroupOnly(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	path := unitFile(h.root, unit)
	body := "[Unit]\nX-Deploybot-Project=demo\nX-Deploybot-Entry=web\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(h.root, "proc")
	sysfs := filepath.Join(h.root, "sys")
	writeCgroup(t, sysfs, unit, "42\n")
	linkSock(t, proc, 42, "3", 99)
	linkSock(t, proc, 7, "3", 77)
	tcp := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0BD7 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 99 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 77 1 0000000000000000 100 0 0 10 0\n"
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := h.sys()
	sys.Proc = proc
	sys.Sysfs = sysfs
	raw := Handle(context.Background(), "sockets", h.ref(t, "demo", "web"), sys)
	ports, user, reason, err := programs.DecodeSockets(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	if len(ports) != 1 || ports[0] != 3031 {
		t.Fatalf("ports %v", ports)
	}
	if user == nil || *user != "joe" {
		t.Fatalf("user %v", user)
	}
	os.Remove(filepath.Join(sysfs, "fs", "cgroup", "system.slice", unit, "cgroup.procs"))
	raw = Handle(context.Background(), "sockets", h.ref(t, "demo", "web"), sys)
	ports, user, reason, err = programs.DecodeSockets(raw)
	if err != nil || reason != "" || len(ports) != 0 || user != nil {
		t.Fatalf("missing cgroup %s %v %v %v", raw, ports, user, err)
	}
}

func TestHelperValidationTable(t *testing.T) {
	h := newHarness(t, true, false, 20)
	outside := filepath.Join(h.base, "outside")
	sibling := filepath.Join(h.base, "workspace-evil", "app")
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"run_as root", h.startAs(t, h.app, "/usr/bin/pnpm", "root", false), programs.ReasonBadSpec},
		{"uid under 1000", h.startAs(t, h.app, "/usr/bin/pnpm", "daemon", false), programs.ReasonPolicy},
		{"run_as not listed", h.startAs(t, h.app, "/usr/bin/pnpm", "mallory", false), programs.ReasonPolicy},
		{"dir outside", h.start(t, "demo", "web", outside, "/usr/bin/pnpm", false, nil), programs.ReasonPolicy},
		{"dir symlink", h.start(t, "demo", "web", filepath.Join(h.app, "escape"), "/usr/bin/pnpm", false, nil), programs.ReasonPolicy},
		{"dir dotdot", h.start(t, "demo", "web", h.app+"/../app", "/usr/bin/pnpm", false, nil), programs.ReasonBadSpec},
		{"prefix sibling", h.start(t, "demo", "web", sibling, "/usr/bin/pnpm", false, nil), programs.ReasonPolicy},
		{"exec not allowed", h.start(t, "demo", "web", h.app, "/tmp/not-allowed", false, nil), programs.ReasonPolicy},
		{"metacharacters", h.start(t, "demo", "web", h.app, "pnpm;rm", false, nil), programs.ReasonBadSpec},
		{"shell without allow", h.shell(t), programs.ReasonPolicy},
		{"bad entry", h.start(t, "demo", "Bad", h.app, "/usr/bin/pnpm", false, nil), programs.ReasonBadSpec},
		{"nul arg", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, []string{"a\x00b"}), programs.ReasonBadSpec},
		{"newline arg", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, []string{"a\nb"}), programs.ReasonBadSpec},
		{"env password", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{"PASSWORD": "super-secret"}), programs.ReasonBadSpec},
		{"env home", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{"HOME": "x"}), programs.ReasonBadSpec},
		{"env lowercase", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{"port": "1"}), programs.ReasonBadSpec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			sys := h.sys()
			sys.Logf = func(f string, args ...any) {
				logged = append(logged, fmt.Sprintf(f, args...))
			}
			raw := Handle(context.Background(), "start", tc.body, sys)
			if refusalReason(t, raw) != tc.want {
				t.Fatalf("got %s want %s", raw, tc.want)
			}
			if strings.Contains(string(raw), "super-secret") || strings.Contains(strings.Join(logged, "\n"), "super-secret") {
				t.Fatal("secret leaked")
			}
			if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
				t.Fatal("denied start wrote a unit")
			}
		})
	}
}

func TestBareNameResolutionOrder(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	local, err := filepath.EvalSymlinks(h.local)
	if err != nil {
		t.Fatal(err)
	}
	got := readUnit(t, h, "deploybot-demo-web.service")
	if !strings.Contains(got, `"`+local+`"`) {
		t.Fatalf("bare name did not use the in-dir bin:\n%s", got)
	}
	if err := os.Remove(filepath.Join(h.app, "node_modules", ".bin", "pnpm")); err != nil {
		t.Fatal(err)
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "api", h.app, "pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got = readUnit(t, h, "deploybot-demo-api.service")
	if !strings.Contains(got, `"/usr/bin/pnpm"`) {
		t.Fatalf("fallback:\n%s", got)
	}
	outside := filepath.Join(h.base, "outside", "pnpm")
	if err := os.Symlink(outside, filepath.Join(h.app, "node_modules", ".bin", "evil")); err != nil {
		t.Fatal(err)
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "evil", h.app, "evil", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("escaping bin %s", raw)
	}
	absEvil := filepath.Join(h.app, "node_modules", ".bin", "evil")
	raw = Handle(context.Background(), "start", h.start(t, "demo", "abs", h.app, absEvil, false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("absolute escaping bin %s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-abs.service")); !os.IsNotExist(err) {
		t.Fatal("absolute escape wrote a unit")
	}
	if err := os.Symlink(outside, filepath.Join(h.app, "node_modules", ".bin", "pnpm")); err != nil {
		t.Fatal(err)
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "fall", h.app, "pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got = readUnit(t, h, "deploybot-demo-fall.service")
	if strings.Contains(got, outside) || !strings.Contains(got, `"/usr/bin/pnpm"`) {
		t.Fatalf("escape won:\n%s", got)
	}
}

func TestShellModeNeedsShInAllow(t *testing.T) {
	h := newHarness(t, true, true, 20)
	raw := Handle(context.Background(), "start", h.shell(t), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got := readUnit(t, h, "deploybot-demo-web.service")
	if !strings.Contains(got, `ExecStart="/bin/sh" "-c" "echo ok"`) {
		t.Fatalf("%s", got)
	}
}

func TestPolicyGatesAtHelper(t *testing.T) {
	h := newHarness(t, true, false, 20)
	body := h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)

	missing := h.sys()
	missing.Policy = filepath.Join(h.base, "no-such.toml")
	if refusalReason(t, Handle(context.Background(), "start", body, missing)) != programs.ReasonPolicy {
		t.Fatal("missing file")
	}

	unowned := h.sys()
	unowned.UID = nil
	if refusalReason(t, Handle(context.Background(), "start", body, unowned)) != programs.ReasonPolicy {
		t.Fatal("non-root owner")
	}

	if err := os.Chmod(h.policy, 0o664); err != nil {
		t.Fatal(err)
	}
	if refusalReason(t, Handle(context.Background(), "start", body, h.sys())) != programs.ReasonPolicy {
		t.Fatal("group writable")
	}
	if err := os.Chmod(h.policy, 0o642); err != nil {
		t.Fatal(err)
	}
	if refusalReason(t, Handle(context.Background(), "start", body, h.sys())) != programs.ReasonPolicy {
		t.Fatal("world writable")
	}
	if err := os.Chmod(h.policy, 0o644); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(h.base, "unknown.toml")
	if err := os.WriteFile(unknown, []byte("enabled = true\nimages = []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := h.sys()
	sys.Policy = unknown
	if refusalReason(t, Handle(context.Background(), "start", body, sys)) != programs.ReasonPolicy {
		t.Fatal("unknown key")
	}
}

type harness struct {
	t      *testing.T
	base   string
	root   string
	app    string
	local  string
	policy string
	passwd string
	group  string
	ctl    *fakeCtl
}

func newHarness(t *testing.T, enabled, shell bool, max int) *harness {
	t.Helper()
	base := t.TempDir()
	app := filepath.Join(base, "workspace", "app")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "workspace-evil", "app")
	for _, d := range []string{app, outside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(app, "escape")); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(app, "node_modules", "pnpm", "bin", "pnpm")
	binDir := filepath.Join(app, "node_modules", ".bin")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "pnpm"), []byte("out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(local, filepath.Join(binDir, "pnpm")); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(base, "passwd")
	group := filepath.Join(base, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\ndaemon:x:1:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allow := []string{"/usr/bin/pnpm"}
	if shell {
		allow = append(allow, "/bin/sh")
	}
	policyPath := writePolicy(t, base, enabled, shell, max, []string{"joe", "root", "daemon"}, []string{filepath.Join(base, "workspace")}, allow)
	return &harness{
		t: t, base: base, root: filepath.Join(base, "root"), app: app, local: local,
		policy: policyPath, passwd: passwd, group: group, ctl: newFake(),
	}
}

func (h *harness) sys() Sys {
	return Sys{
		Systemctl:  h.ctl.systemctl,
		Journalctl: h.ctl.journalctl,
		Root:       h.root,
		Passwd:     h.passwd,
		Group:      h.group,
		Policy:     h.policy,
		UID:        func(os.FileInfo) (uint32, bool) { return 0, true },
	}
}

func (h *harness) start(t *testing.T, project, entry, dir, exec string, shell bool, args []string) []byte {
	t.Helper()
	return h.startFull(t, project, entry, dir, exec, "joe", shell, args, map[string]string{})
}

func (h *harness) startAs(t *testing.T, dir, exec, runAs string, shell bool) []byte {
	t.Helper()
	return h.startFull(t, "demo", "web", dir, exec, runAs, shell, nil, map[string]string{})
}

func (h *harness) startEnv(t *testing.T, project, entry, dir, exec string, shell bool, args []string, env map[string]string) []byte {
	t.Helper()
	return h.startFull(t, project, entry, dir, exec, "joe", shell, args, env)
}

func (h *harness) shell(t *testing.T) []byte {
	t.Helper()
	return h.startFull(t, "demo", "web", h.app, "/bin/sh", "joe", true, []string{"-c", "echo ok"}, map[string]string{})
}

func (h *harness) startFull(t *testing.T, project, entry, dir, exec, runAs string, shell bool, args []string, env map[string]string) []byte {
	t.Helper()
	if args == nil {
		args = []string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	b, err := json.Marshal(map[string]any{
		"project": project, "entry": entry, "dir": dir, "exec": exec,
		"args": args, "shell": shell, "run_as": runAs, "env": env, "restart": "on-failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) ref(t *testing.T, project, entry string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{"project": project, "entry": entry})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) logs(t *testing.T, project, entry string, lines int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"project": project, "entry": entry, "lines": lines})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writePolicy(t *testing.T, dir string, enabled, shell bool, max int, runAs, dirs, allow []string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "enabled = %t\n", enabled)
	b.WriteString("run_as = [" + joinTOML(runAs) + "]\n")
	b.WriteString("dirs = [" + joinTOML(dirs) + "]\n")
	b.WriteString("exec_allow = [" + joinTOML(allow) + "]\n")
	fmt.Fprintf(&b, "allow_shell = %t\n", shell)
	fmt.Fprintf(&b, "max_programs = %d\n", max)
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinTOML(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		v = strings.ReplaceAll(v, `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		parts[i] = `"` + v + `"`
	}
	return strings.Join(parts, ", ")
}

func refusalReason(t *testing.T, raw []byte) string {
	t.Helper()
	var m struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.OK {
		return ""
	}
	return m.Reason
}

func mustStatus(t *testing.T, raw []byte) programs.Status {
	t.Helper()
	st, reason, err := programs.DecodeStatus(raw)
	if err != nil || reason != "" {
		t.Fatalf("%s %v", raw, err)
	}
	return st
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s mode %o want %o", path, fi.Mode().Perm(), want)
	}
}

func readUnit(t *testing.T, h *harness, name string) string {
	t.Helper()
	b, err := os.ReadFile(unitFile(h.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeCgroup(t *testing.T, sysfs, unit, body string) {
	t.Helper()
	dir := filepath.Join(sysfs, "fs", "cgroup", "system.slice", unit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linkSock(t *testing.T, proc string, pid int, fd string, inode int) {
	t.Helper()
	dir := filepath.Join(proc, fmt.Sprintf("%d", pid), "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(dir, fd)); err != nil {
		t.Fatal(err)
	}
}

type fakeCtl struct {
	mu         sync.Mutex
	state      map[string]string
	mono       map[string]uint64
	calls      [][]string
	stopErr    bool
	restartErr bool
	reloadErr  bool
	noBump     bool
	reloadWait time.Duration
	activeErr  bool
	journal    string
}

func newFake() *fakeCtl {
	return &fakeCtl{state: map[string]string{}, mono: map[string]uint64{}}
}

func (f *fakeCtl) systemctl(_ context.Context, args []string) (string, error) {
	if len(args) > 0 && args[0] == "daemon-reload" && f.reloadWait > 0 {
		time.Sleep(f.reloadWait)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return "", fmt.Errorf("argv")
	}
	switch args[0] {
	case "daemon-reload":
		if f.reloadErr {
			return "", fmt.Errorf("reload")
		}
		return "", nil
	case "restart":
		if f.restartErr {
			return "", fmt.Errorf("restart")
		}
		f.state[args[1]] = "active"
		if !f.noBump {
			f.mono[args[1]]++
		}
		return "", nil
	case "stop":
		if f.stopErr {
			return "", fmt.Errorf("stop")
		}
		if _, ok := f.state[args[1]]; !ok {
			return "", fmt.Errorf("not loaded")
		}
		delete(f.state, args[1])
		return "", nil
	case "is-active":
		if f.activeErr {
			return "", fmt.Errorf("is-active")
		}
		st := f.state[args[1]]
		if st == "" {
			return "inactive\n", fmt.Errorf("inactive")
		}
		if st == "active" || st == "activating" {
			return st + "\n", nil
		}
		return st + "\n", fmt.Errorf("%s", st)
	case "show":
		unit := ""
		monoOnly := false
		for _, a := range args {
			if strings.HasSuffix(a, ".service") {
				unit = a
			}
			if a == "ActiveEnterTimestampMonotonic" {
				monoOnly = true
			}
		}
		if monoOnly {
			return fmt.Sprintf("ActiveEnterTimestampMonotonic=%d\n", f.mono[unit]), nil
		}
		st := f.state[unit]
		if st == "" {
			st = "inactive"
		}
		sub, pid, ts := "dead", "0", "0"
		if st == "active" || st == "activating" {
			sub, pid, ts = "running", "4242", "1700000000000000"
		}
		if st == "failed" {
			sub = "failed"
		}
		return fmt.Sprintf("ActiveState=%s\nSubState=%s\nMainPID=%s\nActiveEnterTimestampUSec=%s\n", st, sub, pid, ts), nil
	default:
		return "", fmt.Errorf("argv")
	}
}

func (f *fakeCtl) journalctl(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.journal, nil
}

func (f *fakeCtl) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == name {
			return true
		}
	}
	return false
}

func (f *fakeCtl) journalArgsOK(unit, lines string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) == 0 || c[0] != "-u" {
			continue
		}
		okU, okN := false, false
		for _, a := range c {
			if a == unit {
				okU = true
			}
			if a == lines {
				okN = true
			}
		}
		if okU && okN {
			return true
		}
	}
	return false
}

func TestStartedAtFormat(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	st := mustStatus(t, raw)
	if st.MainPID == nil || *st.MainPID != 4242 || st.StartedAt == nil {
		t.Fatalf("%+v", st)
	}
	want := protocol.FormatTime(time.UnixMicro(1700000000000000).UTC())
	if *st.StartedAt != want {
		t.Fatalf("started_at %s want %s", *st.StartedAt, want)
	}
}
