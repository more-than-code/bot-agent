package packaging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v1.20 item 8: an enrolled upgrade passes only after the restarted agent
// touched its hello marker. The reference file does not stay behind.
func TestInstallerHelloMarkerPresent(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "upgrade")
	}
	assertHasLines(t, res, "upgrade", "verify: hello acknowledged", "verify: active, NRestarts 0, version 0.2.1")
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version after upgrade")
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "var/lib/bot-agent-installer/hello-ref")); !os.IsNotExist(err) {
		t.Fatal("hello-ref left behind")
	}
}

// A marker older than the restart (the previous agent's) does not count.
func TestInstallerHelloMarkerStaleRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	mk := filepath.Join(rg.root, "var/lib/bot-agent/last_hello")
	if err := os.MkdirAll(filepath.Dir(mk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The stub would remove the marker; keep it, but old, by making the
	// stub skip this one restart without touching the file.
	stub := filepath.Join(rg.stubs, "systemctl")
	body, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte(`rm -f "$BOT_AGENT_STUB_DIR/no-hello-once" "$_mk"`), []byte(`rm -f "$BOT_AGENT_STUB_DIR/no-hello-once"`), 1)
	writeExe(t, stub, string(body))
	if err := os.WriteFile(filepath.Join(rg.stubs, "no-hello-once"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 {
		res.fail(t, "", "stale marker accepted")
	}
	assertHasLines(t, res, "stale marker", "verify: no hello after restart", "upgrade failed, restored 0.2.0")
}

// No marker after the restart: verify fails and the existing rollback
// runs. The rollback's verify keeps its old checks and does not wait for
// the marker.
func TestInstallerHelloMarkerMissingRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "no-hello-once"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 {
		res.fail(t, "", "missing marker accepted")
	}
	assertHasLines(t, res, "missing marker",
		"verify: no hello after restart", "verify failed, rolling back",
		"verify: active, NRestarts 0, version 0.2.0", "upgrade failed, restored 0.2.0")
	if strings.Contains(res.combined(), "verify: hello acknowledged") || strings.Count(res.combined(), "verify: no hello after restart") != 1 {
		res.fail(t, "", "the rollback waited for the marker")
	}
	if n := strings.Count(res.log, "systemctl restart bot-agent.service"); n != 2 {
		res.fail(t, "", "restarts: want 2 (upgrade and rollback)")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("version was not restored")
	}
	if _, err := os.Stat(filepath.Join(rg.root, "usr/local/bin/bot-agent.prev")); !os.IsNotExist(err) {
		t.Fatal(".prev remained after a verified rollback")
	}
}

// The restored agent cannot reconnect either (an agent before 0.2.15, or
// the server is down): the rollback still verifies on its old checks.
func TestInstallerHelloMarkerNeverRollbackStillVerifies(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "no-hello"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 1 {
		res.fail(t, "", "exit code")
	}
	assertHasLines(t, res, "never", "verify: no hello after restart", "upgrade failed, restored 0.2.0")
	if strings.Contains(res.combined(), "rollback verify failed") {
		res.fail(t, "", "rollback verify waited for the marker")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("files were not restored")
	}
}

// An installer rollback (break-glass) does not wait for the marker.
func TestInstallerRollbackDoesNotWaitForHello(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if up := runBundle(t, next, rg, "", "upgrade"); up.code != 0 {
		up.fail(t, "", "upgrade")
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "no-hello"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	back := runBundle(t, next, rg, "", "rollback")
	if back.code != 0 {
		back.fail(t, "", "rollback")
	}
	assertHasLines(t, back, "rollback", "verify: active, NRestarts 0, version 0.2.0", "summary: rolled back 0.2.0")
	if strings.Contains(back.combined(), "hello") {
		back.fail(t, "", "rollback checked the marker")
	}
	dry := runBundle(t, next, rg, "", "rollback", "--dry-run")
	if strings.Contains(dry.combined(), "last_hello") {
		dry.fail(t, "", "rollback dry-run names the marker wait")
	}
}

// writeEnvLines replaces the planted env file with these lines.
func writeEnvLines(t *testing.T, rg *rig, body string) {
	t.Helper()
	path := filepath.Join(rg.root, "etc/bot-agent/env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// BOT_AGENT_STATE_DIR in the env file moves the marker; the installer
// looks there (quoted or not, the last line wins) and not at the default.
func TestInstallerHelloMarkerStateDirFromEnv(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env     string
		stubDir string
		ok      bool
		line    string
	}{
		"plain":       {"BOT_AGENT_STATE_DIR=/srv/agent-state\n", "/srv/agent-state", true, "verify: hello acknowledged"},
		"quoted":      {"export BOT_AGENT_STATE_DIR = \"/srv/agent-state/\"\n", "/srv/agent-state", true, "verify: hello acknowledged"},
		"last wins":   {"BOT_AGENT_STATE_DIR=/srv/old\nBOT_AGENT_STATE_DIR=/srv/agent-state\n", "/srv/agent-state", true, "verify: hello acknowledged"},
		"empty":       {"BOT_AGENT_STATE_DIR=\n", "/var/lib/bot-agent", true, "verify: hello acknowledged"},
		"not default": {"BOT_AGENT_STATE_DIR=/srv/agent-state\n", "/var/lib/bot-agent", false, "verify: no hello after restart"},
		"relative":    {"BOT_AGENT_STATE_DIR=state\n", "/var/lib/bot-agent", false, "BOT_AGENT_STATE_DIR in the env file is not a plain absolute path"},
		"dotdot":      {"BOT_AGENT_STATE_DIR=/srv/../etc\n", "/var/lib/bot-agent", false, "BOT_AGENT_STATE_DIR in the env file is not a plain absolute path"},
		"command":     {"BOT_AGENT_STATE_DIR=$(touch /tmp/x)\n", "/var/lib/bot-agent", false, "BOT_AGENT_STATE_DIR in the env file is not a plain absolute path"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rg := newRig(t)
			old := makeBundle(t, "0.2.0")
			next := makeBundle(t, "0.2.1")
			mustInstall(t, old, rg, writeOKConfig(t))
			plantEnv(t, rg)
			writeEnvLines(t, rg, "BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\n"+tc.env)
			if err := os.WriteFile(filepath.Join(rg.stubs, "hello-dir"), []byte(tc.stubDir), 0o644); err != nil {
				t.Fatal(err)
			}
			res := runBundle(t, next, rg, "", "upgrade")
			if (res.code == 0) != tc.ok {
				res.fail(t, "", "exit code")
			}
			assertHasLines(t, res, name, tc.line)
			if !tc.ok {
				assertHasLines(t, res, name, "upgrade failed, restored 0.2.0")
			}
		})
	}
}

// The dry-run plan names the wait.
func TestInstallerDryRunNamesHelloWait(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	res := runBundle(t, next, rg, "", "upgrade", "--dry-run")
	assertDryRun(t, res, 0, "upgrade dry-run")
	assertHasLines(t, res, "upgrade dry-run", "dry-run: wait up to 0s for the agent's last_hello to be newer than the restart")
}

// release-sign is a release tool: no bundle file, unit or installer step
// names it, and install.sh picks only the agent and runner binaries.
func TestReleaseSignNotShipped(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("release-sign")) || bytes.Contains(b, []byte("release_sign")) {
			t.Errorf("%s names release-sign", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, glob := range []string{`"$BUNDLE"/bot-agent-v*-linux-"$ARCH"`, `"$BUNDLE"/bot-agent-runner-v*-linux-"$ARCH"`} {
		if !bytes.Contains(inst, []byte(glob)) {
			t.Fatalf("install.sh no longer picks binaries with %s", glob)
		}
	}
}

// A last_hello that is a symlink does not count, even when its target is
// fresh.
func TestInstallerHelloMarkerSymlinkRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	dir := filepath.Join(rg.root, "var/lib/bot-agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "elsewhere"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, "last_hello")); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 {
		res.fail(t, "", "symlinked marker accepted")
	}
	assertHasLines(t, res, "symlink", "verify: no hello after restart", "upgrade failed, restored 0.2.0")
}
