package runner

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// errSymlink refuses a final path component that is a symlink.
// Open and remove use O_NOFOLLOW / Lstat so the target is never followed.
var errSymlink = errors.New("symlink")

// programEnvDir is the EnvironmentFile= directory cited in the unit.
// It is the host path even when Sys.Root prefixes the files tests write.
const programEnvDir = "/var/lib/bot-agent-runner/programs"

// unitNameRE matches the contract's unit names. Identity still comes from
// the markers, not from splitting this pattern (hyphens are ambiguous).
var unitNameRE = regexp.MustCompile(`^deploybot-[a-z0-9-]{1,64}-[a-z0-9-]{1,48}\.service$`)

func joinRoot(root, rel string) string {
	if root == "" || root == "/" {
		return "/" + rel
	}
	return filepath.Join(root, rel)
}

// Persistent program units live under unitDir and programsDir.
// A wave-1 unit may still be under legacyUnitDir, with its env and spec
// in /run/bot-agent/env, until start, restart, or stop moves it.

func unitDir(root string) string {
	return joinRoot(root, filepath.Join("etc", "systemd", "system"))
}

func legacyUnitDir(root string) string {
	return joinRoot(root, filepath.Join("run", "systemd", "system"))
}

func programsDir(root string) string {
	return joinRoot(root, filepath.Join("var", "lib", "bot-agent-runner", "programs"))
}

func unitFile(root, unit string) string {
	return filepath.Join(unitDir(root), unit)
}

func legacyUnitFile(root, unit string) string {
	return filepath.Join(legacyUnitDir(root), unit)
}

func envFile(root, unit string) string {
	return filepath.Join(programsDir(root), unit+".env")
}

func specFile(root, unit string) string {
	return filepath.Join(programsDir(root), unit+".json")
}

func legacyEnvFile(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", "env", unit+".env"))
}

func legacySpecFile(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", "env", unit+".json"))
}

func programPaths(root, unit string) []string {
	return []string{
		unitFile(root, unit),
		envFile(root, unit),
		specFile(root, unit),
		legacyUnitFile(root, unit),
		legacyEnvFile(root, unit),
		legacySpecFile(root, unit),
	}
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
		filepath.Join("var", "lib", "bot-agent-runner", "programs"),
	} {
		if err := ensurePrivateDir(joinRoot(root, rel)); err != nil {
			return err
		}
	}
	return nil
}

// ensurePrivateDir creates dir at 0700 and, when this process is root, root:root.
// A symlink is refused before chmod, which would otherwise follow it.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return errSymlink
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, 0, 0); err != nil {
			return err
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

// renderUnit is the contract's unit file. The EnvironmentFile path is the
// production path even when tests write under a fake root. Env values are
// not placed here. [Install] is what makes systemd start the unit at boot.
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
	b.WriteString("EnvironmentFile=" + programEnvDir + "/" + res.Unit + ".env\n")
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
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
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
	// Rename replaces a symlink. Refuse one instead of swapping it out.
	if err := refuseSymlink(path); err != nil {
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
	seen := map[string]bool{}
	var out []ownedUnit
	for _, dir := range []string{unitDir(root), legacyUnitDir(root)} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !unitNameRE.MatchString(name) || seen[name] {
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			// Claim the name only after the markers match. A foreign file in
			// /etc must not hide an owned /run unit from max_programs. The
			// same name owned in both places is claimed once, here.
			p, en, ok := markersOf(filepath.Join(dir, name))
			if !ok {
				continue
			}
			want, ok := programs.UnitName(p, en)
			if !ok || want != name {
				continue
			}
			seen[name] = true
			out = append(out, ownedUnit{Unit: name, Project: p, Entry: en})
		}
	}
	return out, nil
}

func markersOf(path string) (project, entry string, ok bool) {
	b, err := readNoFollow(path)
	if err != nil {
		return "", "", false
	}
	return markers(string(b))
}

func readNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
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

// unitLoc is where this unit name sits. etc is /etc/systemd/system and
// run is the wave-1 /run/systemd/system copy. A symlink is a flag, not a
// locate error: /etc wins, and the verb decides whether a link is fatal.
type unitLoc struct {
	etcSeen bool
	etcOurs bool
	etcLink bool
	runSeen bool
	runOurs bool
	runLink bool
}

func (l unitLoc) owned() bool { return l.etcOurs || l.runOurs }

// blocked is a same-name file we did not write. A foreign /etc file always
// blocks. A foreign /run file blocks only when /etc does not hold our unit,
// because systemd loads /etc first.
func (l unitLoc) blocked() bool {
	if l.etcSeen && !l.etcOurs {
		return true
	}
	if l.etcOurs {
		return false
	}
	return l.runSeen && !l.runOurs
}

// linkRefused is the symlink that would be the unit systemd loads.
// A /run symlink is not fatal once /etc already has a file.
func (l unitLoc) linkRefused() bool {
	if l.etcLink {
		return true
	}
	return l.runLink && !l.etcSeen && !l.etcOurs
}

func locateUnit(root string, res programs.Resolved) (unitLoc, error) {
	var loc unitLoc
	var err error
	loc.etcSeen, loc.etcOurs, loc.etcLink, err = inspectUnit(unitFile(root, res.Unit), res)
	if err != nil {
		return unitLoc{}, err
	}
	loc.runSeen, loc.runOurs, loc.runLink, err = inspectUnit(legacyUnitFile(root, res.Unit), res)
	if err != nil {
		return unitLoc{}, err
	}
	return loc, nil
}

func inspectUnit(path string, res programs.Resolved) (seen, ours, link bool, err error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, false, true, nil
	}
	if !fi.Mode().IsRegular() {
		return true, false, false, nil
	}
	name, ok := programs.UnitName(res.Project, res.Entry)
	if !ok || name != res.Unit || !unitNameRE.MatchString(res.Unit) {
		return true, false, false, nil
	}
	p, e, ok := markersOf(path)
	if !ok || p != res.Project || e != res.Entry {
		return true, false, false, nil
	}
	return true, true, false, nil
}

// guardMutable refuses a symlink on a path this verb writes or deletes.
// The /run unit is omitted when /etc already has a file: systemd loads that
// file, and a foreign or symlinked /run unit is left untouched.
func guardMutable(root, unit string, loc unitLoc) error {
	paths := []string{
		unitFile(root, unit),
		envFile(root, unit),
		specFile(root, unit),
		legacyEnvFile(root, unit),
		legacySpecFile(root, unit),
	}
	if !loc.etcSeen && !loc.etcOurs {
		paths = append(paths, legacyUnitFile(root, unit))
	}
	for _, path := range paths {
		if err := refuseSymlink(path); err != nil {
			return err
		}
	}
	return nil
}

func refuseSymlink(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	return nil
}

func removeRegular(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return errSymlink
	}
	return os.Remove(path)
}

func removePersistent(root, unit string) error {
	return removeRegulars([]string{
		unitFile(root, unit),
		envFile(root, unit),
		specFile(root, unit),
	})
}

func removeLegacy(root, unit string) error {
	return removeRegulars([]string{
		legacyUnitFile(root, unit),
		legacyEnvFile(root, unit),
		legacySpecFile(root, unit),
	})
}

// removeRegulars tries every path. A symlink or other failure does not
// skip the rest. The first error is what the caller reports.
func removeRegulars(paths []string) error {
	var first error
	for _, path := range paths {
		if err := removeRegular(path); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ownedStopPaths is the files stop may delete. A foreign /run unit is not
// in the list: systemd is using /etc, and that file stays.
func ownedStopPaths(root, unit string, runOurs bool) []string {
	paths := []string{unitFile(root, unit), envFile(root, unit), specFile(root, unit)}
	if runOurs {
		paths = append(paths, legacyUnitFile(root, unit), legacyEnvFile(root, unit), legacySpecFile(root, unit))
	}
	return paths
}

// filesLeft reports a path that is still present, or an Lstat error other
// than a missing file. Stop uses it after removal so a leftover symlink
// is io_error rather than success.
func filesLeft(paths []string) bool {
	for _, path := range paths {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return true
	}
	return false
}

func regularFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// dropInPresent is true when either drop-in path exists, whatever its type.
// The helper does not create or remove these.
func dropInPresent(root, unit string) (bool, error) {
	for _, dir := range []string{unitDir(root), legacyUnitDir(root)} {
		_, err := os.Lstat(filepath.Join(dir, unit+".d"))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

// specOwned is a regular spec whose stored project and entry match.
// A symlink is not followed and is not owned.
func specOwned(path string, res programs.Resolved) bool {
	b, err := readNoFollow(path)
	if err != nil {
		return false
	}
	var meta struct {
		Project string `json:"project"`
		Entry   string `json:"entry"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return false
	}
	return meta.Project != "" && meta.Project == res.Project && meta.Entry == res.Entry
}

// readSpec prefers the persistent spec and falls back to the wave-1 file.
func readSpec(root, unit string) ([]byte, error) {
	b, err := readNoFollow(specFile(root, unit))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return readNoFollow(legacySpecFile(root, unit))
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
