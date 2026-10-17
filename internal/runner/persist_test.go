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

func TestPersistentPathsAndModes(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	unit := "deploybot-demo-web.service"
	unitPath := filepath.Join(h.root, "etc", "systemd", "system", unit)
	envPath := filepath.Join(h.root, "var", "lib", "bot-agent-runner", "programs", unit+".env")
	specPath := filepath.Join(h.root, "var", "lib", "bot-agent-runner", "programs", unit+".json")
	progDir := filepath.Join(h.root, "var", "lib", "bot-agent-runner", "programs")
	assertMode(t, unitPath, 0o644)
	assertMode(t, envPath, 0o600)
	assertMode(t, specPath, 0o600)
	assertMode(t, progDir, 0o700)
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(h.root, envPath)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := "/" + filepath.ToSlash(rel)
	if !strings.Contains(string(body), "[Install]\n") || !strings.Contains(string(body), "WantedBy=multi-user.target\n") || !strings.Contains(string(body), "EnvironmentFile="+wantEnv+"\n") {
		t.Fatalf("unit\n%s", body)
	}
	assertDirEmptyOrMissing(t, filepath.Join(h.root, "run", "systemd", "system"))
	assertDirEmptyOrMissing(t, filepath.Join(h.root, "run", "bot-agent", "env"))
	if _, err := os.Lstat(filepath.Join(h.root, "run", "systemd", "system", unit)); !os.IsNotExist(err) {
		t.Fatal("wrote a /run unit")
	}
	if _, err := os.Lstat(filepath.Join(h.root, "run", "bot-agent", "env", unit+".env")); !os.IsNotExist(err) {
		t.Fatal("wrote a /run env")
	}
}

func TestSystemctlOrder(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	want := [][]string{
		{"daemon-reload"},
		{"enable", unit},
		{"restart", unit},
	}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("start %v", got)
	}

	h.ctl.calls = nil
	raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	want = [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("restart %v", got)
	}

	h.ctl.calls = nil
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	want = [][]string{
		{"disable", unit},
		{"stop", unit},
		{"daemon-reload"},
	}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("stop %v", got)
	}
	assertGone(t, unitFile(h.root, unit))
	assertGone(t, envFile(h.root, unit))
	assertGone(t, specFile(h.root, unit))
	assertGone(t, legacyUnitFile(h.root, unit))
	assertGone(t, legacyEnvFile(h.root, unit))
	assertGone(t, legacySpecFile(h.root, unit))
}

func TestForeignRunUnitUntouched(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	path := legacyUnitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const foreign = "[Service]\nExecStart=/bin/sh\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"start", "stop", "restart", "status", "logs", "sockets"} {
		in := h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
		if verb == "logs" {
			in = h.logs(t, "demo", "web", 10)
		} else if verb != "start" {
			in = h.ref(t, "demo", "web")
		}
		raw := Handle(context.Background(), verb, in, h.sys())
		if refusalReason(t, raw) != programs.ReasonNotOwned {
			t.Fatalf("%s %s", verb, raw)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != foreign {
			t.Fatalf("%s touched the /run unit: %s", verb, got)
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("%s calls %v", verb, h.ctl.calls)
		}
	}
	if h.ctl.called("enable") || h.ctl.called("disable") || h.ctl.called("stop") || h.ctl.called("restart") {
		t.Fatalf("systemd was called: %v", h.ctl.calls)
	}
	if _, err := os.Lstat(unitFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("foreign /run unit caused an /etc write")
	}
}

func TestSymlinkPathsRefused(t *testing.T) {
	unit := "deploybot-demo-web.service"
	rels := []string{
		filepath.Join("etc", "systemd", "system", unit),
		filepath.Join("var", "lib", "bot-agent-runner", "programs", unit+".env"),
		filepath.Join("var", "lib", "bot-agent-runner", "programs", unit+".json"),
		filepath.Join("run", "systemd", "system", unit),
		filepath.Join("run", "bot-agent", "env", unit+".env"),
		filepath.Join("run", "bot-agent", "env", unit+".json"),
	}
	for _, rel := range rels {
		t.Run(rel, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			target := filepath.Join(h.base, "keep")
			if err := os.WriteFile(target, []byte("KEEP\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(h.root, rel)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			unitLink := rel == filepath.Join("etc", "systemd", "system", unit) || rel == filepath.Join("run", "systemd", "system", unit)
			for _, verb := range []string{"start", "restart", "stop"} {
				in := h.ref(t, "demo", "web")
				if verb == "start" {
					in = h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
				}
				raw := Handle(context.Background(), verb, in, h.sys())
				// Stop only refuses a symlink at the unit file it would load.
				// Env and spec links are not a unit, so stop is inactive.
				if verb == "stop" && !unitLink {
					if refusalReason(t, raw) != "" {
						t.Fatalf("stop %s", raw)
					}
					if st := mustStatus(t, raw); st.ActiveState != "inactive" {
						t.Fatalf("stop %+v", st)
					}
					continue
				}
				if refusalReason(t, raw) != programs.ReasonIO {
					t.Fatalf("%s %s", verb, raw)
				}
			}
			if len(h.ctl.calls) != 0 {
				t.Fatalf("systemctl %v", h.ctl.calls)
			}
			for _, other := range rels {
				if other == rel {
					continue
				}
				assertGone(t, filepath.Join(h.root, other))
			}
			fi, err := os.Lstat(link)
			if err != nil || fi.Mode()&os.ModeSymlink == 0 {
				if unitLink {
					t.Fatal("unit path is no longer a symlink")
				}
				t.Fatal("symlink was removed")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "KEEP\n" {
				t.Fatalf("target %q", got)
			}
		})
	}
}

func TestProgramsDirSymlinkRefused(t *testing.T) {
	h := newHarness(t, true, false, 20)
	target := filepath.Join(h.base, "programs-target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	programsPath := filepath.Join(h.root, "var", "lib", "bot-agent-runner", "programs")
	if err := os.MkdirAll(filepath.Dir(programsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, programsPath); err != nil {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatal(string(raw))
	}
	fi, err := os.Lstat(target)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("target mode %v err %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(h.root, "etc", "systemd", "system", "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("wrote a unit through the programs symlink")
	}
	if len(h.ctl.calls) != 0 {
		t.Fatalf("systemctl %v", h.ctl.calls)
	}
}

func TestUpgradeFromRun(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	h.ctl.state[unit] = "active"

	envBefore, err := os.ReadFile(legacyEnvFile(h.root, unit))
	if err != nil || string(envBefore) != "PORT=1\n" {
		t.Fatal(err)
	}
	specBefore, err := os.ReadFile(legacySpecFile(h.root, unit))
	if err != nil || len(specBefore) == 0 {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
		t.Fatal(string(raw))
	}
	want := [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("upgrade restart %v", got)
	}
	body := readUnit(t, h, unit)
	if !strings.Contains(body, "WantedBy=multi-user.target\n") || !strings.Contains(body, "EnvironmentFile=/var/lib/bot-agent-runner/programs/"+unit+".env\n") || !strings.Contains(body, `ExecStart="/usr/bin/pnpm"`) {
		t.Fatalf("etc unit\n%s", body)
	}
	assertMode(t, unitFile(h.root, unit), 0o644)
	assertMode(t, envFile(h.root, unit), 0o600)
	assertMode(t, specFile(h.root, unit), 0o600)
	assertBytesGone(t, legacyUnitFile(h.root, unit))
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))
	if !h.ctl.called("enable") {
		t.Fatalf("calls %v", h.ctl.calls)
	}

	plantLegacy(t, h, "demo", "web")
	if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	h.ctl.state[unit] = "active"
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	for _, path := range programPaths(h.root, unit) {
		assertGone(t, path)
	}
	want = [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("stop both %v", got)
	}
}

func TestRunOnlyUnitIsVisible(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	h.ctl.state[unit] = "active"
	proc := filepath.Join(h.root, "proc")
	sysfs := filepath.Join(h.root, "sys")
	writeCgroup(t, sysfs, unit, "42\n")
	linkSock(t, proc, 42, "3", 99)
	tcp := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0BD7 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 99 1 0000000000000000 100 0 0 10 0\n"
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(tcp), 0o644); err != nil {
		t.Fatal(err)
	}
	sys := h.sys()
	sys.Proc = proc
	sys.Sysfs = sysfs

	raw := Handle(context.Background(), "status", h.ref(t, "demo", "web"), sys)
	st := mustStatus(t, raw)
	if st.Unit != unit || st.ActiveState != "active" {
		t.Fatalf("%+v", st)
	}
	h.ctl.journal = "from-run\n"
	raw = Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), sys)
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil || reason != "" || len(batch.Lines) != 1 || batch.Lines[0] != "from-run" {
		t.Fatalf("%s %v", raw, err)
	}
	raw = Handle(context.Background(), "list", []byte(`{}`), sys)
	units, reason, err := programs.DecodeList(raw)
	if err != nil || reason != "" || len(units) != 1 || units[0].Unit != unit {
		t.Fatalf("%s %v", raw, err)
	}
	raw = Handle(context.Background(), "sockets", h.ref(t, "demo", "web"), sys)
	ports, _, reason, err := programs.DecodeSockets(raw)
	if err != nil || reason != "" || len(ports) != 1 || ports[0] != 3031 {
		t.Fatalf("sockets %s %v", raw, err)
	}
}

func TestUnitInBothPlacesCountsOnce(t *testing.T) {
	h := newHarness(t, true, false, 2)
	plantOwned(t, h, "demo", "api")
	plantLegacy(t, h, "demo", "api")
	n, have, ok := countUnits(h.sys())
	if !ok || n != 1 || !have["deploybot-demo-api.service"] {
		t.Fatalf("count %d %v ok %v", n, have, ok)
	}
	raw := Handle(context.Background(), "list", []byte(`{}`), h.sys())
	units, reason, err := programs.DecodeList(raw)
	if err != nil || reason != "" || len(units) != 1 {
		t.Fatalf("%s %v", raw, err)
	}
	raw = Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatalf("a unit stored in both places consumed two slots: %s", raw)
	}
}

func TestStartMovesRunUnit(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
		t.Fatalf("%s", raw)
	}
	want := [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("upgrade start %v", got)
	}
	if _, err := os.Lstat(unitFile(h.root, unit)); err != nil {
		t.Fatal(err)
	}
	assertBytesGone(t, legacyUnitFile(h.root, unit))
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))
}

func TestFailedStartRemovesBothCopies(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	paths := programPaths(h.root, unit)
	sys, saw, reloaded := reloadSeesFiles(h, paths)
	h.ctl.reloadErr = true
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), sys)
	if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
		t.Fatalf("%s", raw)
	}
	for _, path := range paths {
		assertGone(t, path)
	}
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))
	if !*reloaded || *saw {
		t.Fatal("cleanup daemon-reload did not run after the files were gone")
	}
	if h.ctl.called("enable") {
		t.Fatal("enable ran after reload failed")
	}
	want := [][]string{{"daemon-reload"}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
}

func TestUpgradeRestartReloadOrEnableFailureKeepsRun(t *testing.T) {
	for _, name := range []string{"reload", "enable"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			plantLegacy(t, h, "demo", "web")
			unit := "deploybot-demo-web.service"
			run := []string{legacyUnitFile(h.root, unit), legacyEnvFile(h.root, unit), legacySpecFile(h.root, unit)}
			before := make([][]byte, len(run))
			for i, path := range run {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[i] = b
			}
			sys := h.sys()
			var snaps [][3]bool
			if name == "enable" {
				etc := [3]string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)}
				sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
					var present [3]bool
					for i, path := range etc {
						_, err := os.Lstat(path)
						present[i] = err == nil
					}
					snaps = append(snaps, present)
					return h.ctl.systemctl(ctx, args)
				}
			}
			if name == "reload" {
				h.ctl.reloadErr = true
			} else {
				h.ctl.enableErr = true
			}
			raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), sys)
			if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
				assertGone(t, path)
			}
			for i, path := range run {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(before[i]) {
					t.Fatalf("%s changed", path)
				}
			}
			if h.ctl.called("disable") || h.ctl.called("stop") {
				t.Fatalf("calls %v", h.ctl.calls)
			}
			want := [][]string{{"daemon-reload"}}
			if name == "enable" {
				want = [][]string{{"daemon-reload"}, {"enable", unit}, {"daemon-reload"}}
			}
			if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
				t.Fatalf("calls %v", got)
			}
			if name == "enable" {
				if len(snaps) != 3 || snaps[0] != [3]bool{true, true, true} || snaps[1] != [3]bool{true, true, true} || snaps[2] != [3]bool{false, false, false} {
					t.Fatalf("presence %v", snaps)
				}
			}

			h.ctl.reloadErr = false
			h.ctl.enableErr = false
			h.ctl.calls = nil
			raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
			if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
				t.Fatalf("retry %s", raw)
			}
			for _, path := range run {
				assertBytesGone(t, path)
			}
			if _, err := os.Lstat(unitFile(h.root, unit)); err != nil {
				t.Fatal(err)
			}
			if !h.ctl.called("enable") {
				t.Fatalf("retry calls %v", h.ctl.calls)
			}
		})
	}
}

func TestStartUpgradeActivationFailureRemovesBoth(t *testing.T) {
	for _, name := range []string{"restartErr", "noBump"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			plantLegacy(t, h, "demo", "web")
			unit := "deploybot-demo-web.service"
			if name == "restartErr" {
				h.ctl.restartErr = true
			} else {
				h.ctl.noBump = true
			}
			h.ctl.state[unit] = "active"
			paths := programPaths(h.root, unit)
			sys, saw, reloaded := reloadSeesFiles(h, paths)
			raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), sys)
			if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			for _, path := range paths {
				assertGone(t, path)
			}
			assertBytesGone(t, legacyEnvFile(h.root, unit))
			assertBytesGone(t, legacySpecFile(h.root, unit))
			if name == "restartErr" && h.ctl.mono[unit] == 0 {
				t.Fatal("restartErr did not bump monotonic time")
			}
			if name == "noBump" && h.ctl.mono[unit] != 0 {
				t.Fatal("noBump moved monotonic time")
			}
			if !*reloaded || *saw {
				t.Fatal("cleanup daemon-reload did not run after the files were gone")
			}
		})
	}
}

func TestRestartUpgradeActivationFailureKeepsEtc(t *testing.T) {
	for _, name := range []string{"restartErr", "noBump"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			plantLegacy(t, h, "demo", "web")
			unit := "deploybot-demo-web.service"
			if name == "restartErr" {
				h.ctl.restartErr = true
			} else {
				h.ctl.noBump = true
			}
			raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
			if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal(err)
				}
			}
			assertBytesGone(t, legacyUnitFile(h.root, unit))
			assertBytesGone(t, legacyEnvFile(h.root, unit))
			assertBytesGone(t, legacySpecFile(h.root, unit))
			want := [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}
			if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
				t.Fatalf("calls %v", got)
			}
			if h.ctl.called("disable") || h.ctl.called("stop") {
				t.Fatalf("calls %v", h.ctl.calls)
			}
			if name == "noBump" && h.ctl.mono[unit] != 0 {
				t.Fatal("noBump moved monotonic time")
			}
			if name == "restartErr" && h.ctl.mono[unit] == 0 {
				t.Fatal("restartErr did not bump monotonic time")
			}
		})
	}
}

func TestMixedOwnershipRefused(t *testing.T) {
	const foreign = "[Service]\nExecStart=/bin/sh\n"

	t.Run("etc ours run foreign", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		unit := "deploybot-demo-web.service"
		plantOwned(t, h, "demo", "web")
		plantSpec(t, h, "demo", "web")
		if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		plantForeign(t, legacyUnitFile(h.root, unit), foreign)
		runBefore, err := os.ReadFile(legacyUnitFile(h.root, unit))
		if err != nil {
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
		h.ctl.journal = "from-etc\n"
		raw = Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys())
		batch, reason, err := programs.DecodeLogs(raw)
		if err != nil || reason != "" || len(batch.Lines) != 1 || batch.Lines[0] != "from-etc" {
			t.Fatalf("logs %s %v", raw, err)
		}
		h.ctl.calls = nil
		raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
		if refusalReason(t, raw) != "" {
			t.Fatal(string(raw))
		}
		got, err := os.ReadFile(unitFile(h.root, unit))
		if err != nil || string(got) != string(unitBefore) {
			t.Fatal("restart rewrote /etc")
		}
		got, err = os.ReadFile(legacyUnitFile(h.root, unit))
		if err != nil || string(got) != string(runBefore) {
			t.Fatal("restart changed /run")
		}
		h.ctl.calls = nil
		h.ctl.state[unit] = "active"
		raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
		if refusalReason(t, raw) != "" {
			t.Fatal(string(raw))
		}
		if st = mustStatus(t, raw); st.ActiveState != "inactive" {
			t.Fatalf("stop %+v", st)
		}
		want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
		if gotCalls := systemctlVerbs(h.ctl); !callsEqual(gotCalls, want) {
			t.Fatalf("stop %v", gotCalls)
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, envFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
		got, err = os.ReadFile(legacyUnitFile(h.root, unit))
		if err != nil || string(got) != foreign {
			t.Fatalf("stop changed /run %q", got)
		}
	})

	t.Run("etc foreign run ours", func(t *testing.T) {
		h := newHarness(t, true, false, 1)
		unit := "deploybot-demo-web.service"
		plantLegacy(t, h, "demo", "web")
		plantForeign(t, unitFile(h.root, unit), foreign)
		paths := []string{
			unitFile(h.root, unit),
			legacyUnitFile(h.root, unit),
			legacyEnvFile(h.root, unit),
			legacySpecFile(h.root, unit),
		}
		before := map[string]string{}
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before[path] = string(b)
		}
		for _, verb := range []string{"start", "restart", "stop", "status", "logs", "sockets"} {
			in := h.ref(t, "demo", "web")
			if verb == "start" {
				in = h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
			}
			if verb == "logs" {
				in = h.logs(t, "demo", "web", 10)
			}
			h.ctl.calls = nil
			raw := Handle(context.Background(), verb, in, h.sys())
			if refusalReason(t, raw) != programs.ReasonNotOwned {
				t.Fatalf("%s %s", verb, raw)
			}
			if len(h.ctl.calls) != 0 {
				t.Fatalf("%s calls %v", verb, h.ctl.calls)
			}
		}
		for _, path := range paths {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != before[path] {
				t.Fatalf("%s changed", path)
			}
		}
		raw := Handle(context.Background(), "list", []byte(`{}`), h.sys())
		units, reason, err := programs.DecodeList(raw)
		if err != nil || reason != "" || len(units) != 0 {
			t.Fatalf("list %s %v", raw, err)
		}
		if h.ctl.called("is-active") {
			t.Fatal("list asked systemd about the hidden unit")
		}
		raw = Handle(context.Background(), "start", h.start(t, "demo", "api", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonPolicy {
			t.Fatalf("max_programs %s", raw)
		}
		if _, err := os.Lstat(unitFile(h.root, "deploybot-demo-api.service")); !os.IsNotExist(err) {
			t.Fatal("second unit was written")
		}
	})
}

func TestPolicyDeniedRestartRemovesRunOnly(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "workspace")}, []string{"/usr/bin/node"})
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatal(string(raw))
	}
	for _, path := range programPaths(h.root, unit) {
		assertGone(t, path)
	}
	assertBytesGone(t, legacyEnvFile(h.root, unit))
	assertBytesGone(t, legacySpecFile(h.root, unit))
	want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
}

func TestRestartRefusesNewlineInResolvedDir(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	path := unitFile(h.root, unit)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(h.app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, real+".real"); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(filepath.Dir(real), "x\nUser=root")
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, real); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(before) {
		t.Fatalf("unit changed\n%s", got)
	}
	if h.ctl.called("daemon-reload") || h.ctl.called("enable") || h.ctl.called("restart") {
		t.Fatalf("calls %v", h.ctl.calls)
	}
}

func TestRestartRefusesNewlineInResolvedExec(t *testing.T) {
	h := newHarness(t, true, false, 20)
	bin := filepath.Join(h.app, "node_modules", ".bin", "pnpm")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("bin\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unit := "deploybot-demo-web.service"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	spec, err := os.ReadFile(specFile(h.root, unit))
	if err != nil || !strings.Contains(string(spec), "node_modules/.bin/pnpm") {
		t.Fatalf("stored exec %s", spec)
	}
	path := unitFile(h.root, unit)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(h.app, "pwned\nUser=root")
	if err := os.WriteFile(dest, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, bin); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(before) {
		t.Fatalf("unit changed\n%s", got)
	}
	if h.ctl.called("daemon-reload") || h.ctl.called("enable") || h.ctl.called("restart") {
		t.Fatalf("calls %v", h.ctl.calls)
	}
}

func plantLegacy(t *testing.T, h *harness, project, entry string) {
	t.Helper()
	unit, ok := programs.UnitName(project, entry)
	if !ok {
		t.Fatal(project, entry)
	}
	path := legacyUnitFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[Unit]\nX-Deploybot-Project=%s\nX-Deploybot-Entry=%s\n", project, entry)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := legacyEnvFile(h.root, unit)
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacySpecFile(h.root, unit), h.start(t, project, entry, h.app, "/usr/bin/pnpm", false, nil), 0o600); err != nil {
		t.Fatal(err)
	}
}

func systemctlVerbs(f *fakeCtl) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) == 0 {
			continue
		}
		switch c[0] {
		case "show", "is-active":
			continue
		default:
			out = append(out, append([]string(nil), c...))
		}
	}
	return out
}

func callsEqual(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("still present %s (%v)", path, err)
	}
}

func assertDirEmptyOrMissing(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		names := make([]string, len(ents))
		for i, e := range ents {
			names[i] = e.Name()
		}
		t.Fatalf("%s not empty: %v", dir, names)
	}
}

func plantForeign(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertBytesGone(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err == nil {
		t.Fatalf("%s still holds %q", path, got)
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// reloadSeesFiles reports whether a daemon-reload after stop still saw one
// of paths, and whether that cleanup reload ran at all.
func reloadSeesFiles(h *harness, paths []string) (Sys, *bool, *bool) {
	saw := false
	reloaded := false
	stopped := false
	sys := h.sys()
	sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
		out, err := h.ctl.systemctl(ctx, args)
		if len(args) > 0 && args[0] == "daemon-reload" && err != nil {
			// The first reload is the failure under test. Cleanup reload
			// must be allowed to succeed so a finished discard stays
			// start_failed rather than a second reload error.
			h.ctl.reloadErr = false
		}
		if len(args) > 0 && args[0] == "stop" {
			stopped = true
		}
		if len(args) > 0 && args[0] == "daemon-reload" && stopped {
			reloaded = true
			for _, path := range paths {
				if _, statErr := os.Lstat(path); statErr == nil {
					saw = true
				}
			}
		}
		return out, err
	}
	return sys, &saw, &reloaded
}
