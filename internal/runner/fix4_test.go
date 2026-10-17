package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestUpgradeRestartEnableRollbackIO(t *testing.T) {
	unit := "deploybot-demo-web.service"
	t.Run("remove", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root unlinks files in a mode 0555 directory")
		}
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
		h.ctl.enableErr = true
		t.Cleanup(func() {
			_ = os.Chmod(unitDir(h.root), 0o755)
			_ = os.Chmod(programsDir(h.root), 0o700)
		})
		sys := h.sys()
		sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
			out, err := h.ctl.systemctl(ctx, args)
			if len(args) > 0 && args[0] == "enable" {
				if chErr := os.Chmod(unitDir(h.root), 0o555); chErr != nil {
					t.Errorf("chmod unit: %v", chErr)
				}
				if chErr := os.Chmod(programsDir(h.root), 0o555); chErr != nil {
					t.Errorf("chmod programs: %v", chErr)
				}
			}
			return out, err
		}
		raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), sys)
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		want := [][]string{{"daemon-reload"}, {"enable", unit}}
		if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
			t.Fatalf("calls %v", got)
		}
		for _, path := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("removed %s: %v", path, err)
			}
		}
		for i, path := range run {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(before[i]) {
				t.Fatalf("%s changed", path)
			}
		}
	})
	t.Run("reload", func(t *testing.T) {
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
		h.ctl.enableErr = true
		reloads := 0
		gone := false
		sys := h.sys()
		etc := []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)}
		sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
			if len(args) > 0 && args[0] == "daemon-reload" {
				reloads++
				if reloads == 2 {
					gone = true
					for _, path := range etc {
						if _, err := os.Lstat(path); err == nil {
							gone = false
						}
					}
					h.ctl.reloadErr = true
				}
			}
			return h.ctl.systemctl(ctx, args)
		}
		raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), sys)
		if refusalReason(t, raw) != programs.ReasonIO {
			t.Fatalf("%s", raw)
		}
		if reloads != 2 || !gone {
			t.Fatalf("second reload reloads=%d gone=%v", reloads, gone)
		}
		for _, path := range etc {
			assertGone(t, path)
		}
		for i, path := range run {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(before[i]) {
				t.Fatalf("%s changed", path)
			}
		}
		want := [][]string{{"daemon-reload"}, {"enable", unit}, {"daemon-reload"}}
		if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
			t.Fatalf("calls %v", got)
		}
	})
}

func TestUpgradeStartEnableErrRemovesRun(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantLegacy(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	h.ctl.enableErr = true
	h.ctl.state[unit] = "active"
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if programs.StatusFresh(raw) || refusalReason(t, raw) != "" {
		t.Fatalf("%s", raw)
	}
	for _, path := range programPaths(h.root, unit) {
		assertGone(t, path)
	}
}

func TestFailedStartCleanupReloadIO(t *testing.T) {
	h := newHarness(t, true, false, 20)
	unit := "deploybot-demo-web.service"
	h.ctl.enableErr = true
	h.ctl.state[unit] = "active"
	reloads := 0
	gone := false
	sys := h.sys()
	sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
		if len(args) > 0 && args[0] == "daemon-reload" {
			reloads++
			if reloads == 2 {
				gone = true
				for _, path := range programPaths(h.root, unit) {
					if _, err := os.Lstat(path); err == nil {
						gone = false
					}
				}
				h.ctl.reloadErr = true
			}
		}
		return h.ctl.systemctl(ctx, args)
	}
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), sys)
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatalf("%s", raw)
	}
	if reloads != 2 || !gone {
		t.Fatalf("cleanup reload reloads=%d gone=%v calls %v", reloads, gone, h.ctl.calls)
	}
	want := [][]string{{"daemon-reload"}, {"enable", unit}, {"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
}

func TestStopLeftoverSpecUnlinkIO(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks files in a mode 0555 directory")
	}
	h := newHarness(t, true, false, 20)
	plantSpec(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	dir := programsDir(h.root)
	spec := specFile(h.root, unit)
	if err := os.MkdirAll(filepath.Join(h.root, "run", "bot-agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	// ensureRuntime chmods this directory back to 0700 before stop. Hold the
	// helper lock so the request blocks after that chmod, put 0555 back, then
	// let the leftover-spec stop run.
	release, err := lockHelper(h.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		release()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	done := make(chan []byte, 1)
	go func() {
		done <- Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fi, statErr := os.Lstat(dir)
		if statErr == nil && fi.Mode().Perm() == 0o700 {
			break
		}
		select {
		case raw := <-done:
			release()
			t.Fatalf("stop finished before programs mode changed: %s", raw)
		default:
		}
		if time.Now().After(deadline) {
			release()
			<-done
			t.Fatal("timed out waiting for ensureRuntime")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		release()
		<-done
		t.Fatal(err)
	}
	release()
	raw := <-done
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatalf("%s", raw)
	}
	if _, err := os.Lstat(spec); err != nil {
		t.Fatal(err)
	}
	if len(h.ctl.calls) != 0 {
		t.Fatalf("calls %v", h.ctl.calls)
	}
}

func TestPolicyDeniedRestartDisableErr(t *testing.T) {
	h := newHarness(t, true, false, 20)
	plantOwned(t, h, "demo", "web")
	plantSpec(t, h, "demo", "web")
	unit := "deploybot-demo-web.service"
	if err := os.WriteFile(envFile(h.root, unit), []byte("PORT=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "workspace")}, []string{"/usr/bin/node"})
	h.ctl.disableErr = true
	h.ctl.state[unit] = "active"
	var logs []string
	sys := h.sys()
	sys.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), sys)
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("%s", raw)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], programs.ReasonPolicy) {
		t.Fatalf("logs %v", logs)
	}
	want := [][]string{{"disable", unit}, {"stop", unit}, {"daemon-reload"}}
	if got := systemctlVerbs(h.ctl); !callsEqual(got, want) {
		t.Fatalf("calls %v", got)
	}
	assertGone(t, unitFile(h.root, unit))
	assertGone(t, envFile(h.root, unit))
	assertGone(t, specFile(h.root, unit))
}
