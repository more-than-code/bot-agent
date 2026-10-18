package packaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realAgent is a bot-agent built once for policy check and init.
// The bundle "binaries" are shell scripts so version is a fixed string.
// TestMain writes it before any test runs. Tests only read it.
var realAgent string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bot-agent-installer-real-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "bot-agent")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/bot-agent")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go build ./cmd/bot-agent: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	realAgent = bin
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type rig struct {
	root     string
	stubs    string
	log      string
	noDocker bool
}

type cmdResult struct {
	stdout string
	stderr string
	log    string
	code   int
}

func (r cmdResult) combined() string {
	return r.stdout + "\n" + r.stderr
}

func (r cmdResult) fail(t *testing.T, token, msg string) {
	t.Helper()
	red := func(s string) string {
		if token == "" {
			return s
		}
		return strings.ReplaceAll(s, token, "[redacted]")
	}
	t.Fatalf("%s\ncode=%d\nstdout:\n%s\nstderr:\n%s\nlog:\n%s", msg, r.code, red(r.stdout), red(r.stderr), red(r.log))
}

func bundleArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		t.Fatalf("unsupported uname -m %q", strings.TrimSpace(string(out)))
	}
	return ""
}

func newRig(t *testing.T) *rig {
	t.Helper()
	rg := &rig{
		root:  t.TempDir(),
		stubs: t.TempDir(),
		log:   filepath.Join(t.TempDir(), "calls.log"),
	}
	writeStubs(t, rg.stubs)
	return rg
}

func (rg *rig) env() []string {
	path := os.Getenv("PATH")
	if rg.noDocker {
		// Drop the stub and any host docker so `command -v docker` fails.
		_ = os.Remove(filepath.Join(rg.stubs, "docker"))
		var kept []string
		for _, dir := range filepath.SplitList(path) {
			if dir == "" {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, "docker")); err == nil {
				continue
			}
			kept = append(kept, dir)
		}
		path = strings.Join(kept, string(os.PathListSeparator))
	}
	return append(os.Environ(),
		"BOT_AGENT_INSTALLER_TEST=1",
		"BOT_AGENT_INSTALLER_SETTLE=0",
		"BOT_AGENT_STUB_LOG="+rg.log,
		"BOT_AGENT_STUB_DIR="+rg.stubs,
		"PATH="+rg.stubs+string(os.PathListSeparator)+path,
	)
}

func (rg *rig) truncateLog(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(rg.log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runBundle(t *testing.T, bundle string, rg *rig, stdin string, args ...string) cmdResult {
	t.Helper()
	if len(args) == 0 {
		t.Fatal("missing subcommand")
	}
	full := append([]string{args[0], "--root", rg.root}, args[1:]...)
	// A lone install is about 8s. With every installer test in parallel the
	// same run exceeds 30s because each external command is spawn-bound.
	// This deadline only reports a stuck process; it is not an assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(bundle, "install.sh"), full...)
	cmd.Env = rg.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("installer hung: %s\nstdout:\n%s\nstderr:\n%s", strings.Join(full, " "), stdout.String(), stderr.String())
		}
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			t.Fatal(runErr)
		}
		code = ee.ExitCode()
	}
	logb, err := os.ReadFile(rg.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return cmdResult{stdout: stdout.String(), stderr: stderr.String(), log: string(logb), code: code}
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func shSingle(t *testing.T, s string) string {
	t.Helper()
	if strings.Contains(s, "'") {
		t.Fatalf("cannot quote %q", s)
	}
	return "'" + s + "'"
}

func makeBundle(t *testing.T, version string) string {
	t.Helper()
	if version == "" || strings.ContainsAny(version, " \t'\"$`;|&") {
		t.Fatalf("bad version %q", version)
	}
	dir := t.TempDir()
	arch := bundleArch(t)
	agent := "#!/bin/sh\nset -eu\ncmd=${1:-}\n" +
		"if [ \"$cmd\" = \"version\" ] || [ \"$cmd\" = \"--version\" ] || [ \"$cmd\" = \"-version\" ]; then\n" +
		"  printf '%s\\n' \"" + version + "\"\n  exit 0\nfi\n" +
		"if [ \"$cmd\" = \"policy\" ] || [ \"$cmd\" = \"init\" ]; then\n" +
		"  exec " + shSingle(t, realAgent) + " \"$@\"\nfi\n" +
		"printf '%s\\n' \"unexpected\" >&2\nexit 1\n"
	writeExe(t, filepath.Join(dir, "bot-agent-v"+version+"-linux-"+arch), agent)
	runner := "#!/bin/sh\nset -eu\nif [ \"${1:-}\" = \"version\" ] || [ \"${1:-}\" = \"--version\" ] || [ \"${1:-}\" = \"-version\" ]; then\n" +
		"  printf '%s\\n' \"" + version + "\"\n  exit 0\nfi\nexit 0\n"
	writeExe(t, filepath.Join(dir, "bot-agent-runner-v"+version+"-linux-"+arch), runner)
	unitDir := filepath.Join(dir, "units")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyMode(t, "bot-agent.service", filepath.Join(unitDir, "bot-agent.service"), 0o644)
	copyMode(t, "bot-agent-runner.socket", filepath.Join(unitDir, "bot-agent-runner.socket"), 0o644)
	copyMode(t, "bot-agent-runner@.service", filepath.Join(unitDir, "bot-agent-runner@.service"), 0o644)
	copyMode(t, filepath.Join("tmpfiles.d", "bot-agent-runner.conf"), filepath.Join(unitDir, "tmpfiles-bot-agent-runner.conf"), 0o644)
	copyMode(t, "install.sh", filepath.Join(dir, "install.sh"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "RELEASE.md"), []byte("test bundle "+version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSums(t, dir)
	return dir
}

func copyMode(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatal(err)
	}
}

func writeSums(t *testing.T, dir string) {
	t.Helper()
	var rels []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "SHA256SUMS" {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rels)
	var b strings.Builder
	for _, rel := range rels {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), rel)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStubs(t *testing.T, dir string) {
	t.Helper()
	writeExe(t, filepath.Join(dir, "systemctl"), `#!/bin/sh
set -eu
printf '%s\n' "systemctl $*" >> "$BOT_AGENT_STUB_LOG"
for a in "$@"; do
  if [ "$a" = "--version" ]; then
    printf '%s\n' "systemd 252 (252.26-1~deb12u1)"
    exit 0
  fi
done
if [ "${1:-}" = "is-active" ]; then
  if [ -f "$BOT_AGENT_STUB_DIR/fail-active" ]; then
    rm -f "$BOT_AGENT_STUB_DIR/fail-active"
    printf '%s\n' "failed"
    exit 3
  fi
  if [ -f "$BOT_AGENT_STUB_DIR/inactive-until-restart" ]; then
    if ! grep -q 'systemctl restart ' "$BOT_AGENT_STUB_LOG"; then
      printf '%s\n' "inactive"
      exit 3
    fi
  fi
  printf '%s\n' "active"
  exit 0
fi
if [ "${1:-}" = "show" ]; then
  if [ -f "$BOT_AGENT_STUB_DIR/fail-restarts" ]; then
    rm -f "$BOT_AGENT_STUB_DIR/fail-restarts"
    printf '%s\n' "3"
    exit 0
  fi
  if [ -f "$BOT_AGENT_STUB_DIR/empty-restarts" ]; then
    rm -f "$BOT_AGENT_STUB_DIR/empty-restarts"
    exit 0
  fi
  printf '%s\n' "0"
  exit 0
fi
if [ "${1:-}" = "list-units" ]; then
  if [ -f "$BOT_AGENT_STUB_DIR/runner-instances" ]; then
    printf '%s\n' "bot-agent-runner@demo.service loaded active running"
  fi
  exit 0
fi
if [ -f "$BOT_AGENT_STUB_DIR/fail-stop" ]; then
  case "${1:-}" in
    stop|disable)
      printf '%s\n' "stop failed" >&2
      exit 1
      ;;
  esac
fi
if [ -f "$BOT_AGENT_STUB_DIR/stop-nomatch" ] && [ "${1:-}" = "stop" ]; then
  printf '%s\n' "stop failed" >&2
  exit 1
fi
exit 0
`)
	writeExe(t, filepath.Join(dir, "id"), `#!/bin/sh
set -eu
printf '%s\n' "id $*" >> "$BOT_AGENT_STUB_LOG"
if [ "${1:-}" = "-u" ]; then
  printf '%s\n' "0"
  exit 0
fi
name=
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) name=$a ;;
  esac
done
if [ -n "$name" ] && [ -f "$BOT_AGENT_STUB_DIR/user-$name" ]; then
  exit 0
fi
exit 1
`)
	writeExe(t, filepath.Join(dir, "getent"), `#!/bin/sh
set -eu
printf '%s\n' "getent $*" >> "$BOT_AGENT_STUB_LOG"
if [ "${1:-}" = "group" ] && [ -n "${2:-}" ] && [ -f "$BOT_AGENT_STUB_DIR/group-$2" ]; then
  printf '%s\n' "$2:x:900:"
  exit 0
fi
exit 2
`)
	writeExe(t, filepath.Join(dir, "groupadd"), `#!/bin/sh
set -eu
printf '%s\n' "groupadd $*" >> "$BOT_AGENT_STUB_LOG"
name=
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) name=$a ;;
  esac
done
if [ -z "$name" ]; then
  exit 1
fi
: > "$BOT_AGENT_STUB_DIR/group-$name"
if [ -f "$BOT_AGENT_STUB_DIR/rewrite-runner-config" ]; then
  _rw=$(cat "$BOT_AGENT_STUB_DIR/rewrite-runner-config")
  if [ -n "$_rw" ]; then
    printf '%s\n' "enabled = true" > "$_rw"
  fi
fi
exit 0
`)
	writeExe(t, filepath.Join(dir, "useradd"), `#!/bin/sh
set -eu
printf '%s\n' "useradd $*" >> "$BOT_AGENT_STUB_LOG"
name=
skip=0
for a in "$@"; do
  if [ "$skip" = "1" ]; then
    skip=0
    continue
  fi
  case "$a" in
    --gid|--shell|--comment|--home|--uid|--groups|-g|-s|-c|-d|-u|-G) skip=1 ;;
    --*) ;;
    -*) ;;
    *) name=$a ;;
  esac
done
if [ -z "$name" ]; then
  exit 1
fi
: > "$BOT_AGENT_STUB_DIR/user-$name"
exit 0
`)
	writeExe(t, filepath.Join(dir, "userdel"), `#!/bin/sh
set -eu
printf '%s\n' "userdel $*" >> "$BOT_AGENT_STUB_LOG"
name=
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) name=$a ;;
  esac
done
if [ -z "$name" ]; then
  exit 1
fi
rm -f "$BOT_AGENT_STUB_DIR/user-$name"
exit 0
`)
	writeExe(t, filepath.Join(dir, "chown"), `#!/bin/sh
set -eu
printf '%s\n' "chown $*" >> "$BOT_AGENT_STUB_LOG"
exit 0
`)
	writeExe(t, filepath.Join(dir, "systemd-tmpfiles"), `#!/bin/sh
set -eu
printf '%s\n' "systemd-tmpfiles $*" >> "$BOT_AGENT_STUB_LOG"
exit 0
`)
	writeExe(t, filepath.Join(dir, "docker"), `#!/bin/sh
set -eu
printf '%s\n' "docker $*" >> "$BOT_AGENT_STUB_LOG"
labelled=0
for a in "$@"; do
  case "$a" in
    *label=dev.deploybot.owner*) labelled=1 ;;
  esac
done
show() {
  if [ "$labelled" = "1" ]; then
    cat "$1" 2>/dev/null || true
  else
    cat "$1" "$2" 2>/dev/null || true
  fi
}
if [ "${1:-}" = "version" ]; then
  exit 0
fi
if [ "${1:-}" = "ps" ]; then
  show "$BOT_AGENT_STUB_DIR/containers-owned" "$BOT_AGENT_STUB_DIR/containers-other"
  exit 0
fi
if [ "${1:-}" = "volume" ] && [ "${2:-}" = "ls" ]; then
  show "$BOT_AGENT_STUB_DIR/volumes-owned" "$BOT_AGENT_STUB_DIR/volumes-other"
  exit 0
fi
if [ "${1:-}" = "network" ] && [ "${2:-}" = "ls" ]; then
  show "$BOT_AGENT_STUB_DIR/networks-owned" "$BOT_AGENT_STUB_DIR/networks-other"
  exit 0
fi
exit 0
`)
}

func treeSig(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		extra := ""
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			extra = hex.EncodeToString(sum[:])
		}
		lines = append(lines, fmt.Sprintf("%s %o %s", filepath.ToSlash(rel), info.Mode(), extra))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s mode %o, want %o", path, fi.Mode().Perm(), want)
	}
}

func binVersion(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s version: %v\n%s", path, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeOKConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ok.toml")
	if err := os.WriteFile(path, []byte("enabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func plantEnv(t *testing.T, rg *rig) {
	t.Helper()
	dir := filepath.Join(rg.root, "etc/bot-agent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat type %T", fi.Sys())
	}
	return uint64(st.Ino)
}

func markBundleUnit(t *testing.T, bundle, marker string) {
	t.Helper()
	p := filepath.Join(bundle, "units", "bot-agent.service")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# " + marker + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	writeSums(t, bundle)
}

func mustInstall(t *testing.T, bundle string, rg *rig, cfg string) cmdResult {
	t.Helper()
	res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
	if res.code != 0 {
		res.fail(t, "", "install failed")
	}
	return res
}

func TestInstallerAC1InstallAndRerun(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	cfg := writeOKConfig(t)
	res := mustInstall(t, bundle, rg, cfg)
	root := rg.root
	assertPerm(t, filepath.Join(root, "usr/local/bin/bot-agent"), 0o755)
	assertPerm(t, filepath.Join(root, "usr/local/libexec/bot-agent-runner"), 0o755)
	assertPerm(t, filepath.Join(root, "etc/systemd/system/bot-agent.service"), 0o644)
	assertPerm(t, filepath.Join(root, "etc/systemd/system/bot-agent-runner.socket"), 0o644)
	assertPerm(t, filepath.Join(root, "etc/systemd/system/bot-agent-runner@.service"), 0o644)
	assertPerm(t, filepath.Join(root, "etc/tmpfiles.d/bot-agent-runner.conf"), 0o644)
	assertPerm(t, filepath.Join(root, "etc/bot-agent/runner.toml"), 0o644)
	assertPerm(t, filepath.Join(root, "etc/bot-agent"), 0o750)
	assertPerm(t, filepath.Join(root, "var/lib/bot-agent-installer"), 0o700)
	rec, err := os.ReadFile(filepath.Join(root, "var/lib/bot-agent-installer/installed"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec), "version 0.2.0\n") {
		t.Fatalf("record:\n%s", rec)
	}
	if !strings.Contains(res.log, "chown root:bot-agent ") || !strings.Contains(res.log, "chown root:root ") {
		res.fail(t, "", "chown log")
	}
	if !strings.Contains(res.log, "systemctl daemon-reload") || strings.Contains(res.log, "systemctl enable") {
		res.fail(t, "", "systemctl log")
	}
	if !strings.Contains(res.log, "useradd ") || !strings.Contains(res.log, "--system") || !strings.Contains(res.log, "--no-create-home") || !strings.Contains(res.log, "nologin") {
		res.fail(t, "", "useradd log")
	}
	if !strings.Contains(res.log, "systemd-tmpfiles --create ") || !strings.Contains(res.log, "bot-agent-runner.conf") {
		res.fail(t, "", "tmpfiles log")
	}
	if binVersion(t, filepath.Join(root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("installed version")
	}
	rg.truncateLog(t)
	again := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
	if again.code != 0 || !strings.Contains(again.stdout, "already installed, nothing changed") {
		again.fail(t, "", "re-run")
	}
	if strings.Contains(again.log, "daemon-reload") {
		again.fail(t, "", "re-run reloaded")
	}
}

func TestInstallerKeepsPolicyAndEnv(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	etc := filepath.Join(rg.root, "etc/bot-agent")
	if err := os.MkdirAll(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	policy := []byte("# marker-keep-me\nenabled = false\n")
	envBody := []byte("SECRET=keep-me-env\n")
	if err := os.WriteFile(filepath.Join(etc, "runner.toml"), policy, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(etc, "runner.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "env"), envBody, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeOKConfig(t)
	res := mustInstall(t, bundle, rg, cfg)
	if !strings.Contains(res.stdout, "kept:") {
		res.fail(t, "", "did not say kept")
	}
	gotPolicy, err := os.ReadFile(filepath.Join(etc, "runner.toml"))
	if err != nil {
		t.Fatal(err)
	}
	gotEnv, err := os.ReadFile(filepath.Join(etc, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPolicy, policy) || !bytes.Equal(gotEnv, envBody) {
		t.Fatal("installer replaced runner.toml or env")
	}
}

func TestInstallerAC2BadPolicy(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	before := treeSig(t, rg.root)
	cfg := filepath.Join(t.TempDir(), "bad.toml")
	body := "enabled = true\nimages = [\n  \"ghcr.io/acme/*\"\n]\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
	if res.code == 0 {
		res.fail(t, "", "bad policy was accepted")
	}
	if !strings.Contains(res.combined(), "invalid: parse") || !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", "bad policy output")
	}
	if strings.Contains(res.log, "useradd ") {
		res.fail(t, "", "useradd ran")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("fake root changed")
	}
}

func TestInstallerAC4UpgradeAndPrev(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	cfg := writeOKConfig(t)
	mustInstall(t, old, rg, cfg)
	plantEnv(t, rg)
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "upgrade")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version after upgrade")
	}
	prev := filepath.Join(rg.root, "usr/local/bin/bot-agent.prev")
	if binVersion(t, prev) != "0.2.0" {
		t.Fatal("prev version")
	}
	if !strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, "", "agent was not restarted")
	}
}

func TestInstallerAC4ModifiedUnit(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	cfg := writeOKConfig(t)
	mustInstall(t, old, rg, cfg)
	unit := filepath.Join(rg.root, "etc/systemd/system/bot-agent.service")
	f, err := os.OpenFile(unit, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# local\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 1 || !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", "modified unit")
	}
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "# local") {
		t.Fatal("unit was replaced")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("binary changed")
	}
	if strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, "", "restarted despite refusal")
	}
}

func TestInstallerAC4AutoRollback(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	cfg := writeOKConfig(t)
	mustInstall(t, old, rg, cfg)
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-restarts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 {
		res.fail(t, "", "failing verify was accepted")
	}
	if !strings.Contains(res.combined(), "rolling back") && !strings.Contains(res.combined(), "restored") {
		res.fail(t, "", "no rollback")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("version was not restored")
	}
}

func TestInstallerAC5Rollback(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	cfg := writeOKConfig(t)
	mustInstall(t, old, rg, cfg)
	up := runBundle(t, next, rg, "", "upgrade")
	if up.code != 0 {
		up.fail(t, "", "upgrade")
	}
	back := runBundle(t, next, rg, "", "rollback")
	if back.code != 0 {
		back.fail(t, "", "rollback")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("rollback version")
	}
	if _, err := os.Stat(filepath.Join(rg.root, "usr/local/bin/bot-agent.prev")); !os.IsNotExist(err) {
		t.Fatal(".prev still present")
	}
	again := runBundle(t, next, rg, "", "rollback")
	if again.code != 0 || !strings.Contains(again.stdout, "nothing to roll back") {
		again.fail(t, "", "second rollback")
	}
}

func plantWorkloads(t *testing.T, rg *rig) {
	t.Helper()
	etc := filepath.Join(rg.root, "etc/systemd/system")
	runDir := filepath.Join(rg.root, "run/systemd/system")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	web := "[Service]\nX-Deploybot-Project=demo\nX-Deploybot-Entry=web\nExecStart=/bin/true\n"
	job := "[Service]\nX-Deploybot-Project=demo\nX-Deploybot-Entry=job\nExecStart=/bin/true\n"
	if err := os.WriteFile(filepath.Join(etc, "deploybot-demo-web.service"), []byte(web), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "deploybot-demo-job.service"), []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "deploybot-foreign.service"), []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "nginx.service"), []byte("[Service]\nExecStart=/usr/sbin/nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "deploybot-link.service")
	_ = os.Remove(link)
	if err := os.Symlink("nginx.service", link); err != nil {
		t.Fatal(err)
	}
	keepDir := filepath.Join(rg.root, "var/lib/bot-agent-runner")
	if err := os.MkdirAll(keepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "keep"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rg.root, "etc/bot-agent"), 0o750); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		if err := os.WriteFile(envPath, []byte("SECRET=keep-me-env\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pairs := []struct{ name, body string }{
		{"containers-owned", "id-owned name-owned\n"},
		{"containers-other", "id-other name-other\n"},
		{"volumes-owned", "vol-owned\n"},
		{"volumes-other", "vol-other\n"},
		{"networks-owned", "net-owned\n"},
		{"networks-other", "net-other\n"},
	}
	for _, p := range pairs {
		if err := os.WriteFile(filepath.Join(rg.stubs, p.name), []byte(p.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("still present: %s (%v)", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("missing %s: %v", path, err)
	}
}

func TestInstallerAC6RemoveDefault(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	plantWorkloads(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, "", "remove")
	if res.code != 0 {
		res.fail(t, "", "remove")
	}
	assertGone(t, filepath.Join(rg.root, "usr/local/bin/bot-agent"))
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent.service"))
	assertGone(t, filepath.Join(rg.root, "var/lib/bot-agent-installer"))
	assertExists(t, filepath.Join(rg.root, "etc/bot-agent/env"))
	assertExists(t, filepath.Join(rg.root, "var/lib/bot-agent-runner/keep"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-demo-web.service"))
	assertExists(t, filepath.Join(rg.root, "run/systemd/system/deploybot-demo-job.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-foreign.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/nginx.service"))
	fi, err := os.Lstat(filepath.Join(rg.root, "etc/systemd/system/deploybot-link.service"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was removed")
	}
	out := res.stdout
	for _, needle := range []string{"left in place", "deploybot-demo-web", "id-owned", "revoke the host in the console"} {
		if !strings.Contains(out, needle) {
			res.fail(t, "", "missing "+needle)
		}
	}
	for _, bad := range []string{"docker rm", "docker stop", "volume rm", "network rm", "id-other", "userdel "} {
		if strings.Contains(res.log, bad) {
			res.fail(t, "", "log has "+bad)
		}
	}
	if !strings.Contains(res.log, "systemctl stop bot-agent-runner@*.service") {
		res.fail(t, "", "runner instances were not stopped")
	}
	for _, line := range strings.Split(res.log, "\n") {
		if !strings.Contains(line, "deploybot-") {
			continue
		}
		if strings.Contains(line, "systemctl stop") || strings.Contains(line, "systemctl disable") {
			res.fail(t, "", "stopped deploybot: "+line)
		}
	}
}

func TestInstallerAC6RemoveWorkloads(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	plantWorkloads(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, "", "remove", "--remove-workloads")
	if res.code != 0 {
		res.fail(t, "", "remove --remove-workloads")
	}
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-demo-web.service"))
	assertGone(t, filepath.Join(rg.root, "run/systemd/system/deploybot-demo-job.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-foreign.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/nginx.service"))
	assertExists(t, filepath.Join(rg.root, "etc/bot-agent"))
	assertExists(t, filepath.Join(rg.root, "var/lib/bot-agent-runner/keep"))
	if !strings.Contains(res.log, "docker rm -f id-owned") || strings.Contains(res.log, "id-other") {
		res.fail(t, "", "container selection")
	}
	if strings.Contains(res.log, "volume rm") {
		res.fail(t, "", "volume removed without --purge")
	}
	if !strings.Contains(res.log, "docker network rm net-owned") || strings.Contains(res.log, "net-other") {
		res.fail(t, "", "network selection")
	}
}

func TestInstallerAC6Purge(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	plantWorkloads(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, "", "remove", "--purge")
	if res.code != 0 {
		res.fail(t, "", "purge")
	}
	assertGone(t, filepath.Join(rg.root, "etc/bot-agent"))
	assertGone(t, filepath.Join(rg.root, "var/lib/bot-agent-runner"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-demo-web.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/nginx.service"))
	if !strings.Contains(res.log, "userdel ") {
		res.fail(t, "", "userdel")
	}
	if strings.Contains(res.log, "docker rm") || strings.Contains(res.log, "volume rm") || strings.Contains(res.log, "network rm") {
		res.fail(t, "", "purge removed workloads")
	}
	if !strings.Contains(res.stdout, "revoke the host in the console") {
		res.fail(t, "", "no revoke reminder")
	}
}

func TestInstallerAC6PurgeWorkloads(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	plantWorkloads(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, "", "remove", "--purge", "--remove-workloads")
	if res.code != 0 {
		res.fail(t, "", "purge workloads")
	}
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-demo-web.service"))
	assertGone(t, filepath.Join(rg.root, "etc/bot-agent"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/nginx.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-foreign.service"))
	for _, needle := range []string{"docker rm -f id-owned", "docker volume rm vol-owned", "docker network rm net-owned"} {
		if !strings.Contains(res.log, needle) {
			res.fail(t, "", "missing "+needle)
		}
	}
	for _, bad := range []string{"id-other", "vol-other", "net-other"} {
		if strings.Contains(res.log, bad) {
			res.fail(t, "", "removed "+bad)
		}
	}
}

func TestInstallerAC7DryRun(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	cfg := writeOKConfig(t)
	before := treeSig(t, rg.root)
	cmds := []struct {
		bundle string
		args   []string
	}{
		{bundle, []string{"check", "--dry-run"}},
		{bundle, []string{"check", "--dry-run", "--runner"}},
		{bundle, []string{"install", "--dry-run", "--runner", "--runner-config", cfg}},
		{bundle, []string{"enroll", "--dry-run", "--server", "wss://example.invalid/agent/v1/ws"}},
		{bundle, []string{"upgrade", "--dry-run"}},
		{bundle, []string{"rollback", "--dry-run"}},
		{bundle, []string{"remove", "--dry-run"}},
		{bundle, []string{"remove", "--dry-run", "--purge", "--remove-workloads"}},
	}
	for _, c := range cmds {
		sig := treeSig(t, rg.root)
		res := runBundle(t, c.bundle, rg, "", c.args...)
		if treeSig(t, rg.root) != sig || treeSig(t, rg.root) != before {
			res.fail(t, "", "dry-run changed empty root: "+strings.Join(c.args, " "))
		}
	}
	installed := mustInstall(t, bundle, rg, cfg)
	_ = installed
	after := treeSig(t, rg.root)
	live := []struct {
		bundle string
		args   []string
	}{
		{bundle, []string{"check", "--dry-run", "--runner"}},
		{bundle, []string{"install", "--dry-run", "--runner", "--runner-config", cfg}},
		{bundle, []string{"enroll", "--dry-run", "--server", "wss://example.invalid/agent/v1/ws"}},
		{next, []string{"upgrade", "--dry-run"}},
		{bundle, []string{"rollback", "--dry-run"}},
		{bundle, []string{"remove", "--dry-run", "--purge", "--remove-workloads"}},
	}
	for _, c := range live {
		res := runBundle(t, c.bundle, rg, "", c.args...)
		if treeSig(t, rg.root) != after {
			res.fail(t, "", "dry-run changed installed root: "+strings.Join(c.args, " "))
		}
	}
}

func TestInstallerAC8BadSHA256(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	sums := filepath.Join(bundle, "SHA256SUMS")
	body, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 64 {
		t.Fatal("short sums")
	}
	if body[0] == 'a' {
		body[0] = 'b'
	} else {
		body[0] = 'a'
	}
	if err := os.WriteFile(sums, body, 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, rg.root)
	res := runBundle(t, bundle, rg, "", "install", "--runner")
	if res.code != 1 || !strings.Contains(res.combined(), "sha256 mismatch") || !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", "bad sha256")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("fake root changed")
	}
}

func TestInstallerEnrollTokenHidden(t *testing.T) {
	t.Parallel()
	const token = "EnrollTokenValue1234"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	if res.code != 0 {
		res.fail(t, token, "enroll")
	}
	if strings.Contains(res.stdout, token) || strings.Contains(res.stderr, token) || strings.Contains(res.log, token) {
		t.Fatal("token appeared in installer output or stub log")
	}
	if !strings.Contains(res.log, "systemctl enable --now bot-agent.service") {
		res.fail(t, token, "agent not enabled")
	}
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	assertPerm(t, envPath, 0o600)
	if !strings.Contains(res.log, "chown bot-agent:bot-agent ") || !strings.Contains(res.log, "etc/bot-agent/env") {
		res.fail(t, token, "env chown")
	}
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("BOT_AGENT_SERVER_URL=")) {
		t.Fatal("env missing server url")
	}
}

func TestInstallerRefusals(t *testing.T) {
	t.Parallel()
	t.Run("usage", func(t *testing.T) {
		cmd := exec.Command("sh", "install.sh")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("exit 0: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("code %v\n%s", err, out)
		}
	})
	t.Run("root without test env", func(t *testing.T) {
		dir := t.TempDir()
		before := treeSig(t, dir)
		cmd := exec.Command("sh", "install.sh", "install", "--root", dir)
		cmd.Env = append(os.Environ(), "BOT_AGENT_INSTALLER_TEST=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("exit 0: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("code %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "--root is test-only") {
			t.Fatalf("%s", out)
		}
		if treeSig(t, dir) != before {
			t.Fatal("root flag wrote files")
		}
	})
	t.Run("test env without root", func(t *testing.T) {
		cmd := exec.Command("sh", "install.sh", "check")
		cmd.Env = append(os.Environ(), "BOT_AGENT_INSTALLER_TEST=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("exit 0: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("code %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "test mode requires --root") {
			t.Fatalf("%s", out)
		}
	})
	t.Run("not root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Fatal("this check must run unprivileged")
		}
		cmd := exec.Command("sh", "install.sh", "check")
		cmd.Env = append(os.Environ(), "BOT_AGENT_INSTALLER_TEST=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("exit 0: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("code %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "must run as root") {
			t.Fatalf("%s", out)
		}
	})
	t.Run("root resolves to slash", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "to-root")
		if err := os.Symlink("/", link); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "install.sh", "check", "--root", link)
		cmd.Env = append(os.Environ(), "BOT_AGENT_INSTALLER_TEST=1")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("exit 0: %s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("code %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "--root must not be /") {
			t.Fatalf("%s", out)
		}
	})
}

func TestInstallerUnitBytesUpgradeRollback(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	markBundleUnit(t, old, "unit-v020")
	markBundleUnit(t, next, "unit-v021")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	unit := filepath.Join(rg.root, "etc/systemd/system/bot-agent.service")
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "upgrade")
	}
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("unit-v021")) || bytes.Contains(body, []byte("unit-v020")) {
		t.Fatalf("installed unit:\n%s", body)
	}
	prev, err := os.ReadFile(unit + ".prev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prev, []byte("unit-v020")) || bytes.Contains(prev, []byte("unit-v021")) {
		t.Fatalf("prev unit:\n%s", prev)
	}
	back := runBundle(t, next, rg, "", "rollback")
	if back.code != 0 {
		back.fail(t, "", "rollback")
	}
	body, err = os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("unit-v020")) || bytes.Contains(body, []byte("unit-v021")) {
		t.Fatalf("restored unit:\n%s", body)
	}
}

func TestInstallerSameVersionForce(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	cfg := writeOKConfig(t)
	mustInstall(t, bundle, rg, cfg)
	plantEnv(t, rg)
	etc := filepath.Join(rg.root, "etc/bot-agent")
	envBody, err := os.ReadFile(filepath.Join(etc, "env"))
	if err != nil {
		t.Fatal(err)
	}
	policyBody, err := os.ReadFile(filepath.Join(etc, "runner.toml"))
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(rg.root, "etc/systemd/system/bot-agent.service")
	f, err := os.OpenFile(unit, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n# local-edit\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	res := runBundle(t, bundle, rg, "", "upgrade", "--force")
	if res.code != 0 {
		res.fail(t, "", "force upgrade")
	}
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("local-edit")) {
		t.Fatal("modified unit was kept")
	}
	prev, err := os.ReadFile(unit + ".prev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prev, []byte("local-edit")) {
		t.Fatal(".prev lost the local edit")
	}
	gotEnv, err := os.ReadFile(filepath.Join(etc, "env"))
	if err != nil {
		t.Fatal(err)
	}
	gotPolicy, err := os.ReadFile(filepath.Join(etc, "runner.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotEnv, envBody) || !bytes.Equal(gotPolicy, policyBody) {
		t.Fatal("force upgrade replaced env or runner.toml")
	}
}

func TestInstallerRejectsPolicyMode(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	etc := filepath.Join(rg.root, "etc/bot-agent")
	if err := os.MkdirAll(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(etc, "runner.toml")
	if err := os.WriteFile(path, []byte("enabled = false\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, rg.root)
	res := runBundle(t, bundle, rg, "", "install", "--runner")
	if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "runner.toml mode must be 644") {
		res.fail(t, "", "wrong policy mode")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("fake root changed")
	}
}

func TestInstallerInsecureDevDropin(t *testing.T) {
	t.Parallel()
	want := []byte("# Dev only. Written by install.sh --insecure-dev.\n[Service]\nEnvironment=BOT_AGENT_INSECURE_DEV=1\n")
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	cfg := writeOKConfig(t)
	res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg, "--insecure-dev")
	if res.code != 0 {
		res.fail(t, "", "insecure install")
	}
	drop := filepath.Join(rg.root, "etc/systemd/system/bot-agent.service.d/insecure-dev.conf")
	got, err := os.ReadFile(drop)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("drop-in:\n%s", got)
	}
	assertPerm(t, drop, 0o644)

	plain := newRig(t)
	mustInstall(t, bundle, plain, cfg)
	if _, err := os.Lstat(filepath.Join(plain.root, "etc/systemd/system/bot-agent.service.d/insecure-dev.conf")); !os.IsNotExist(err) {
		t.Fatal("default install wrote the drop-in")
	}
}

func TestInstallerRestartEnrolledInactive(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "inactive-until-restart"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "enrolled inactive upgrade")
	}
	if !strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, "", "enrolled inactive host was not restarted")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version")
	}
}

func TestInstallerUpgradeNotEnrolled(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "not enrolled upgrade")
	}
	if !strings.Contains(res.stdout, "not enrolled: agent not restarted") {
		res.fail(t, "", "missing not-enrolled line")
	}
	if strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, "", "restarted without env")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version")
	}
}

func TestInstallerEmptyNRestartsRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "empty-restarts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 {
		res.fail(t, "", "empty NRestarts was accepted")
	}
	if !strings.Contains(res.combined(), "rolling back") && !strings.Contains(res.combined(), "restored") {
		res.fail(t, "", "no rollback")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("version was not restored")
	}
	if _, err := os.Stat(filepath.Join(rg.root, "usr/local/bin/bot-agent.prev")); !os.IsNotExist(err) {
		t.Fatal(".prev remained after rollback")
	}
}

func TestInstallerRefusesSymlinkDest(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	secret := filepath.Join(rg.root, "secret-payload")
	payload := []byte("secret-payload-bytes\n")
	if err := os.WriteFile(secret, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(rg.root, "usr/local/bin/bot-agent")
	if err := os.Remove(agent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, agent); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, rg.root)
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "refusing symlink") {
		res.fail(t, "", "symlink dest")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("fake root changed")
	}
	got, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("symlink target changed")
	}
}

func TestInstallerRefusesSymlinkBundle(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	bin := filepath.Join(bundle, "bot-agent-v0.2.0-linux-"+bundleArch(t))
	real := bin + ".real"
	if err := os.Rename(bin, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, bin); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, rg.root)
	res := runBundle(t, bundle, rg, "", "install", "--runner")
	if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "bundle symlink") {
		res.fail(t, "", "symlink bundle")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("fake root changed")
	}
}

func TestInstallerUpgradeRenamesBinary(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	agent := filepath.Join(rg.root, "usr/local/bin/bot-agent")
	before := fileInode(t, agent)
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 0 {
		res.fail(t, "", "upgrade")
	}
	after := fileInode(t, agent)
	if before == after {
		t.Fatalf("inode unchanged (%d); replace was not a rename", before)
	}
	if binVersion(t, agent) != "0.2.1" {
		t.Fatal("version")
	}
}

func TestInstallerRemoveDockerAbsent(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	rg.noDocker = true
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	res := runBundle(t, bundle, rg, "", "remove")
	if res.code != 0 || !strings.Contains(res.stdout, "containers not listed (docker absent)") {
		res.fail(t, "", "default remove with docker absent")
	}
	assertGone(t, filepath.Join(rg.root, "usr/local/bin/bot-agent"))
}

func TestInstallerRunnerConfigPinned(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, "0.2.0")

	t.Run("symlink", func(t *testing.T) {
		rg := newRig(t)
		dir := t.TempDir()
		real := filepath.Join(dir, "runner.toml")
		if err := os.WriteFile(real, []byte("enabled = false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.toml")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		before := treeSig(t, rg.root)
		res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", link)
		if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "not a regular file") {
			res.fail(t, "", "symlink runner-config")
		}
		if treeSig(t, rg.root) != before {
			t.Fatal("fake root changed")
		}
	})

	t.Run("directory", func(t *testing.T) {
		rg := newRig(t)
		dir := t.TempDir()
		before := treeSig(t, rg.root)
		res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", dir)
		if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "not a regular file") {
			res.fail(t, "", "directory runner-config")
		}
		if treeSig(t, rg.root) != before {
			t.Fatal("fake root changed")
		}
	})

	t.Run("group writable", func(t *testing.T) {
		rg := newRig(t)
		cfg := filepath.Join(t.TempDir(), "runner.toml")
		if err := os.WriteFile(cfg, []byte("enabled = false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfg, 0o664); err != nil {
			t.Fatal(err)
		}
		before := treeSig(t, rg.root)
		res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
		if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "writable by group or other") {
			res.fail(t, "", "group-writable runner-config")
		}
		if treeSig(t, rg.root) != before {
			t.Fatal("fake root changed")
		}
	})

	t.Run("other writable", func(t *testing.T) {
		rg := newRig(t)
		cfg := filepath.Join(t.TempDir(), "runner.toml")
		if err := os.WriteFile(cfg, []byte("enabled = false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfg, 0o622); err != nil {
			t.Fatal(err)
		}
		before := treeSig(t, rg.root)
		res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
		if res.code == 0 || !strings.Contains(res.stdout, "nothing changed") || !strings.Contains(res.combined(), "writable by group or other") {
			res.fail(t, "", "other-writable runner-config")
		}
		if treeSig(t, rg.root) != before {
			t.Fatal("fake root changed")
		}
	})

	t.Run("swap after check", func(t *testing.T) {
		rg := newRig(t)
		cfg := writeOKConfig(t)
		if err := os.WriteFile(filepath.Join(rg.stubs, "rewrite-runner-config"), []byte(cfg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", cfg)
		if res.code != 0 {
			res.fail(t, "", "install")
		}
		got, err := os.ReadFile(filepath.Join(rg.root, "etc/bot-agent/runner.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "enabled = false\n" {
			t.Fatalf("installed policy:\n%s", got)
		}
		swapped, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if string(swapped) != "enabled = true\n" {
			t.Fatalf("source was not swapped:\n%s", swapped)
		}
	})
}

func TestInstallerUpgradeWriteFailureRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	markBundleUnit(t, next, "unit-new")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	writeExe(t, filepath.Join(rg.stubs, "mv"), mvFailOnceStub(t))
	if err := os.WriteFile(filepath.Join(rg.stubs, "mv-fail-unit-once"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade")
	if res.code != 1 {
		res.fail(t, "", "upgrade write failure")
	}
	if !strings.Contains(res.combined(), "rolling back") {
		res.fail(t, "", "no rollback")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("new binary left installed")
	}
	rec, err := os.ReadFile(filepath.Join(rg.root, "var/lib/bot-agent-installer/installed"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec), "version 0.2.0\n") {
		t.Fatalf("record:\n%s", rec)
	}
}

func TestInstallerRollbackWriteFailureRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	markBundleUnit(t, old, "unit-old")
	markBundleUnit(t, next, "unit-new")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	up := runBundle(t, next, rg, "", "upgrade")
	if up.code != 0 {
		up.fail(t, "", "upgrade")
	}
	writeExe(t, filepath.Join(rg.stubs, "mv"), mvFailOnceStub(t))
	if err := os.WriteFile(filepath.Join(rg.stubs, "mv-fail-unit-once"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "rollback")
	if res.code != 1 {
		res.fail(t, "", "rollback write failure")
	}
	if !strings.Contains(res.combined(), "rolling back") {
		res.fail(t, "", "no rollback")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("binary was not the previous version")
	}
	body, err := os.ReadFile(filepath.Join(rg.root, "etc/systemd/system/bot-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("unit-old")) || bytes.Contains(body, []byte("unit-new")) {
		t.Fatalf("unit after failed rollback:\n%s", body)
	}
}

func mvFailOnceStub(t *testing.T) string {
	t.Helper()
	return `#!/bin/sh
set -eu
real=/bin/mv
if [ ! -x "$real" ]; then
  real=/usr/bin/mv
fi
dest=
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) dest=$a ;;
  esac
done
case "$dest" in
  */bot-agent.service)
    if [ -f "$BOT_AGENT_STUB_DIR/mv-fail-unit-once" ]; then
      rm -f "$BOT_AGENT_STUB_DIR/mv-fail-unit-once"
      printf '%s\n' "mv failed" >&2
      exit 1
    fi
    ;;
esac
exec "$real" "$@"
`
}

func TestInstallerRemoveStopFailure(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	plantWorkloads(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-stop"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "runner-instances"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, bundle, rg, "", "remove", "--remove-workloads")
	if res.code != 1 {
		res.fail(t, "", "remove stop failure")
	}
	for _, needle := range []string{
		"systemctl failed: disable --now deploybot-demo-web.service",
		"systemctl failed: disable --now bot-agent.service",
		"systemctl failed: disable --now bot-agent-runner.socket",
		"systemctl failed: stop bot-agent-runner@*.service",
		"removed unit: deploybot-demo-web.service",
		"summary: remove incomplete",
	} {
		if !strings.Contains(res.stdout, needle) {
			res.fail(t, "", "missing "+needle)
		}
	}
	if strings.Contains(res.stdout, "summary: removed") {
		res.fail(t, "", "reported removed")
	}
	assertGone(t, filepath.Join(rg.root, "usr/local/bin/bot-agent"))
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent.service"))
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/deploybot-demo-web.service"))
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/nginx.service"))
}

func TestInstallerRemoveStopNoMatch(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	if err := os.WriteFile(filepath.Join(rg.stubs, "stop-nomatch"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, bundle, rg, "", "remove")
	if res.code != 0 || !strings.Contains(res.stdout, "summary: removed") || strings.Contains(res.stdout, "systemctl failed:") {
		res.fail(t, "", "no-match stop was a failure")
	}
	assertGone(t, filepath.Join(rg.root, "usr/local/bin/bot-agent"))
}

func TestInstallerRemoveWorkloadsDockerAbsent(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	rg.noDocker = true
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	agent := filepath.Join(rg.root, "usr/local/bin/bot-agent")
	res := runBundle(t, bundle, rg, "", "remove", "--purge", "--remove-workloads")
	if res.code != 1 {
		res.fail(t, "", "remove-workloads with docker absent")
	}
	for _, needle := range []string{
		"containers could not be listed",
		"volumes could not be listed",
		"networks could not be listed",
	} {
		if !strings.Contains(res.stdout, needle) {
			res.fail(t, "", "missing "+needle)
		}
	}
	assertExists(t, agent)
	if strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", "precheck message after units")
	}
}
