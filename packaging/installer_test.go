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
		"BOT_AGENT_ENROLL_WAIT=0",
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
  if [ "${2:-}" = "bot-agent-runner.socket" ] && [ -f "$BOT_AGENT_STUB_DIR/fail-socket-active" ]; then
    rm -f "$BOT_AGENT_STUB_DIR/fail-socket-active"
    printf '%s\n' "inactive"
    exit 3
  fi
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
  if [ -f "$BOT_AGENT_STUB_DIR/inactive-until-enable" ]; then
    if ! grep -q 'systemctl enable --now bot-agent.service' "$BOT_AGENT_STUB_LOG"; then
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
if [ "${1:-}" = "cat" ]; then
  exit 1
fi
if [ -f "$BOT_AGENT_STUB_DIR/fail-socket-disable" ]; then
  if [ "${1:-}" = "disable" ] && [ "${2:-}" = "--now" ] && [ "${3:-}" = "bot-agent-runner.socket" ]; then
    printf '%s\n' "disable failed" >&2
    exit 1
  fi
fi
if [ -f "$BOT_AGENT_STUB_DIR/persist-env-path" ]; then
  _do=0
  if [ "${1:-}" = "restart" ] && [ "${2:-}" = "bot-agent.service" ]; then
    _do=1
  fi
  if [ "${1:-}" = "enable" ] && [ "${2:-}" = "--now" ] && [ "${3:-}" = "bot-agent.service" ]; then
    _do=1
  fi
  if [ "$_do" = "1" ]; then
    _p=$(cat "$BOT_AGENT_STUB_DIR/persist-env-path")
    if [ -n "$_p" ] && [ -f "$_p" ]; then
      if [ -f "$BOT_AGENT_STUB_DIR/persist-env-body" ]; then
        cat "$BOT_AGENT_STUB_DIR/persist-env-body" > "$_p"
      else
        _url=
        while IFS= read -r _line || [ -n "$_line" ]; do
          case "$_line" in
            BOT_AGENT_SERVER_URL=*) _url="$_line" ;;
          esac
        done < "$_p"
        if [ -z "$_url" ]; then
          _url=BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws
        fi
        {
          printf '%s\n' "$_url"
          printf '%s\n' "BOT_AGENT_HOST_ID=11111111-1111-4111-8111-111111111111"
          printf '%s\n' "BOT_AGENT_CREDENTIAL_ID=22222222-2222-4222-8222-222222222222"
          printf '%s\n' "BOT_AGENT_CREDENTIAL=ccccccccccccccccccccccccccccccccccccccccccc"
        } > "$_p"
      fi
      chmod 0600 "$_p"
    fi
  fi
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
if [ -f "$BOT_AGENT_STUB_DIR/chown-swap-env" ]; then
  _last=
  for a in "$@"; do
    _last=$a
  done
  case "$_last" in
    */etc/bot-agent/env)
      _sent=$(cat "$BOT_AGENT_STUB_DIR/chown-swap-env")
      if [ -n "$_sent" ]; then
        rm -f "$_last"
        ln -s "$_sent" "$_last"
      fi
      ;;
  esac
fi
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
    *label=dev.deploybot.owner=bot-agent*) labelled=1 ;;
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
	assertStickyDir(t, filepath.Join(root, "etc/bot-agent"))
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
	for _, needle := range []string{
		"label=dev.deploybot.owner=bot-agent",
	} {
		if !strings.Contains(res.log, needle) {
			res.fail(t, "", "missing "+needle)
		}
	}
	if strings.Contains(res.log, "label=dev.deploybot.owner ") || strings.Contains(res.log, "label=dev.deploybot.owner\n") {
		res.fail(t, "", "label filter has no value")
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

// assertDryRun checks a --dry-run result. want is the exit code.
// A finished plan exits 0. A failed precheck exits 1 and says nothing changed.
// Neither may claim a file was written, deleted, or removed.
func assertDryRun(t *testing.T, res cmdResult, want int, label string) {
	t.Helper()
	if res.code != want {
		res.fail(t, "", fmt.Sprintf("%s: exit %d, want %d", label, res.code, want))
	}
	if strings.Contains(res.stdout, "wrote:") || strings.Contains(res.stderr, "wrote:") {
		res.fail(t, "", label+": printed wrote:")
	}
	body := "\n" + res.stdout + "\n" + res.stderr + "\n"
	for _, claimed := range []string{
		"\ndeleted:",
		"\npurged:",
		"\nuseradd:",
		"\ngroupadd:",
		"\nuserdel:",
		"\nremoved ",
		"\nsummary: installed",
		"\nsummary: upgraded",
		"\nsummary: removed",
		"\nsummary: remove incomplete",
		"\nsummary: enrolled",
		"\nsummary: rolled back",
	} {
		if strings.Contains(body, claimed) {
			res.fail(t, "", label+": claimed a change: "+strings.TrimPrefix(claimed, "\n"))
		}
	}
	if want == 0 && (strings.Contains(res.stderr, "No such file") || strings.Contains(res.stderr, "sha256sum:") || strings.Contains(res.stderr, "shasum:")) {
		res.fail(t, "", label+": hashed a missing path")
	}
	if want == 1 && !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", label+": precheck did not say nothing changed")
	}
}

func assertHasLines(t *testing.T, res cmdResult, label string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(res.combined(), line) {
			res.fail(t, "", label+": missing "+line)
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
	freshInstall := []string{
		"dry-run: write /usr/local/bin/bot-agent mode 0755 owner root:root",
		"dry-run: write /etc/systemd/system/bot-agent.service mode 0644 owner root:root",
		"dry-run: write /usr/local/libexec/bot-agent-runner mode 0755 owner root:root",
		"dry-run: write /etc/systemd/system/bot-agent-runner.socket mode 0644 owner root:root",
		"dry-run: write /etc/systemd/system/bot-agent-runner@.service mode 0644 owner root:root",
		"dry-run: write /etc/tmpfiles.d/bot-agent-runner.conf mode 0644 owner root:root",
		"dry-run: write /etc/bot-agent/runner.toml mode 0644 owner root:root",
		"summary: dry-run install 0.2.0",
	}
	// Fresh enroll and upgrade are prechecks: the agent is not installed.
	// They exit 1. Every other dry-run finishes and exits 0.
	cmds := []struct {
		bundle string
		args   []string
		code   int
		lines  []string
	}{
		{bundle, []string{"check", "--dry-run"}, 0, nil},
		{bundle, []string{"check", "--dry-run", "--runner"}, 0, nil},
		{bundle, []string{"install", "--dry-run", "--runner", "--runner-config", cfg}, 0, freshInstall},
		{bundle, []string{"enroll", "--dry-run", "--server", "wss://example.invalid/agent/v1/ws"}, 1, []string{"bot-agent is not installed"}},
		{bundle, []string{"upgrade", "--dry-run"}, 1, []string{"not installed by the installer"}},
		{bundle, []string{"rollback", "--dry-run"}, 0, []string{"nothing to roll back"}},
		{bundle, []string{"remove", "--dry-run"}, 0, []string{"summary: dry-run remove"}},
		{bundle, []string{"remove", "--dry-run", "--purge", "--remove-workloads"}, 0, []string{"summary: dry-run remove"}},
	}
	for _, c := range cmds {
		sig := treeSig(t, rg.root)
		res := runBundle(t, c.bundle, rg, "", c.args...)
		label := "empty " + strings.Join(c.args, " ")
		if treeSig(t, rg.root) != sig || treeSig(t, rg.root) != before {
			res.fail(t, "", "dry-run changed empty root: "+label)
		}
		assertDryRun(t, res, c.code, label)
		assertHasLines(t, res, label, c.lines...)
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "usr/local/bin/bot-agent")); !os.IsNotExist(err) {
		t.Fatal("fresh install --dry-run created the agent binary")
	}
	installed := mustInstall(t, bundle, rg, cfg)
	_ = installed
	after := treeSig(t, rg.root)
	live := []struct {
		bundle string
		args   []string
		lines  []string
	}{
		{bundle, []string{"check", "--dry-run", "--runner"}, nil},
		{bundle, []string{"install", "--dry-run", "--runner", "--runner-config", cfg}, []string{"already installed, nothing changed"}},
		{bundle, []string{"enroll", "--dry-run", "--server", "wss://example.invalid/agent/v1/ws"}, []string{
			"summary: dry-run enroll",
			"dry-run: exec bot-agent init --server <url> --join-token-stdin --env-file /etc/bot-agent/env",
			"dry-run: systemctl restart bot-agent.service",
			"dry-run: systemctl enable --now bot-agent-runner.socket",
		}},
		{next, []string{"upgrade", "--dry-run"}, []string{
			"dry-run: write /usr/local/bin/bot-agent mode 0755 owner root:root",
			"dry-run: write /usr/local/libexec/bot-agent-runner mode 0755 owner root:root",
			"dry-run: backup replaced files to <file>.prev",
			"summary: dry-run upgrade 0.2.0 -> 0.2.1",
		}},
		{bundle, []string{"rollback", "--dry-run"}, []string{"nothing to roll back"}},
		{bundle, []string{"remove", "--dry-run", "--purge", "--remove-workloads"}, []string{
			"dry-run: delete /usr/local/bin/bot-agent mode 755 owner root:root",
			"summary: dry-run remove",
		}},
	}
	for _, c := range live {
		res := runBundle(t, c.bundle, rg, "", c.args...)
		label := "installed " + strings.Join(c.args, " ")
		if treeSig(t, rg.root) != after {
			res.fail(t, "", "dry-run changed installed root: "+label)
		}
		assertDryRun(t, res, 0, label)
		assertHasLines(t, res, label, c.lines...)
	}

	// A real upgrade leaves .prev. Rollback --dry-run must list restores
	// and leave that set in place.
	up := runBundle(t, next, rg, "", "upgrade")
	if up.code != 0 {
		up.fail(t, "", "upgrade before rollback dry-run")
	}
	withPrev := treeSig(t, rg.root)
	rb := runBundle(t, next, rg, "", "rollback", "--dry-run")
	if treeSig(t, rg.root) != withPrev {
		rb.fail(t, "", "rollback dry-run changed the root")
	}
	assertDryRun(t, rb, 0, "rollback dry-run with prev")
	assertHasLines(t, rb, "rollback dry-run with prev",
		"dry-run: restore /usr/local/bin/bot-agent from /usr/local/bin/bot-agent.prev",
		"summary: dry-run rollback 0.2.0",
	)
	if _, err := os.Lstat(filepath.Join(rg.root, "usr/local/bin/bot-agent.prev")); err != nil {
		t.Fatal("rollback dry-run removed .prev")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("rollback dry-run replaced the binary")
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
	markPersist(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	if res.code != 0 {
		res.fail(t, token, "enroll")
	}
	assertNoToken(t, res, token)
	if strings.Contains(res.log, "systemctl enable --now bot-agent.service") {
		res.fail(t, token, "active agent was enabled instead of restarted")
	}
	if !strings.Contains(res.log, "systemctl restart bot-agent.service") {
		res.fail(t, token, "active agent was not restarted")
	}
	if !strings.Contains(res.log, "systemctl enable --now bot-agent-runner.socket") {
		res.fail(t, token, "runner socket was not enabled")
	}
	for _, line := range []string{
		"systemctl restart bot-agent.service",
		"systemctl enable --now bot-agent-runner.socket",
		"verify: active, NRestarts 0, version 0.2.0",
		"verify: runner socket active",
		"summary: enrolled",
	} {
		if !strings.Contains(res.stdout, line) {
			res.fail(t, token, "missing "+line)
		}
	}
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	assertPerm(t, envPath, 0o600)
	if !strings.Contains(res.log, "chown -h bot-agent:bot-agent ") || !strings.Contains(res.log, "etc/bot-agent/env") {
		res.fail(t, token, "env chown")
	}
	assertLogOrder(t, res, "enroll settle",
		"systemctl restart bot-agent.service",
		"systemctl reset-failed bot-agent.service",
		"systemctl show -p NRestarts --value bot-agent.service",
	)
	assertEnvKeys(t, envPath, true, true, false)
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\n")) || bytes.Contains(body, []byte(token)) {
		t.Fatal("env server url")
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
	if strings.Contains(res.stdout, "verify: active") || strings.Contains(res.stdout, "systemctl restart") {
		res.fail(t, "", "not-enrolled upgrade printed a restart")
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

// watchSocketUnitAtDisable records whether the socket unit file existed
// when systemctl disable --now bot-agent-runner.socket ran.
func watchSocketUnitAtDisable(t *testing.T, rg *rig) {
	t.Helper()
	src := filepath.Join(rg.stubs, "systemctl")
	real := filepath.Join(rg.stubs, "systemctl.real")
	if err := os.Rename(src, real); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket")
	writeExe(t, src, fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "disable" ] && [ "${3:-}" = "bot-agent-runner.socket" ]; then
  if [ -e %q ] || [ -h %q ]; then
    printf '%%s\n' "socket-unit-present-at-disable" >> "$BOT_AGENT_STUB_LOG"
  else
    printf '%%s\n' "socket-unit-absent-at-disable" >> "$BOT_AGENT_STUB_LOG"
  fi
fi
exec %q "$@"
`, unit, unit, real))
}

func assertLogOrder(t *testing.T, res cmdResult, label string, needles ...string) {
	t.Helper()
	pos := 0
	for _, n := range needles {
		rel := strings.Index(res.log[pos:], n)
		if rel < 0 {
			res.fail(t, "", label+": missing "+n)
		}
		pos += rel + len(n)
	}
}

// assertNoSystemctlChange fails when dry-run invoked systemctl for real.
// Every run still calls systemctl --version to see that systemd is present.
func assertNoSystemctlChange(t *testing.T, res cmdResult, label string) {
	t.Helper()
	for _, line := range strings.Split(res.log, "\n") {
		if line == "" || line == "systemctl --version" {
			continue
		}
		if strings.HasPrefix(line, "systemctl ") {
			res.fail(t, "", label+": systemctl was called: "+line)
		}
	}
}

func plantHandAgent(t *testing.T, rg *rig, bundle, version string) {
	t.Helper()
	src := filepath.Join(bundle, "bot-agent-v"+version+"-linux-"+bundleArch(t))
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(rg.root, "usr/local/bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(binDir, "bot-agent")
	if err := os.WriteFile(dst, body, 0o755); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(rg.root, "etc/systemd/system")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(bundle, "units", "bot-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(unitDir, "bot-agent.service")
	if err := os.WriteFile(unitPath, unit, 0o644); err != nil {
		t.Fatal(err)
	}
	plantEnv(t, rg)
	if _, err := os.Lstat(filepath.Join(rg.root, "var/lib/bot-agent-installer")); !os.IsNotExist(err) {
		t.Fatal("hand plant created an installer record")
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "usr/local/libexec/bot-agent-runner")); !os.IsNotExist(err) {
		t.Fatal("hand plant created the helper")
	}
}

func TestInstallerUpgradeRunnerSocketEnrolled(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "upgrade", "--runner")
	if res.code != 0 {
		res.fail(t, "", "enrolled upgrade --runner")
	}
	assertLogOrder(t, res, "enrolled upgrade --runner",
		"systemctl daemon-reload",
		"systemctl enable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
		"systemctl is-active bot-agent-runner.socket",
	)
	for _, line := range []string{
		"systemctl enable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
		"verify: active, NRestarts 0, version 0.2.1",
		"verify: runner socket active",
	} {
		if !strings.Contains(res.stdout, line) {
			res.fail(t, "", "missing "+line)
		}
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version")
	}
}

func TestInstallerUpgradeRunnerSocketNotEnrolled(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	before := treeSig(t, rg.root)
	rg.truncateLog(t)
	dry := runBundle(t, next, rg, "", "upgrade", "--dry-run", "--runner")
	if treeSig(t, rg.root) != before {
		dry.fail(t, "", "not enrolled dry-run changed the root")
	}
	assertDryRun(t, dry, 0, "not enrolled upgrade --dry-run --runner")
	assertNoSystemctlChange(t, dry, "not enrolled upgrade --dry-run --runner")
	if strings.Contains(dry.combined(), "enable --now bot-agent-runner.socket") {
		dry.fail(t, "", "not enrolled dry-run printed socket enable")
	}
	if !strings.Contains(dry.stdout, "not enrolled: agent not restarted") {
		dry.fail(t, "", "not enrolled dry-run missing the not-enrolled line")
	}
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "upgrade", "--runner")
	if res.code != 0 {
		res.fail(t, "", "not enrolled upgrade --runner")
	}
	if !strings.Contains(res.stdout, "not enrolled: agent not restarted") {
		res.fail(t, "", "missing not-enrolled line")
	}
	if strings.Contains(res.stdout, "verify: active") || strings.Contains(res.stdout, "systemctl restart") || strings.Contains(res.stdout, "enable --now bot-agent-runner.socket") {
		res.fail(t, "", "not enrolled upgrade printed a socket or restart step")
	}
	if strings.Contains(res.log, "systemctl enable --now bot-agent-runner.socket") || strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, "", "not enrolled upgrade enabled the socket or restarted")
	}
}

func TestInstallerUpgradeRunnerSocketDryRun(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	before := treeSig(t, rg.root)
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "upgrade", "--dry-run", "--runner")
	if treeSig(t, rg.root) != before {
		res.fail(t, "", "enrolled dry-run changed the root")
	}
	assertDryRun(t, res, 0, "enrolled upgrade --dry-run --runner")
	assertNoSystemctlChange(t, res, "enrolled upgrade --dry-run --runner")
	out := res.stdout
	reloadAt := strings.Index(out, "dry-run: systemctl daemon-reload")
	enableAt := strings.Index(out, "dry-run: systemctl enable --now bot-agent-runner.socket")
	restartAt := strings.Index(out, "dry-run: systemctl restart bot-agent.service")
	if reloadAt < 0 || enableAt < 0 || restartAt < 0 || !(reloadAt < enableAt && enableAt < restartAt) {
		res.fail(t, "", "dry-run socket step order")
	}
	rg.truncateLog(t)
	plain := runBundle(t, next, rg, "", "upgrade", "--dry-run")
	if treeSig(t, rg.root) != before {
		plain.fail(t, "", "enrolled dry-run without --runner changed the root")
	}
	assertNoSystemctlChange(t, plain, "enrolled upgrade --dry-run")
	if strings.Contains(plain.combined(), "enable --now bot-agent-runner.socket") {
		plain.fail(t, "", "upgrade without --runner printed a socket enable")
	}
}

func TestInstallerRollbackRunnerSocketReenable(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	up := runBundle(t, next, rg, "", "upgrade", "--runner")
	if up.code != 0 {
		up.fail(t, "", "upgrade before re-enable rollback")
	}
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "rollback")
	if res.code != 0 {
		res.fail(t, "", "rollback re-enable")
	}
	assertLogOrder(t, res, "rollback re-enable",
		"systemctl daemon-reload",
		"systemctl enable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
	)
	for _, line := range []string{
		"systemctl enable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
		"verify: active, NRestarts 0, version 0.2.0",
		"verify: runner socket active",
	} {
		if !strings.Contains(res.stdout, line) {
			res.fail(t, "", "missing "+line)
		}
	}
	if strings.Contains(res.log, "systemctl disable --now bot-agent-runner.socket") {
		res.fail(t, "", "rollback disabled a socket it restored")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("rollback version")
	}
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
}

func TestInstallerRollbackRunnerSocketDisable(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	up := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if up.code != 0 {
		up.fail(t, "", "hand upgrade before disable rollback")
	}
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
	watchSocketUnitAtDisable(t, rg)
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "rollback")
	if res.code != 0 {
		res.fail(t, "", "rollback disable")
	}
	assertLogOrder(t, res, "rollback disable",
		"systemctl disable --now bot-agent-runner.socket",
		"systemctl daemon-reload",
		"systemctl restart bot-agent.service",
	)
	for _, line := range []string{
		"systemctl disable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
		"verify: active, NRestarts 0, version 0.2.0",
	} {
		if !strings.Contains(res.stdout, line) {
			res.fail(t, "", "missing "+line)
		}
	}
	if strings.Contains(res.stdout, "verify: runner socket active") {
		res.fail(t, "", "disable rollback claimed the socket was active")
	}
	if strings.Contains(res.log, "socket-unit-absent-at-disable") || !strings.Contains(res.log, "socket-unit-present-at-disable") {
		res.fail(t, "", "disable ran without the socket unit file")
	}
	if strings.Contains(res.log, "systemctl enable --now bot-agent-runner.socket") {
		res.fail(t, "", "rollback re-enabled a socket it removed")
	}
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("rollback version")
	}
}

func TestInstallerRollbackRunnerSocketDisableDryRun(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	up := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if up.code != 0 {
		up.fail(t, "", "hand upgrade before disable dry-run")
	}
	before := treeSig(t, rg.root)
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "rollback", "--dry-run")
	if treeSig(t, rg.root) != before {
		res.fail(t, "", "disable dry-run changed the root")
	}
	assertDryRun(t, res, 0, "rollback disable dry-run")
	assertNoSystemctlChange(t, res, "rollback disable dry-run")
	disableAt := strings.Index(res.stdout, "dry-run: systemctl disable --now bot-agent-runner.socket")
	deleteAt := strings.Index(res.stdout, "dry-run: delete /etc/systemd/system/bot-agent-runner.socket")
	if disableAt < 0 || deleteAt < 0 || disableAt > deleteAt {
		res.fail(t, "", "disable dry-run is not before the socket delete")
	}
	assertExists(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
}

func TestInstallerUpgradeRunnerVerifyFailDisablesSocketBeforeDelete(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-restarts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watchSocketUnitAtDisable(t, rg)
	res := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if res.code == 0 {
		res.fail(t, "", "verify failure was accepted")
	}
	if !strings.Contains(res.combined(), "upgrade failed, restored 0.2.0") {
		res.fail(t, "", "automatic rollback did not restore 0.2.0")
	}
	if strings.Contains(res.log, "socket-unit-absent-at-disable") || !strings.Contains(res.log, "socket-unit-present-at-disable") {
		res.fail(t, "", "disable ran without the socket unit file")
	}
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("old agent was not restored")
	}
}

func TestInstallerRefusesSymlinkedEtcBotAgent(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	target := t.TempDir()
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(rg.root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "bot-agent")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, bundle, rg, "", "install")
	if res.code == 0 || !strings.Contains(res.combined(), "refusing symlink /etc/bot-agent") {
		res.fail(t, "", "symlinked /etc/bot-agent was not refused")
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("link target mode %o, want %o", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestInstallerHandBinaryGroupWritableNotExecuted(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.1")
	binDir := filepath.Join(rg.root, "usr/local/bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(rg.root, "hand-version-executed")
	dst := filepath.Join(binDir, "bot-agent")
	body := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' executed > %q\nprintf '%%s\\n' '9.9.9'\n", marker)
	if err := os.WriteFile(dst, []byte(body), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o775); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("fixture is not group-writable: %o", info.Mode().Perm())
	}
	res := runBundle(t, bundle, rg, "", "check")
	if res.code != 0 {
		res.fail(t, "", "check")
	}
	if !strings.Contains(res.stdout, "installed: none\n") || strings.Contains(res.stdout, "9.9.9") || strings.Contains(res.stdout, "(not installed by the installer)") {
		res.fail(t, "", "group-writable binary was treated as installed")
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("group-writable binary was executed")
	}
}

func TestInstallerRunnerSocketInactiveRollsBack(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-socket-active"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, next, rg, "", "upgrade", "--runner")
	if res.code == 0 {
		res.fail(t, "", "inactive socket was accepted")
	}
	if !strings.Contains(res.combined(), "verify: runner socket is inactive") {
		res.fail(t, "", "missing socket verify line")
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

func TestInstallerHandInstalledVersion(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	const handLine = "installed: 0.2.0 (not installed by the installer)"

	check := runBundle(t, next, rg, "", "check")
	if check.code != 0 {
		check.fail(t, "", "hand check")
	}
	if !strings.Contains(check.stdout, handLine) || !strings.Contains(check.stdout, "summary: check ok") {
		check.fail(t, "", "hand check installed line")
	}
	if strings.Contains(check.stdout, "installed: none") {
		check.fail(t, "", "hand check said none")
	}

	before := treeSig(t, rg.root)
	refused := runBundle(t, next, rg, "", "upgrade")
	if refused.code == 0 || !strings.Contains(refused.stdout, handLine) || !strings.Contains(refused.stdout, "nothing changed") {
		refused.fail(t, "", "hand upgrade without --force")
	}
	if treeSig(t, rg.root) != before {
		refused.fail(t, "", "refused hand upgrade changed the root")
	}

	rg.truncateLog(t)
	dry := runBundle(t, next, rg, "", "upgrade", "--dry-run", "--force", "--runner")
	if treeSig(t, rg.root) != before {
		dry.fail(t, "", "hand dry-run changed the root")
	}
	assertDryRun(t, dry, 0, "hand upgrade dry-run")
	assertNoSystemctlChange(t, dry, "hand upgrade dry-run")
	if !strings.Contains(dry.stdout, handLine) || !strings.Contains(dry.stdout, "summary: dry-run upgrade 0.2.0 -> 0.2.1") {
		dry.fail(t, "", "hand dry-run summary")
	}
	if !strings.Contains(dry.stdout, "dry-run: systemctl enable --now bot-agent-runner.socket") {
		dry.fail(t, "", "hand dry-run missing socket step")
	}

	rg.truncateLog(t)
	up := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if up.code != 0 {
		up.fail(t, "", "hand upgrade")
	}
	if !strings.Contains(up.stdout, handLine) || !strings.Contains(up.stdout, "summary: upgraded 0.2.0 -> 0.2.1") {
		up.fail(t, "", "hand upgrade summary")
	}
	if strings.Contains(up.stdout, "unknown") {
		up.fail(t, "", "hand upgrade used unknown")
	}
	assertLogOrder(t, up, "hand upgrade",
		"systemctl daemon-reload",
		"systemctl enable --now bot-agent-runner.socket",
		"systemctl restart bot-agent.service",
	)
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version")
	}

	again := runBundle(t, next, rg, "", "check")
	if again.code != 0 {
		again.fail(t, "", "check after hand upgrade")
	}
	if !strings.Contains(again.stdout, "installed: 0.2.1\n") || strings.Contains(again.stdout, "not installed by the installer)") {
		again.fail(t, "", "recorded version line")
	}
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

func assertStickyDir(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := fi.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetuid | os.ModeSetgid)
	want := os.ModeSticky | 0o770
	if got != want {
		t.Fatalf("%s mode %#o, want sticky|0770", path, got)
	}
}

func assertNoToken(t *testing.T, res cmdResult, token string) {
	t.Helper()
	if token != "" && (strings.Contains(res.stdout, token) || strings.Contains(res.stderr, token) || strings.Contains(res.log, token)) {
		t.Fatal("token appeared in installer output or stub log")
	}
}

func markPersist(t *testing.T, rg *rig) {
	t.Helper()
	path := filepath.Join(rg.root, "etc", "bot-agent", "env")
	if err := os.WriteFile(filepath.Join(rg.stubs, "persist-env-path"), []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertEnvKeys(t *testing.T, path string, wantHost, wantCred, wantJoin bool) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	host, cred, join := false, false, false
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "BOT_AGENT_HOST_ID=") && len(line) > len("BOT_AGENT_HOST_ID="):
			host = true
		case strings.HasPrefix(line, "BOT_AGENT_CREDENTIAL_ID=") && len(line) > len("BOT_AGENT_CREDENTIAL_ID="):
			cred = true
		case strings.HasPrefix(line, "BOT_AGENT_JOIN_TOKEN="):
			join = true
		}
	}
	if host != wantHost || cred != wantCred || join != wantJoin {
		t.Fatalf("env keys host=%t cred=%t join=%t, want %t %t %t", host, cred, join, wantHost, wantCred, wantJoin)
	}
}

func TestInstallerEtcModeRepaired(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	etc := filepath.Join(rg.root, "etc/bot-agent")
	assertStickyDir(t, etc)
	if err := os.Chmod(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	plantEnv(t, rg)
	up := runBundle(t, next, rg, "", "upgrade")
	if up.code != 0 {
		up.fail(t, "", "upgrade repairs etc mode")
	}
	assertStickyDir(t, etc)
	if err := os.Chmod(etc, 0o750); err != nil {
		t.Fatal(err)
	}
	back := runBundle(t, next, rg, "", "rollback")
	if back.code != 0 {
		back.fail(t, "", "rollback repairs etc mode")
	}
	assertStickyDir(t, etc)
}

func TestInstallerEnrollInsecureDev(t *testing.T) {
	t.Parallel()
	const token = "EnrollInsecureToken12"
	wantDrop := []byte("# Dev only. Written by install.sh --insecure-dev.\n[Service]\nEnvironment=BOT_AGENT_INSECURE_DEV=1\n")
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	drop := filepath.Join(rg.root, "etc/systemd/system/bot-agent.service.d/insecure-dev.conf")

	before := treeSig(t, rg.root)
	dry := runBundle(t, bundle, rg, "", "enroll", "--dry-run", "--insecure-dev", "--server", "ws://127.0.0.1:9")
	if treeSig(t, rg.root) != before {
		dry.fail(t, "", "insecure enroll dry-run changed the root")
	}
	assertDryRun(t, dry, 0, "insecure enroll dry-run")
	if !strings.Contains(dry.stdout, "dry-run: write /etc/systemd/system/bot-agent.service.d/insecure-dev.conf mode 0644 owner root:root") {
		dry.fail(t, "", "insecure enroll dry-run missing the drop-in")
	}
	if _, err := os.Lstat(drop); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote the drop-in")
	}

	bad := runBundle(t, bundle, rg, "not a token\n", "enroll", "--insecure-dev", "--server", "ws://127.0.0.1:9")
	if bad.code == 0 || !strings.Contains(bad.combined(), "enroll failed") || !strings.Contains(bad.stdout, "nothing changed") {
		bad.fail(t, "not a token", "bad token enroll")
	}
	assertNoToken(t, bad, "not a token")
	if _, err := os.Lstat(drop); !os.IsNotExist(err) {
		t.Fatal("failed init wrote the drop-in")
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "etc/bot-agent/env")); !os.IsNotExist(err) {
		t.Fatal("failed init wrote the env file")
	}

	markPersist(t, rg)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--insecure-dev", "--server", "ws://127.0.0.1:9")
	if res.code != 0 {
		res.fail(t, token, "insecure enroll")
	}
	assertNoToken(t, res, token)
	got, err := os.ReadFile(drop)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantDrop) {
		t.Fatalf("drop-in:\n%s", got)
	}
	assertPerm(t, drop, 0o644)
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("BOT_AGENT_SERVER_URL=ws://127.0.0.1:9/agent/v1/ws\n")) || bytes.Contains(body, []byte(token)) {
		t.Fatal("stored server url")
	}
	assertEnvKeys(t, envPath, true, true, false)

	// An existing drop-in still lets init use ws without passing the flag again.
	plain := newRig(t)
	installed := runBundle(t, bundle, plain, "", "install", "--runner", "--runner-config", writeOKConfig(t), "--insecure-dev")
	if installed.code != 0 {
		installed.fail(t, "", "install insecure before enroll")
	}
	markPersist(t, plain)
	again := runBundle(t, bundle, plain, token+"\n", "enroll", "--server", "ws://127.0.0.1:9")
	if again.code != 0 {
		again.fail(t, token, "enroll with existing drop-in")
	}
	assertNoToken(t, again, token)
	body, err = os.ReadFile(filepath.Join(plain.root, "etc/bot-agent/env"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("BOT_AGENT_SERVER_URL=ws://127.0.0.1:9/agent/v1/ws\n")) {
		t.Fatal("existing drop-in did not complete the url")
	}
}

func TestInstallerEnrollWsWithoutFlag(t *testing.T) {
	t.Parallel()
	const token = "EnrollWsTokenValue1"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	before := treeSig(t, rg.root)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "ws://127.0.0.1:9")
	if res.code == 0 || !strings.Contains(res.combined(), "enroll failed") || !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, token, "ws enroll without the flag")
	}
	assertNoToken(t, res, token)
	if strings.Contains(res.combined(), "summary: enrolled") {
		res.fail(t, token, "ws enroll claimed success")
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "etc/bot-agent/env")); !os.IsNotExist(err) {
		t.Fatal("rejected ws enroll wrote the env file")
	}
	if _, err := os.Lstat(filepath.Join(rg.root, "etc/systemd/system/bot-agent.service.d/insecure-dev.conf")); !os.IsNotExist(err) {
		t.Fatal("rejected ws enroll wrote the drop-in")
	}
	if treeSig(t, rg.root) != before {
		t.Fatal("rejected ws enroll changed the root")
	}
}

func TestInstallerEnrollInactiveEnable(t *testing.T) {
	t.Parallel()
	const token = "EnrollInactiveToken1"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	if err := os.WriteFile(filepath.Join(rg.stubs, "inactive-until-enable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markPersist(t, rg)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	if res.code != 0 {
		res.fail(t, token, "inactive enroll")
	}
	assertNoToken(t, res, token)
	if strings.Contains(res.stdout, "systemctl restart bot-agent.service") || strings.Contains(res.log, "systemctl restart ") {
		res.fail(t, token, "inactive enroll restarted")
	}
	for _, line := range []string{
		"systemctl enable --now bot-agent.service",
		"systemctl enable --now bot-agent-runner.socket",
		"verify: active, NRestarts 0, version 0.2.0",
		"verify: runner socket active",
		"summary: enrolled",
	} {
		if !strings.Contains(res.stdout, line) {
			res.fail(t, token, "missing "+line)
		}
	}
	assertEnvKeys(t, filepath.Join(rg.root, "etc/bot-agent/env"), true, true, false)
}

func TestInstallerEnrollCredentialNotPersisted(t *testing.T) {
	t.Parallel()
	const token = "EnrollMissingKeys123"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	if res.code != 1 || !strings.Contains(res.combined(), "enroll failed: credential not persisted") {
		res.fail(t, token, "missing credential keys")
	}
	assertNoToken(t, res, token)
	if strings.Contains(res.stdout, "summary: enrolled") || strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, token, "failed enroll printed success")
	}
	if !strings.Contains(res.stdout, "systemctl restart bot-agent.service") {
		res.fail(t, token, "active agent was not restarted before the check")
	}
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	assertEnvKeys(t, envPath, false, false, true)
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(token)) == false || bytes.Contains(body, []byte("BOT_AGENT_HOST_ID=")) {
		t.Fatal("env did not keep the join token without a credential")
	}
}

func TestInstallerEnrollChmodBeforeChown(t *testing.T) {
	t.Parallel()
	const token = "EnrollChmodToken1234"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	sentinel := filepath.Join(rg.root, "sentinel-not-env")
	if err := os.WriteFile(sentinel, []byte("sentinel-bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "chown-swap-env"), []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(rg.stubs, "chmod"), `#!/bin/sh
set -eu
printf '%s\n' "chmod $*" >> "$BOT_AGENT_STUB_LOG"
real=/bin/chmod
if [ ! -x "$real" ]; then
  real=/usr/bin/chmod
fi
exec "$real" "$@"
`)
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	assertNoToken(t, res, token)
	chmodAt := strings.Index(res.log, "chmod 0600 ")
	chownAt := strings.Index(res.log, "chown -h bot-agent:bot-agent ")
	if chmodAt < 0 || chownAt < 0 || chmodAt > chownAt {
		res.fail(t, token, "chmod 0600 did not run before chown -h")
	}
	chmodLine := res.log[chmodAt:]
	if nl := strings.Index(chmodLine, "\n"); nl >= 0 {
		chmodLine = chmodLine[:nl]
	}
	chownLine := res.log[chownAt:]
	if nl := strings.Index(chownLine, "\n"); nl >= 0 {
		chownLine = chownLine[:nl]
	}
	if !strings.HasSuffix(chmodLine, "/etc/bot-agent/env") || !strings.HasSuffix(chownLine, "/etc/bot-agent/env") {
		res.fail(t, token, "env chmod or chown targeted another path")
	}
	for _, line := range strings.Split(res.log[chownAt+len(chownLine):], "\n") {
		if strings.HasPrefix(line, "chmod ") && strings.HasSuffix(line, "/etc/bot-agent/env") {
			res.fail(t, token, "chmod followed env after chown")
		}
	}
	info, err := os.Lstat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("sentinel mode %o, want 644", info.Mode().Perm())
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sentinel-bytes\n" {
		t.Fatalf("sentinel bytes changed: %q", got)
	}
	envPath := filepath.Join(rg.root, "etc/bot-agent/env")
	link, err := os.Lstat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		t.Fatal("env was not replaced at chown")
	}
}

func TestInstallerEnrollEnvParse(t *testing.T) {
	t.Parallel()
	const (
		token = "EnrollParseToken1234"
		host  = "11111111-1111-4111-8111-111111111111"
		cred  = "22222222-2222-4222-8222-222222222222"
	)
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{
			name: "export with leading space",
			body: "BOT_AGENT_HOST_ID=" + host + "\nBOT_AGENT_CREDENTIAL_ID=" + cred + "\n export BOT_AGENT_JOIN_TOKEN=x\n",
		},
		{
			name: "space before equals",
			body: "BOT_AGENT_HOST_ID=" + host + "\nBOT_AGENT_CREDENTIAL_ID=" + cred + "\nBOT_AGENT_JOIN_TOKEN =x\n",
		},
		{
			name: "quoted empty host id",
			body: "BOT_AGENT_HOST_ID=\"\"\nBOT_AGENT_CREDENTIAL_ID=" + cred + "\n",
		},
		{
			name: "quoted ids",
			body: "BOT_AGENT_HOST_ID=\"" + host + "\"\nBOT_AGENT_CREDENTIAL_ID=\"" + cred + "\"\n",
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rg := newRig(t)
			bundle := makeBundle(t, "0.2.0")
			mustInstall(t, bundle, rg, writeOKConfig(t))
			markPersist(t, rg)
			if err := os.WriteFile(filepath.Join(rg.stubs, "persist-env-body"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
			assertNoToken(t, res, token)
			if strings.Contains(res.combined(), " export BOT_AGENT_JOIN_TOKEN") || strings.Contains(res.combined(), "BOT_AGENT_JOIN_TOKEN =x") {
				res.fail(t, token, "env line was printed")
			}
			envPath := filepath.Join(rg.root, "etc/bot-agent/env")
			if _, err := os.Lstat(envPath); err != nil {
				t.Fatal(err)
			}
			if tc.ok {
				if res.code != 0 || !strings.Contains(res.stdout, "summary: enrolled") {
					res.fail(t, token, "quoted ids were not ready")
				}
				return
			}
			if res.code != 1 || !strings.Contains(res.combined(), "enroll failed: credential not persisted") {
				res.fail(t, token, "malformed env was accepted")
			}
			if strings.Contains(res.stdout, "summary: enrolled") {
				res.fail(t, token, "malformed env claimed enrolled")
			}
		})
	}
}

func TestInstallerEnrollSettleFailure(t *testing.T) {
	t.Parallel()
	const token = "EnrollSettleToken123"
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	markPersist(t, rg)
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-restarts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runBundle(t, bundle, rg, token+"\n", "enroll", "--server", "wss://example.invalid/agent/v1/ws")
	assertNoToken(t, res, token)
	if res.code != 1 {
		res.fail(t, token, "settle failure was accepted")
	}
	for _, needle := range []string{
		"systemctl stop bot-agent",
		"move /etc/bot-agent/env aside",
		"then enroll again",
	} {
		if !strings.Contains(res.combined(), needle) {
			res.fail(t, token, "missing "+needle)
		}
	}
	if strings.Contains(res.stdout, "nothing changed") || strings.Contains(res.stdout, "summary: enrolled") {
		res.fail(t, token, "settle failure looked successful")
	}
	assertEnvKeys(t, filepath.Join(rg.root, "etc/bot-agent/env"), true, true, false)
	assertLogOrder(t, res, "settle order",
		"systemctl restart bot-agent.service",
		"systemctl reset-failed bot-agent.service",
		"systemctl show -p NRestarts --value bot-agent.service",
	)
}

func TestInstallerRollbackSocketStopStillRestores(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-restarts"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-socket-disable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watchSocketUnitAtDisable(t, rg)
	res := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if res.code == 0 {
		res.fail(t, "", "verify failure was accepted")
	}
	if !strings.Contains(res.combined(), "upgrade failed, restored 0.2.0") {
		res.fail(t, "", "restore did not finish after the socket stop failed")
	}
	if strings.Contains(res.log, "socket-unit-absent-at-disable") || !strings.Contains(res.log, "socket-unit-present-at-disable") {
		res.fail(t, "", "socket stop did not run while the unit file existed")
	}
	assertGone(t, filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket"))
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("old agent was not restored")
	}
}

func TestInstallerRollbackAbsentSocketIsSuccess(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	plantHandAgent(t, rg, old, "0.2.0")
	up := runBundle(t, next, rg, "", "upgrade", "--force", "--runner")
	if up.code != 0 {
		up.fail(t, "", "upgrade")
	}
	socket := filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket")
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-socket-disable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "rollback")
	if res.code != 0 {
		res.fail(t, "", "absent socket disable failed the rollback")
	}
	if strings.Contains(res.log, "systemctl disable --now bot-agent-runner.socket") {
		res.fail(t, "", "disable ran for an absent socket unit")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("rollback version")
	}
}

func TestInstallerRemoveAbsentSocketDisable(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	installed := runBundle(t, bundle, rg, "", "install")
	if installed.code != 0 {
		installed.fail(t, "", "install without runner")
	}
	socket := filepath.Join(rg.root, "etc/systemd/system/bot-agent-runner.socket")
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatal("install without runner created the socket unit")
	}
	if err := os.WriteFile(filepath.Join(rg.stubs, "fail-socket-disable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rg.truncateLog(t)
	res := runBundle(t, bundle, rg, "", "remove")
	if res.code != 0 || !strings.Contains(res.stdout, "summary: removed") || strings.Contains(res.stdout, "summary: remove incomplete") {
		res.fail(t, "", "absent socket made remove incomplete")
	}
	if strings.Contains(res.log, "systemctl disable --now bot-agent-runner.socket") {
		res.fail(t, "", "disable ran for a socket that was never installed")
	}
	assertGone(t, filepath.Join(rg.root, "usr/local/bin/bot-agent"))
}

func TestInstallerUpgradeRefusesPolicySymlink(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	installed := runBundle(t, old, rg, "", "install")
	if installed.code != 0 {
		installed.fail(t, "", "install without runner")
	}
	sentinel := filepath.Join(rg.root, "sentinel-policy")
	if err := os.WriteFile(sentinel, []byte("not-a-template\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(rg.root, "etc/bot-agent/runner.toml")
	writeExe(t, filepath.Join(rg.stubs, "mv"), fmt.Sprintf(`#!/bin/sh
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
"$real" "$@"
case "$dest" in
  */usr/local/libexec/bot-agent-runner)
    if [ ! -e %q ] && [ ! -h %q ]; then
      ln -s %q %q
    fi
    ;;
esac
`, policy, policy, sentinel, policy))
	res := runBundle(t, next, rg, "", "upgrade", "--runner")
	if res.code == 0 || !strings.Contains(res.combined(), "refusing symlink /etc/bot-agent/runner.toml") {
		res.fail(t, "", "policy symlink was not refused")
	}
	if strings.Contains(res.combined(), "kept: /etc/bot-agent/runner.toml") {
		res.fail(t, "", "policy symlink was kept")
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "not-a-template\n" || bytes.Contains(got, []byte("enabled = false")) {
		t.Fatalf("sentinel changed: %q", got)
	}
}

func TestInstallerReexecStagedScript(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	res := runBundle(t, bundle, rg, "", "check")
	if res.code != 0 {
		res.fail(t, "", "check")
	}
	raw, err := os.ReadFile(filepath.Join(rg.stubs, "installer-argv0"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(raw))
	if !strings.Contains(got, "bot-agent-inst.") || !strings.HasSuffix(got, "/install.sh") {
		t.Fatalf("continuing script was %q", got)
	}
	if got == filepath.Join(bundle, "install.sh") {
		t.Fatal("ran the bundle copy")
	}
	if _, err := os.Lstat(got); !os.IsNotExist(err) {
		t.Fatalf("stage directory was not removed: %v", err)
	}
	if strings.Count(res.stdout, "arch:") != 1 {
		res.fail(t, "", "arch line was repeated")
	}
	if strings.Count(res.stdout, "systemd: present") != 1 {
		res.fail(t, "", "systemd line was repeated")
	}
	dry := runBundle(t, bundle, rg, "", "check", "--dry-run")
	if dry.code != 0 {
		dry.fail(t, "", "check dry-run")
	}
	if strings.Count(dry.stdout, "systemd: present") != 1 {
		dry.fail(t, "", "dry-run duplicated the preamble")
	}
}
