package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestRestartOfEtcUnitKeepsBytesOnFailure(t *testing.T) {
	unit := "deploybot-demo-web.service"
	cases := []struct {
		name string
		set  func(*fakeCtl)
		want [][]string
	}{
		{"reloadErr", func(f *fakeCtl) { f.reloadErr = true }, [][]string{{"daemon-reload"}}},
		{"enableErr", func(f *fakeCtl) { f.enableErr = true }, [][]string{{"daemon-reload"}, {"enable", unit}}},
		{"restartErr", func(f *fakeCtl) { f.restartErr = true }, [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}},
		{"noBump", func(f *fakeCtl) { f.noBump = true }, [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			plantOwned(t, h, "demo", "web")
			plantSpec(t, h, "demo", "web")
			if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := map[string]fileSnap{}
			for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
				before[path] = snapFile(t, path)
			}
			tc.set(h.ctl)
			raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
			if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			for path, snap := range before {
				got := snapFile(t, path)
				if got.body != snap.body || !got.mod.Equal(snap.mod) {
					t.Fatalf("%s changed", path)
				}
			}
			if h.ctl.called("disable") || h.ctl.called("stop") {
				t.Fatalf("calls %v", h.ctl.calls)
			}
			if got := systemctlVerbs(h.ctl); !callsEqual(got, tc.want) {
				t.Fatalf("calls %v", got)
			}
		})
	}
}

func TestPolicyDeniedRestartRemovesEtc(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantOwned(t, h, "demo", "web")
	plantSpec(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "workspace")}, []string{"/usr/bin/node"})
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatal(string(raw))
	}
	assertGone(t, unitFile(h.root, unit))
	assertGone(t, envFile(h.root, unit))
	assertGone(t, specFile(h.root, unit))
	want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
}

func TestFailedStartMutatingCalls(t *testing.T) {
	unit := "deploybot-demo-web.service"
	const foreign = "[Service]\nExecStart=/bin/sh\n"
	cases := []struct {
		name string
		set  func(*fakeCtl)
		want [][]string
	}{
		{
			"reloadErr",
			func(f *fakeCtl) { f.reloadErr = true },
			[][]string{{"daemon-reload"}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}},
		},
		{
			"enableErr",
			func(f *fakeCtl) { f.enableErr = true },
			[][]string{{"daemon-reload"}, {"enable", unit}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}},
		},
		{
			"restartErr",
			func(f *fakeCtl) { f.restartErr = true },
			[][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}},
		},
		{
			"noBump",
			func(f *fakeCtl) { f.noBump = true },
			[][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			plantOwned(t, h, "demo", "web")
			plantSpec(t, h, "demo", "web")
			if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			plantForeign(t, legacyUnitFile(h.root, unit), foreign)
			h.ctl.state[unit] = "active"
			tc.set(h.ctl)
			presentAtStop := false
			sys := h.sys()
			sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
				if len(args) > 0 && args[0] == "stop" {
					presentAtStop = true
					for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
						if _, err := os.Lstat(path); err != nil {
							presentAtStop = false
						}
					}
				}
				out, err := h.ctl.systemctl(ctx, args)
				if len(args) > 0 && args[0] == "daemon-reload" && err != nil {
					h.ctl.reloadErr = false
				}
				return out, err
			}
			raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), sys)
			if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			if !presentAtStop {
				t.Fatal("persistent files were already gone when stop ran")
			}
			for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
				assertGone(t, path)
			}
			got, err := os.ReadFile(legacyUnitFile(h.root, unit))
			if err != nil || string(got) != foreign {
				t.Fatalf("run file %q", got)
			}
			if calls := systemctlVerbs(h.ctl); !callsEqual(calls, tc.want) {
				t.Fatalf("calls %v", calls)
			}
		})
	}
}

func TestFailedStartCleanupIO(t *testing.T) {
	unit := "deploybot-demo-web.service"
	t.Run("remove", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		h.ctl.reloadErr = true
		h.ctl.state[unit] = "active"
		target := filepath.Join(h.base, "keep")
		if err := os.WriteFile(target, []byte("KEEP\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sys := h.sys()
		sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
			out, err := h.ctl.systemctl(ctx, args)
			if len(args) > 0 && args[0] == "daemon-reload" && err != nil {
				link := envFile(h.root, unit)
				_ = os.Remove(link)
				if syErr := os.Symlink(target, link); syErr != nil {
					t.Errorf("symlink: %v", syErr)
				}
				h.ctl.reloadErr = false
			}
			return out, err
		}
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), sys)
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		assertSymlink(t, envFile(h.root, unit), "KEEP\n")
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
	})
	t.Run("disable", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		h.ctl.restartErr = true
		h.ctl.disableErr = true
		h.ctl.state[unit] = "active"
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		if !h.ctl.called("stop") {
			t.Fatalf("disable failure skipped stop: %v", h.ctl.calls)
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, envFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
	})
}

func TestFieldBreakCRAndBackslash(t *testing.T) {
	unit := "deploybot-demo-web.service"
	refused := []struct {
		name   string
		reason string
		setup  func(t *testing.T, h *harness) []byte
	}{
		{
			name:   "env cr",
			reason: programs.ReasonBadSpec,
			setup: func(t *testing.T, h *harness) []byte {
				return h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{"PORT": "1\r2"})
			},
		},
		{
			name:   "home cr",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) []byte {
				rewriteHome(t, h, "/home/joe\r")
				return h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
			},
		},
		{
			name:   "dir backslash",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) []byte {
				retargetDir(t, h.app, "x\\User=root")
				return h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
			},
		},
		{
			name:   "exec backslash",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) []byte {
				retargetBin(t, h.app, "pwned\\User=root")
				return h.start(t, "demo", "web", h.app, "pnpm", false, nil)
			},
		},
		{
			name:   "home backslash",
			reason: programs.ReasonPolicy,
			setup: func(t *testing.T, h *harness) []byte {
				rewriteHome(t, h, `/home/joe\`)
				return h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
			},
		},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, false, 20)
			raw := Handle(context.Background(), "start", tc.setup(t, h), h.sys())
			if refusalReason(t, raw) != tc.reason {
				t.Fatalf("%s", raw)
			}
			if _, err := os.Lstat(unitFile(h.root, unit)); !os.IsNotExist(err) {
				t.Fatal("wrote an /etc unit")
			}
			assertGone(t, envFile(h.root, unit))
			assertGone(t, specFile(h.root, unit))
			if h.ctl.called("daemon-reload") || h.ctl.called("enable") || h.ctl.called("restart") {
				t.Fatalf("calls %v", h.ctl.calls)
			}
		})
	}
	t.Run("env backslash allowed", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		raw := Handle(context.Background(), "start", h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, map[string]string{"PORT": `a\b`}), h.sys())
		if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
			t.Fatalf("%s", raw)
		}
	})
}

func TestDropInRefused(t *testing.T) {
	unit := "deploybot-demo-web.service"
	kinds := []struct {
		name string
		run  bool
		kind string
	}{
		{"etc dir", false, "dir"},
		{"run dir", true, "dir"},
		{"etc file", false, "file"},
		{"run symlink", true, "link"},
	}
	for _, verb := range []string{"start", "restart"} {
		for _, kind := range kinds {
			t.Run(verb+" "+kind.name, func(t *testing.T) {
				h := newHarness(t, true, false, 20)
				var unitBefore []byte
				if verb == "restart" {
					plantOwned(t, h, "demo", "web")
					plantSpec(t, h, "demo", "web")
					b, err := os.ReadFile(unitFile(h.root, unit))
					if err != nil {
						t.Fatal(err)
					}
					unitBefore = b
				}
				dir := unitDir(h.root)
				if kind.run {
					dir = legacyUnitDir(h.root)
				}
				path := filepath.Join(dir, unit+".d")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				switch kind.kind {
				case "dir":
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
						t.Fatal(err)
					}
				default:
					target := filepath.Join(h.base, "drop")
					if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				in := h.ref(t, "demo", "web")
				if verb == "start" {
					in = h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
				}
				raw := Handle(context.Background(), verb, in, h.sys())
				if refusalReason(t, raw) != programs.ReasonIO {
					t.Fatalf("%s", raw)
				}
				if len(h.ctl.calls) != 0 {
					t.Fatalf("calls %v", h.ctl.calls)
				}
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("drop-in was removed")
				}
				if verb == "start" {
					assertGone(t, unitFile(h.root, unit))
				} else {
					got, err := os.ReadFile(unitFile(h.root, unit))
					if err != nil || string(got) != string(unitBefore) {
						t.Fatal("unit bytes changed")
					}
				}
			})
		}
	}
	t.Run("restart upgrade run", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		plantLegacy(t, h, "demo", "web")
		run := []string{legacyUnitFile(h.root, unit), legacyEnvFile(h.root, unit), legacySpecFile(h.root, unit)}
		before := make([][]byte, len(run))
		for i, path := range run {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before[i] = b
		}
		drop := filepath.Join(unitDir(h.root), unit+".d")
		if err := os.MkdirAll(drop, 0o755); err != nil {
			t.Fatal(err)
		}
		raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
		if _, err := os.Lstat(drop); err != nil {
			t.Fatal("drop-in was removed")
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, envFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
		for i, path := range run {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(before[i]) {
				t.Fatalf("%s changed", path)
			}
		}
	})
}

func TestWriteOrder(t *testing.T) {
	unit := "deploybot-demo-web.service"
	t.Run("env fails", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		if err := os.MkdirAll(envFile(h.root, unit), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
		if fi, err := os.Lstat(envFile(h.root, unit)); err != nil || !fi.IsDir() {
			t.Fatal("env directory changed")
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
	})
	t.Run("spec fails fresh", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		if err := os.MkdirAll(specFile(h.root, unit), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, envFile(h.root, unit))
		if fi, err := os.Lstat(specFile(h.root, unit)); err != nil || !fi.IsDir() {
			t.Fatal("spec directory changed")
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
	})
	t.Run("spec fails keep unit", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		plantOwned(t, h, "demo", "web")
		before := snapFile(t, unitFile(h.root, unit))
		if err := os.MkdirAll(specFile(h.root, unit), 0o755); err != nil {
			t.Fatal(err)
		}
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		got := snapFile(t, unitFile(h.root, unit))
		if got.body != before.body {
			t.Fatal("unit was replaced")
		}
		envBody, err := os.ReadFile(envFile(h.root, unit))
		if err != nil || string(envBody) == "PORT=1\n" {
			t.Fatalf("env %q err %v", envBody, err)
		}
		if fi, err := os.Lstat(specFile(h.root, unit)); err != nil || !fi.IsDir() {
			t.Fatal("spec directory changed")
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
	})
	t.Run("unit fails fresh", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		lockDir(t, unitDir(h.root))
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		assertGone(t, unitFile(h.root, unit))
		assertGone(t, envFile(h.root, unit))
		assertGone(t, specFile(h.root, unit))
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
	})
	t.Run("unit fails keep unit", func(t *testing.T) {
		h := newHarness(t, true, false, 20)
		plantOwned(t, h, "demo", "web")
		plantSpec(t, h, "demo", "web")
		if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapFile(t, unitFile(h.root, unit))
		lockDir(t, unitDir(h.root))
		raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		got := snapFile(t, unitFile(h.root, unit))
		if got.body != before.body || !got.mod.Equal(before.mod) {
			t.Fatal("unit changed")
		}
		envBody, err := os.ReadFile(envFile(h.root, unit))
		if err != nil || string(envBody) == "PORT=1\n" {
			t.Fatalf("env %q err %v", envBody, err)
		}
		specBody, err := os.ReadFile(specFile(h.root, unit))
		if err != nil || len(specBody) == 0 {
			t.Fatal("spec was not replaced")
		}
		if len(h.ctl.calls) != 0 {
			t.Fatalf("calls %v", h.ctl.calls)
		}
	})
}

type fileSnap struct {
	body string
	mod  time.Time
}

func snapFile(t *testing.T, path string) fileSnap {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnap{body: string(b), mod: fi.ModTime()}
}

func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func rewriteHome(t *testing.T, h *harness, home string) {
	t.Helper()
	body := "root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\njoe:x:1000:1000:Joe:" + home + ":/bin/bash\n"
	if err := os.WriteFile(h.passwd, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
