package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestUpgradeRestartRefusesFieldBreak(t *testing.T) {
	unit := "deploybot-demo-web.service"
	cases := []struct {
		name   string
		reason string
		setup  func(t *testing.T, h *harness)
	}{
		{
			name:   "dir lf",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) {
				plantLegacy(t, h, "demo", "web")
				retargetDir(t, h.app, "x\nUser=root")
			},
		},
		{
			name:   "dir cr",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) {
				plantLegacy(t, h, "demo", "web")
				retargetDir(t, h.app, "x\rUser=root")
			},
		},
		{
			name:   "exec lf",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) {
				plantRunExec(t, h, "pnpm", nil)
				retargetBin(t, h.app, "pwned\nUser=root")
			},
		},
		{
			name:   "exec cr",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) {
				plantRunExec(t, h, "pnpm", nil)
				retargetBin(t, h.app, "pwned\rUser=root")
			},
		},
		{
			name:   "arg cr",
			reason: programs.ReasonBadSpec,
			setup: func(t *testing.T, h *harness) {
				plantRunExec(t, h, "/usr/bin/pnpm", []string{"keep\r"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			tc.setup(t, h)
			run := []string{legacyUnitFile(h.root, unit), legacyEnvFile(h.root, unit), legacySpecFile(h.root, unit)}
			before := make([][]byte, len(run))
			for i, path := range run {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = b
			}
			raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
			if refusalReason(t, raw) != tc.reason {
				t.Fatalf("%s", raw)
			}
			if _, err := os.Lstat(unitFile(h.root, unit)); !os.IsNotExist(err) {
				t.Fatal("wrote an /etc unit")
			}
			assertGone(t, envFile(h.root, unit))
			assertGone(t, specFile(h.root, unit))
			for i, path := range run {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(before[i]) {
					t.Fatalf("%s changed", path)
				}
			}
			if h.ctl.called("daemon-reload") || h.ctl.called("enable") || h.ctl.called("restart") {
				t.Fatalf("calls %v", h.ctl.calls)
			}
		})
	}
}

func TestStopRunOnly(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	st := mustStatus(t, raw)
	if st.ActiveState != "inactive" || st.Unit != unit {
		t.Fatalf("%+v", st)
	}
	want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	assertBytesGone(t, legacyUnitFile(h.root, unit))
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))
	assertGone(t, unitFile(h.root, unit))
}

func TestStopDisableErrorLeavesFiles(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantOwned(t, h, "demo", "web")
	plantSpec(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ctl.state[unit] = "active"
	h.ctl.disableErr = true
	raw := Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatalf("%s", raw)
	}
	if h.ctl.called("stop") {
		t.Fatalf("stop ran: %v", h.ctl.calls)
	}
	for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStopEnvSymlinkAfterDisable(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantOwned(t, h, "demo", "web")
	plantLegacy(t, h, "demo", "web")
	plantSpec(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	target := filepath.Join(h.base, "keep")
	if err := os.WriteFile(target, []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := envFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatalf("%s", raw)
	}
	want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	assertSymlink(t, link, "KEEP\n")
	assertGone(t, specFile(h.root, unit))
	assertGone(t, unitFile(h.root, unit))
	assertBytesGone(t, legacyUnitFile(h.root, unit))
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))

	// The env symlink has no matching spec, so it is not owned.
	h.ctl.calls = nil
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatalf("orphan env %s", raw)
	}
	if st := mustStatus(t, raw); st.ActiveState != "inactive" {
		t.Fatalf("orphan %+v", st)
	}
	if len(h.ctl.calls) != 0 {
		t.Fatalf("orphan calls %v", h.ctl.calls)
	}
	assertSymlink(t, link, "KEEP\n")

	plantSpec(t, h, "demo", "web")
	h.ctl.calls = nil
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatalf("leftover spec %s", raw)
	}
	if st := mustStatus(t, raw); st.ActiveState != "inactive" {
		t.Fatalf("leftover %+v", st)
	}
	if len(h.ctl.calls) != 0 {
		t.Fatalf("leftover calls %v", h.ctl.calls)
	}
	assertGone(t, specFile(h.root, unit))
	assertSymlink(t, link, "KEEP\n")
}

func TestStartLeavesForeignRun(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	plantOwned(t, h, "demo", "web")
	const foreign = "[Service]\nExecStart=/bin/sh\n"
	plantForeign(t, legacyUnitFile(h.root, unit), foreign)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
		t.Fatalf("%s", raw)
	}
	got, err := os.ReadFile(legacyUnitFile(h.root, unit))
	if err != nil || string(got) != foreign {
		t.Fatalf("start changed /run %q", got)
	}
}

func TestEtcOursLeavesRunSymlink(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	plantOwned(t, h, "demo", "web")
	plantSpec(t, h, "demo", "web")
	target := filepath.Join(h.base, "keep")
	if err := os.WriteFile(target, []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := legacyUnitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	unitBefore, err := os.ReadFile(unitFile(h.root, unit))
	if err != nil {
		t.Fatal(err)
	}
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "status", h.ref(t, "demo", "web"), h.sys())
	st := mustStatus(t, raw)
	if st.Unit != unit || st.ActiveState != "active" {
		t.Fatalf("status %+v", st)
	}
	raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatalf("restart %s", raw)
	}
	got, err := os.ReadFile(unitFile(h.root, unit))
	if err != nil || string(got) != string(unitBefore) {
		t.Fatal("restart rewrote /etc")
	}
	assertSymlink(t, link, "KEEP\n")
	h.ctl.state[unit] = "active"
	h.ctl.calls = nil
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatalf("stop %s", raw)
	}
	assertGone(t, unitFile(h.root, unit))
	assertSymlink(t, link, "KEEP\n")
}

func plantRunExec(t *testing.T, h *harness, exec string, args []string) {
	t.Helper()
	plantLegacy(t, h, "demo", "web")
	bin := filepath.Join(h.app, "node_modules", ".bin", "pnpm")
	if exec == "pnpm" {
		if err := os.Remove(bin); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("bin\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unit := "deploybot-demo-web.service"
	if err := os.WriteFile(legacySpecFile(h.root, unit), h.start(t, "demo", "web", h.app, exec, false, args), 0o600); err != nil {
		t.Fatal(err)
	}
}

func retargetDir(t *testing.T, app, name string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, real+".real"); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(filepath.Dir(real), name)
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, real); err != nil {
		t.Fatal(err)
	}
}

func retargetBin(t *testing.T, app, name string) {
	t.Helper()
	bin := filepath.Join(app, "node_modules", ".bin", "pnpm")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(app, name)
	if err := os.WriteFile(dest, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, bin); err != nil {
		t.Fatal(err)
	}
}

func assertSymlink(t *testing.T, link, targetBody string) {
	t.Helper()
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was removed")
	}
	got, err := os.ReadFile(link)
	if err != nil || string(got) != targetBody {
		t.Fatalf("target %q err %v", got, err)
	}
}
