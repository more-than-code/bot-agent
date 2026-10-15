package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// unitNameRE matches the contract's unit names. Identity still comes from
// the markers, not from splitting this pattern (hyphens are ambiguous).
var unitNameRE = regexp.MustCompile(`^deploybot-[a-z0-9-]{1,64}-[a-z0-9-]{1,48}\.service$`)

func joinRoot(root, rel string) string {
	if root == "" || root == "/" {
		return "/" + rel
	}
	return filepath.Join(root, rel)
}

func unitFile(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "systemd", "system", unit))
}

func envFile(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", "env", unit+".env"))
}

func specFile(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", "env", unit+".json"))
}

// ensureRuntime creates the helper's state directories at 0700 root:root
// when this process is root. systemd-tmpfiles does the same at boot. The
// helper still creates them so a start works when tmpfiles has not run and
// the directory is creatable (the unit's ReadWritePaths entry is marked
// optional because a missing path fails namespace setup).
func ensureRuntime(root string) error {
	for _, rel := range []string{
		filepath.Join("run", "bot-agent"),
		filepath.Join("run", "bot-agent", "env"),
	} {
		dir := joinRoot(root, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(dir, 0, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordSpec is the validated start request stored next to the env file.
// Restart reads it back and runs it through Check against the current policy.
func recordSpec(res programs.Resolved) ([]byte, error) {
	args := res.Args
	if args == nil {
		args = []string{}
	}
	env := res.Env
	if env == nil {
		env = map[string]string{}
	}
	return json.Marshal(struct {
		Project string            `json:"project"`
		Entry   string            `json:"entry"`
		Dir     string            `json:"dir"`
		Exec    string            `json:"exec"`
		Args    []string          `json:"args"`
		Shell   bool              `json:"shell"`
		RunAs   string            `json:"run_as"`
		Env     map[string]string `json:"env"`
		Restart string            `json:"restart"`
	}{
		Project: res.Project, Entry: res.Entry, Dir: res.Dir, Exec: res.Exec,
		Args: args, Shell: res.Shell, RunAs: res.RunAs, Env: env, Restart: res.Restart,
	})
}

func unitDir(root string) string {
	return joinRoot(root, filepath.Join("run", "systemd", "system"))
}

// renderUnit is the contract's unit file. The EnvironmentFile path is the
// production path even when tests write under a fake root. Env values are
// not placed here.
func renderUnit(res programs.Resolved) string {
	dir := programs.QuotePath(res.Dir)
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("X-Deploybot-Project=" + res.Project + "\n")
	b.WriteString("X-Deploybot-Entry=" + res.Entry + "\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("User=" + res.RunAs + "\n")
	b.WriteString("Group=" + res.Group + "\n")
	// WorkingDirectory takes the rest of the line as the path; systemd does not
	// unquote it. Only specifier expansion applies, so escape % alone.
	b.WriteString("WorkingDirectory=" + strings.ReplaceAll(res.Dir, "%", "%%") + "\n")
	b.WriteString("ExecStart=" + programs.ExecStart(res.Exec, res.Args) + "\n")
	// Environment= quotes the whole KEY=value assignment.
	b.WriteString("Environment=" + programs.QuotePath("HOME="+res.Home) + "\n")
	b.WriteString("Environment=" + programs.QuotePath("PATH="+res.Dir+"/node_modules/.bin:/usr/local/bin:/usr/bin:/bin") + "\n")
	b.WriteString("EnvironmentFile=/run/bot-agent/env/" + res.Unit + ".env\n")
	b.WriteString("Restart=" + res.Restart + "\n")
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("PrivateTmp=yes\n")
	b.WriteString("ProtectSystem=full\n")
	b.WriteString("ProtectHome=read-only\n")
	b.WriteString("ReadWritePaths=" + dir + "\n")
	b.WriteString("PrivateDevices=yes\n")
	b.WriteString("ProtectKernelTunables=yes\n")
	b.WriteString("ProtectKernelModules=yes\n")
	b.WriteString("ProtectControlGroups=yes\n")
	// Engine sockets are root-equivalent. Hide them even when run_as is in group docker.
	// "-" makes each optional: a missing path would otherwise fail namespace setup (226/NAMESPACE).
	b.WriteString("InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/containerd/containerd.sock -/run/podman/podman.sock\n")
	b.WriteString("RestrictSUIDSGID=yes\n")
	b.WriteString("LockPersonality=yes\n")
	return b.String()
}

func renderEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(programs.QuoteEnv(env[k]))
		b.WriteByte('\n')
	}
	return b.String()
}

// atomicWrite replaces path via a temp file in the same directory.
// The mode is set before the bytes are written. When this process is root,
// the temp file is chowned to root before the write.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".bot-agent-runner-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if os.Geteuid() == 0 {
		if err := f.Chown(0, 0); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return os.Chmod(path, mode)
}

type ownedUnit struct {
	Unit    string
	Project string
	Entry   string
}

func readOwned(root string) ([]ownedUnit, error) {
	dir := unitDir(root)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []ownedUnit
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !unitNameRE.MatchString(name) {
			continue
		}
		p, en, ok := markersOf(unitFile(root, name))
		if !ok {
			continue
		}
		want, ok := programs.UnitName(p, en)
		if !ok || want != name {
			continue
		}
		out = append(out, ownedUnit{Unit: name, Project: p, Entry: en})
	}
	return out, nil
}

func markersOf(path string) (project, entry string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	return markers(string(b))
}

func markers(body string) (project, entry string, ok bool) {
	var haveP, haveE bool
	for _, line := range strings.Split(body, "\n") {
		if v, found := strings.CutPrefix(line, "X-Deploybot-Project="); found {
			if haveP {
				return "", "", false
			}
			project = v
			haveP = true
		}
		if v, found := strings.CutPrefix(line, "X-Deploybot-Entry="); found {
			if haveE {
				return "", "", false
			}
			entry = v
			haveE = true
		}
	}
	if !haveP || !haveE || project == "" || entry == "" {
		return "", "", false
	}
	return project, entry, true
}

// createdByUs is true only when the unit file exists and both markers match
// the request. A missing file is not created by us.
func createdByUs(root string, res programs.Resolved) bool {
	name, ok := programs.UnitName(res.Project, res.Entry)
	if !ok || name != res.Unit || !unitNameRE.MatchString(res.Unit) {
		return false
	}
	p, e, ok := markersOf(unitFile(root, res.Unit))
	return ok && p == res.Project && e == res.Entry
}

func fileMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// lockHelper takes an exclusive flock on run/bot-agent/helper.lock and returns
// the release function. It blocks until any other helper process releases it.
func lockHelper(root string) (func(), error) {
	path := joinRoot(root, filepath.Join("run", "bot-agent", "helper.lock"))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
