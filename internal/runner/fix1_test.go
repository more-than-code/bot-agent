package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestHandleRejectsOversizeFrame(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", bytesRepeat(programs.MaxFrame+1), h.sys())
	if refusalReason(t, raw) != programs.ReasonBadSpec {
		t.Fatalf("%s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("oversize start wrote a unit")
	}
}

func TestCountFailureFailsClosed(t *testing.T) {
	h := newHarness(t, true, false, 20)
	dir := unitDir(h.root)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
}

func TestInactiveUnitFileCounts(t *testing.T) {
	h := newHarness(t, true, false, 1)
	plantOwned(t, h, "demo", "api")
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("inactive unit file did not consume the slot")
	}
	if h.ctl.called("is-active") {
		t.Fatal("the file count called is-active")
	}
}

func TestMismatchedMarkersAreForeign(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	path := unitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "[Service]\nX-Deploybot-Project=other\nX-Deploybot-Entry=web\nExecStart=/bin/sh\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"start", "restart"} {
		in := h.ref(t, "demo", "web")
		if verb == "start" {
			in = h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
		}
		raw := Handle(context.Background(), verb, in, h.sys())
		if refusalReason(t, raw) != programs.ReasonNotOwned {
			t.Fatalf("%s %s", verb, raw)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("%s rewrote %s", verb, got)
		}
	}
	if h.ctl.called("restart") || h.ctl.called("stop") {
		t.Fatalf("calls %v", h.ctl.calls)
	}
}

func TestListOwnedUnits(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantOwned(t, h, "demo", "api")
	plantOwned(t, h, "demo", "web")
	h.ctl.state["deploybot-demo-api.service"] = "active"
	foreign := unitFile(h.root, "deploybot-other-skip.service")
	if err := os.WriteFile(foreign, []byte("ExecStart=/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), "list", []byte(`{}`), h.sys())
	units, reason, err := programs.DecodeList(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	got := map[string]string{}
	for _, u := range units {
		got[u.Unit] = u.Project + "/" + u.Entry + "/" + u.ActiveState
	}
	if got["deploybot-demo-api.service"] != "demo/api/active" || got["deploybot-demo-web.service"] != "demo/web/inactive" {
		t.Fatalf("%+v", units)
	}
	if _, ok := got["deploybot-other-skip.service"]; ok {
		t.Fatal("foreign unit listed")
	}
	if refusalReason(t, Handle(context.Background(), "list", []byte(`{"x":1}`), h.sys())) != programs.ReasonBadSpec {
		t.Fatal("non-empty list body")
	}
	missing := h.sys()
	missing.Policy = filepath.Join(h.base, "missing.toml")
	if refusalReason(t, Handle(context.Background(), "list", []byte(`{}`), missing)) != programs.ReasonPolicy {
		t.Fatal("disabled list")
	}
}

func TestLinesAndArgCaps(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	for _, n := range []int{0, 1001} {
		h.ctl.calls = nil
		raw = Handle(context.Background(), "logs", h.logs(t, "demo", "web", n), h.sys())
		if refusalReason(t, raw) != programs.ReasonBadSpec {
			t.Fatalf("lines %d %s", n, raw)
		}
		if h.ctl.called("-u") {
			t.Fatalf("journalctl ran for lines %d", n)
		}
	}
	for _, n := range []int{1, 1000} {
		raw = Handle(context.Background(), "logs", h.logs(t, "demo", "web", n), h.sys())
		if refusalReason(t, raw) != "" {
			t.Fatalf("lines %d %s", n, raw)
		}
	}
	args := make([]string, 33)
	for i := range args {
		args[i] = "a"
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "args", h.app, "/usr/bin/pnpm", false, args), h.sys())
	if refusalReason(t, raw) != programs.ReasonBadSpec {
		t.Fatalf("33 args %s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-args.service")); !os.IsNotExist(err) {
		t.Fatal("33 args wrote a unit")
	}
}

func TestHelperEnvDenylist(t *testing.T) {
	keys := []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "NODE_PATH", "BASH_ENV", "ENV", "IFS", "PYTHONPATH", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB"}
	for _, key := range keys {
		h := newHarness(t, true, false, 20)
		raw := Handle(context.Background(), "start", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{key: "x"}), h.sys())
		if refusalReason(t, raw) != programs.ReasonPolicy {
			t.Fatalf("%s %s", key, raw)
		}
		if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
			t.Fatalf("%s wrote a unit", key)
		}
	}
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, []string{"NODE_OPTIONS=--inspect"}), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("hoisted %s", raw)
	}
}

func TestStartTruth(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	h.ctl.state[unit] = "active"
	h.ctl.mono[unit] = 5
	h.ctl.restartErr = true
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
		t.Fatalf("nonzero restart %s", raw)
	}
	st := mustStatus(t, raw)
	if st.ActiveState != "active" {
		t.Fatalf("old process hidden %+v", st)
	}
	if _, err := os.Stat(unitFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("failed start left the unit file")
	}
	if !h.ctl.called("stop") {
		t.Fatal("failed start did not stop the unit")
	}

	h = newHarness(t, true, false, 20)
	h.ctl.noBump = true
	raw = Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
		t.Fatalf("stale monotonic %s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("stale activation left the unit file")
	}
}

func TestSocketsMapsTCP6(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	plantOwned(t, h, "demo", "web")
	proc := filepath.Join(h.root, "proc")
	sysfs := filepath.Join(h.root, "sys")
	writeCgroup(t, sysfs, unit, "42\n")
	linkSock(t, proc, 42, "3", 99)
	linkSock(t, proc, 7, "3", 77)
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	tcp := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 77 1 0000000000000000 100 0 0 10 0\n"
	tcp6 := "" +
		"  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000000000000000000000000000:0BD7 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 99 1 0000000000000000 100 0 0 10 0\n"
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp6"), []byte(tcp6), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := h.sys()
	sys.Proc = proc
	sys.Sysfs = sysfs
	raw := Handle(context.Background(), "sockets", h.ref(t, "demo", "web"), sys)
	ports, _, reason, err := programs.DecodeSockets(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	if len(ports) != 1 || ports[0] != 3031 {
		t.Fatalf("ports %v", ports)
	}
}

func TestDoStartRefusesRootGroup(t *testing.T) {
	h := newHarness(t, true, false, 20)
	res := programs.Resolved{
		Unit: "deploybot-demo-web.service", Project: "demo", Entry: "web",
		RunAs: "joe", Group: "root", Home: "/home/joe", Dir: h.app, Exec: "/usr/bin/pnpm", Restart: "no",
	}
	raw := doStart(context.Background(), res, h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, res.Unit)); !os.IsNotExist(err) {
		t.Fatal("root group wrote a unit")
	}
	if badGroup("0") == false || badGroup("joe") {
		t.Fatal("badGroup")
	}
}

func TestCommandEnvIsFixed(t *testing.T) {
	if systemctlBin != "/usr/bin/systemctl" || journalctlBin != "/usr/bin/journalctl" {
		t.Fatalf("%s %s", systemctlBin, journalctlBin)
	}
	out, err := command("systemctl")(context.Background(), nil)
	if err == nil || out != "" {
		t.Fatalf("relative bin %q %v", out, err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	body := "#!/bin/sh\nprintf '%s\\n' \"$0\"\nprintf '%s\\n' \"LANG=$LANG\"\nprintf '%s\\n' \"PATH=$PATH\"\nif [ -n \"$SENTINEL_SECRET\" ]; then printf '%s\\n' \"LEAK=$SENTINEL_SECRET\"; fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SENTINEL_SECRET", "hunter2")
	out, err = command(script)(context.Background(), []string{"is-active", "u.service"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, script) || !strings.Contains(out, "LANG=C") || !strings.Contains(out, "PATH=/usr/bin:/bin") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatal(out)
	}
}

func plantSpec(t *testing.T, h *harness, project, entry string) {
	t.Helper()
	unit, ok := programs.UnitName(project, entry)
	if !ok {
		t.Fatal(project, entry)
	}
	path := specFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, h.start(t, project, entry, h.app, "/usr/bin/pnpm", false, nil), 0o600); err != nil {
		t.Fatal(err)
	}
}

func plantOwned(t *testing.T, h *harness, project, entry string) {
	t.Helper()
	unit, ok := programs.UnitName(project, entry)
	if !ok {
		t.Fatal(project, entry)
	}
	path := unitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[Unit]\nX-Deploybot-Project=%s\nX-Deploybot-Entry=%s\n", project, entry)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bytesRepeat(n int) []byte {
	return []byte(strings.Repeat("a", n))
}
