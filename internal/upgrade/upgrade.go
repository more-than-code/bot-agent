// Package upgrade is `bot-agent-runner upgrade-run <version>`
// (agent-protocol v1.20 item 6). It runs as root in the transient unit
// bot-agent-upgrade, outside the helper's sandbox: it downloads a
// release from the fixed GitHub URL, checks the signature with the
// compiled key, unpacks and checks the bundle, runs the bundled
// install.sh upgrade, and writes result.json. The server supplies only the
// version, which the helper has already checked.
package upgrade

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/release"
)

// Env is what one run may touch. Production uses Production(); tests
// point every path and command at temp dirs and fakes.
type Env struct {
	// StateDir is /var/lib/bot-agent-runner/upgrade.
	StateDir string
	// LockPath is /run/bot-agent/upgrade.lock, held for the whole run so
	// the helper's mutating verbs answer busy. Empty skips it (tests).
	LockPath string
	// RunnerInstalled tells whether to pass --runner to install.sh.
	RunnerInstalled func() bool
	// Fetch downloads the three release files.
	Fetch Fetcher
	// PublicKeys are the release keys. Nil is release.PublicKeys.
	PublicKeys []ed25519.PublicKey
	// Arch is the bundle architecture. Empty is runtime.GOARCH.
	Arch string
	// Version runs `<bin> version` and returns its trimmed stdout.
	Version func(ctx context.Context, bin string) (string, error)
	// Install runs the bundled install.sh with args from dir and returns
	// its exit code and combined output.
	Install func(ctx context.Context, dir string, args []string) (int, string, error)
	Now     func() time.Time
	Logf    func(string, ...any)
}

// Fetcher downloads one release file by name for version.
type Fetcher interface {
	Fetch(ctx context.Context, version, name string) ([]byte, error)
}

// File names in a release (item 6.1).
func tarballName(version string) string { return "bot-agent-v" + version + ".tar.gz" }

const (
	sumsName = "SHA256SUMS"
	sigName  = "SHA256SUMS.sig"
)

// Run performs one upgrade and writes result.json. The returned result is
// what was written (or what would have been, when the write failed).
func Run(ctx context.Context, version string, env Env) (agentupgrade.Result, error) {
	if !agentupgrade.ValidVersion(version) {
		return agentupgrade.Result{}, errors.New("bad version")
	}
	if env.LockPath != "" {
		unlock, err := holdLock(env.LockPath)
		if err != nil {
			return agentupgrade.Result{}, err
		}
		defer unlock()
	}
	if err := ensureDir(env.StateDir); err != nil {
		return agentupgrade.Result{}, err
	}
	outcome, reason := attempt(ctx, version, env)
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	res := agentupgrade.Result{
		Version: version, Outcome: outcome, Reason: reason,
		At: now().UTC().Format(time.RFC3339),
	}
	env.logf("bot-agent-runner: upgrade-run %s %s %s", version, outcome, dash(reason))
	err := writeResult(filepath.Join(env.StateDir, "result.json"), res)
	pruneStaged(env.StateDir, version)
	return res, err
}

// attempt returns the outcome and reason token.
// The order is fixed (item 6): SHA256SUMS and its signature first, then
// the tarball only after the signature verified.
func attempt(ctx context.Context, version string, env Env) (string, string) {
	if env.Fetch == nil {
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonDownload
	}
	sums, err := env.Fetch.Fetch(ctx, version, sumsName)
	if err == nil {
		var sig []byte
		sig, err = env.Fetch.Fetch(ctx, version, sigName)
		if err == nil {
			pubs := env.PublicKeys
			if pubs == nil {
				pubs = release.PublicKeys
			}
			if verr := release.VerifyAny(pubs, sums, sig); verr != nil {
				env.logf("bot-agent-runner: upgrade-run signature: %s", verr)
				return agentupgrade.OutcomeFailed, agentupgrade.ReasonSignature
			}
		}
	}
	if err != nil {
		env.logf("bot-agent-runner: upgrade-run download: %s", err)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonDownload
	}
	tarball, err := env.Fetch.Fetch(ctx, version, tarballName(version))
	if err != nil {
		env.logf("bot-agent-runner: upgrade-run download: %s", err)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonDownload
	}
	want, err := parseSums(sums)
	if err != nil {
		env.logf("bot-agent-runner: upgrade-run SHA256SUMS: %s", err)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonBundle
	}
	stage := filepath.Join(env.StateDir, version)
	top := "bot-agent-v" + version
	if err := Unpack(tarball, stage, top); err != nil {
		env.logf("bot-agent-runner: upgrade-run unpack: %s", err)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonBundle
	}
	bundle := filepath.Join(stage, top)
	if err := checkBundle(bundle, sums, want); err != nil {
		env.logf("bot-agent-runner: upgrade-run bundle: %s", err)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonBundle
	}
	arch := env.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	agentName := top + "-linux-" + arch
	if _, listed := want[agentName]; !listed {
		env.logf("bot-agent-runner: upgrade-run bundle: no agent for %s", arch)
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonBundle
	}
	got, err := env.Version(ctx, filepath.Join(bundle, agentName))
	if err != nil || got != version {
		env.logf("bot-agent-runner: upgrade-run bundled agent version does not match")
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonVersion
	}
	args := []string{"upgrade"}
	if env.RunnerInstalled != nil && env.RunnerInstalled() {
		args = append(args, "--runner")
	}
	code, out, err := env.Install(ctx, bundle, args)
	if ctx.Err() != nil {
		// Stopped (SIGTERM): the installer got the signal and its grace
		// to roll back, but this run cannot vouch for the result.
		env.logf("bot-agent-runner: upgrade-run stopped during install")
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonInstall
	}
	if err == nil && code == 0 {
		return agentupgrade.OutcomeSucceeded, ""
	}
	return classifyInstall(out)
}

// classifyInstall maps a failed install.sh run onto the contract. Only a
// verified restore ("upgrade failed, restored <version>") is rolled_back;
// the marker line then names a missing hello. A restore whose own verify
// failed ("rollback verify failed") and every other failure is failed.
func classifyInstall(out string) (string, string) {
	if !strings.Contains(out, "upgrade failed, restored ") {
		return agentupgrade.OutcomeFailed, agentupgrade.ReasonInstall
	}
	if strings.Contains(out, NoHelloLine) {
		return agentupgrade.OutcomeRolledBack, agentupgrade.ReasonNotReconnected
	}
	return agentupgrade.OutcomeRolledBack, agentupgrade.ReasonInstall
}

// NoHelloLine is what install.sh prints when the agent wrote no hello
// marker after the restart. packaging tests check the installer prints it.
const NoHelloLine = "verify: no hello after restart"

// parseSums reads "<sha256>  <path>" lines (an optional '*' before the
// path). Paths must be relative, without "..", and unique.
func parseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, errors.New("bad line")
		}
		if _, err := hex.DecodeString(f[0]); err != nil || strings.ToLower(f[0]) != f[0] {
			return nil, errors.New("bad hash")
		}
		p := strings.TrimPrefix(f[1], "*")
		if !safeRel(p) {
			return nil, errors.New("bad path")
		}
		if _, dup := out[p]; dup {
			return nil, errors.New("duplicate path")
		}
		out[p] = f[0]
	}
	if len(out) == 0 {
		return nil, errors.New("empty")
	}
	return out, nil
}

// safeRel is a relative slash path with no empty, "." or ".." segment.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// requiredFiles must be listed in SHA256SUMS (item 6.3).
var requiredFiles = []string{
	"install.sh",
	"units/bot-agent.service",
	"units/bot-agent-runner.socket",
	"units/bot-agent-runner@.service",
	"units/tmpfiles-bot-agent-runner.conf",
}

// checkBundle requires the bundled SHA256SUMS to equal the signed one byte
// for byte and to list install.sh and the four units, every listed file to
// be a regular file with that hash, and no other file in the tree.
func checkBundle(bundle string, signed []byte, want map[string]string) error {
	inner, err := os.ReadFile(filepath.Join(bundle, sumsName))
	if err != nil {
		return errors.New("bundled SHA256SUMS missing")
	}
	if !bytes.Equal(inner, signed) {
		return errors.New("bundled SHA256SUMS differs from the signed one")
	}
	for _, rel := range requiredFiles {
		if _, ok := want[rel]; !ok {
			return fmt.Errorf("SHA256SUMS does not list %s", rel)
		}
	}
	err = filepath.WalkDir(bundle, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(bundle, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == sumsName {
			return nil
		}
		if _, ok := want[rel]; !ok {
			return fmt.Errorf("%s: not listed in SHA256SUMS", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for rel, sum := range want {
		got, err := hashRegular(filepath.Join(bundle, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("%s: missing or not a regular file", rel)
		}
		if got != sum {
			return fmt.Errorf("%s: sha256 mismatch", rel)
		}
	}
	return nil
}

func hashRegular(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeResult(path string, r agentupgrade.Result) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".result-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(agentupgrade.Encode(r), '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// pruneStaged removes every staged version directory except keep.
// result.json and other files stay.
func pruneStaged(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if !ent.IsDir() || ent.Name() == keep || !agentupgrade.ValidVersion(ent.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, ent.Name()))
	}
}

// ensureDir creates dir 0700 and refuses a symlink or non-directory.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("state dir is not a directory")
	}
	return os.Chmod(dir, 0o700)
}

func (e Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// fixedEnv is the environment for the bundled agent and install.sh.
func fixedEnv(extra ...string) []string {
	return append([]string{"LANG=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}, extra...)
}

// versionTimeout bounds `bot-agent version`.
const versionTimeout = 10 * time.Second

// RunVersion is the production Env.Version.
func RunVersion(ctx context.Context, bin string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "version")
	cmd.Env = fixedEnv()
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout = &capped{buf: &out, max: 256}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// maxInstallOut bounds the installer output kept for classification.
const maxInstallOut = 1 << 20

// installGrace is how long install.sh gets after SIGTERM to finish its
// own rollback before it is killed.
var installGrace = 120 * time.Second

// RunInstall is the production Env.Install: /bin/sh <dir>/install.sh args
// with BOT_AGENT_INSTALL_BUNDLE=dir. Its output also goes to stdout, which
// the unit sends to the journal. A cancelled ctx (the unit stopped) sends
// install.sh SIGTERM, so its EXIT trap can restore the previous files,
// and kills it only after installGrace.
func RunInstall(ctx context.Context, dir string, args []string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{filepath.Join(dir, "install.sh")}, args...)...)
	cmd.Dir = dir
	cmd.Env = fixedEnv("BOT_AGENT_INSTALL_BUNDLE=" + dir)
	cmd.Stdin = nil
	var out bytes.Buffer
	w := io.MultiWriter(&capped{buf: &out, max: maxInstallOut}, os.Stdout)
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = installGrace
	err := cmd.Run()
	if err == nil {
		return 0, out.String(), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode(), out.String(), nil
	}
	return -1, out.String(), err
}

type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// holdLock takes the upgrade lock without waiting; a second upgrade-run
// fails instead of running alongside the first.
func holdLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another upgrade is running")
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
