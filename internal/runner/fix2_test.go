package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestStartReloadFailureRemovesFiles(t *testing.T) {
	h := newHarness(t, true, false, 20)
	h.ctl.reloadErr = true
	unit := "deploybot-demo-web.service"
	// The cleanup stop needs a loaded unit in the fake.
	h.ctl.state[unit] = "active"
	// Only the first reload fails; a failed cleanup reload is io_error (v1.5.1 rule 3).
	sys := h.sys()
	sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
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
	if _, err := os.Stat(unitFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("reload failure left the unit file")
	}
	if _, err := os.Stat(envFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("reload failure left the env file")
	}
	if _, err := os.Stat(specFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("reload failure left the spec file")
	}
	if !h.ctl.called("stop") {
		t.Fatal("reload failure did not stop the unit")
	}
}

func TestRuntimeDirsArePrivate(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	assertMode(t, joinRoot(h.root, filepath.Join("run", "bot-agent")), 0o700)
	assertMode(t, joinRoot(h.root, filepath.Join("run", "bot-agent", "env")), 0o700)
	assertMode(t, specFile(h.root, "deploybot-demo-web.service"), 0o600)
}

func TestRestartRechecksStoredSpec(t *testing.T) {
	cases := []struct {
		name  string
		shell bool
		setup func(t *testing.T, h *harness)
	}{
		{name: "exec", setup: func(t *testing.T, h *harness) {
			writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "workspace")}, []string{"/usr/bin/node"})
		}},
		{name: "dir", setup: func(t *testing.T, h *harness) {
			writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "outside")}, []string{"/usr/bin/pnpm"})
		}},
		{name: "run_as", setup: func(t *testing.T, h *harness) {
			writePolicy(t, h.base, true, false, 20, []string{"daemon"}, []string{filepath.Join(h.base, "workspace")}, []string{"/usr/bin/pnpm"})
		}},
		{name: "shell", shell: true, setup: func(t *testing.T, h *harness) {
			writePolicy(t, h.base, true, false, 20, []string{"joe"}, []string{filepath.Join(h.base, "workspace")}, []string{"/bin/sh"})
		}},
		{name: "env", setup: func(t *testing.T, h *harness) {
			path := specFile(h.root, "deploybot-demo-web.service")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			m["env"] = map[string]string{"NODE_OPTIONS": "--inspect"}
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing", setup: func(t *testing.T, h *harness) {
			if err := os.Remove(specFile(h.root, "deploybot-demo-web.service")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true, tc.shell, 20)
			body := h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil)
			if tc.shell {
				body = h.shell(t)
			}
			raw := Handle(context.Background(), "start", body, h.sys())
			if refusalReason(t, raw) != "" || !programs.StatusFresh(raw) {
				t.Fatalf("start %s", raw)
			}
			h.ctl.calls = nil
			tc.setup(t, h)
			raw = Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
			if refusalReason(t, raw) != programs.ReasonPolicy {
				t.Fatalf("%s", raw)
			}
			if h.ctl.called("restart") {
				t.Fatal("restarted a unit the current policy refuses")
			}
			if !h.ctl.called("stop") {
				t.Fatal("disallowed unit was not stopped")
			}
			if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
				t.Fatal("disallowed unit file remains")
			}
		})
	}
}
