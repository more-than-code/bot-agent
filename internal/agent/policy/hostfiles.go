package policy

import (
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
)

// HostFile is one v1.18 host config file (agent-protocol v1.18 item 1).
// Check is always set; Health and Idle are nil when the policy has none.
// ExecSafe is SafeExec of every command at load time. An unsafe or missing
// executable does not invalidate the policy (spec rev 1a A2): the helper
// refuses the apply, and policy check prints exec=unsafe.
type HostFile struct {
	Name   string
	Path   string
	Dir    string
	File   string
	Mode   uint32
	Secret bool
	Unit   string
	RunAs  Account
	Check  []string
	Health []string
	Idle   []string

	ExecSafe bool
}

// Commands is the file's commands in check, idle, health order.
func (f HostFile) Commands() [][]string {
	out := [][]string{f.Check}
	if f.Idle != nil {
		out = append(out, f.Idle)
	}
	if f.Health != nil {
		out = append(out, f.Health)
	}
	return out
}

// CommandsSafe runs SafeExec on every command's executable now.
func (f HostFile) CommandsSafe(fs ExecFS) bool {
	for _, argv := range f.Commands() {
		if !SafeExec(fs, argv[0]) {
			return false
		}
	}
	return true
}

// HostFile returns the declared file called name.
func (p Policy) HostFile(name string) (HostFile, bool) {
	for _, f := range p.HostFiles {
		if f.Name == name {
			return f, true
		}
	}
	return HostFile{}, false
}

// ExecFS is what the executable check reads. Production uses the host
// filesystem; tests pass a fake so ownership by root can be modelled.
type ExecFS interface {
	Lstat(name string) (os.FileInfo, error)
	Readlink(name string) (string, error)
}

type osFS struct{}

func (osFS) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osFS) Readlink(name string) (string, error)   { return os.Readlink(name) }

const maxLinkHops = 16

// SafeExec resolves exe through symlinks and reports whether the resolved
// target is a root-owned regular executable file that is not group- or
// world-writable, and every directory on the way to it (and on the way to
// each link) is root-owned and not group- or world-writable. Each symlink
// on the walk must itself be root-owned. The root helper runs it again at
// every apply, so a swapped link is checked when it is used.
func SafeExec(fs ExecFS, exe string) bool {
	if fs == nil {
		fs = osFS{}
	}
	if exe == "" || exe[0] != '/' || path.Clean(exe) != exe {
		return false
	}
	parts := splitAbs(exe)
	if len(parts) == 0 {
		return false
	}
	if !safeDir(fs, "/") {
		return false
	}
	cur := "/"
	hops := 0
	for i := 0; i < len(parts); i++ {
		next := path.Join(cur, parts[i])
		fi, err := fs.Lstat(next)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			hops++
			if hops > maxLinkHops {
				return false
			}
			if uid, ok := ownerUID(fi); !ok || uid != 0 {
				return false
			}
			target, err := fs.Readlink(next)
			if err != nil || target == "" {
				return false
			}
			if !strings.HasPrefix(target, "/") {
				target = path.Join(cur, target)
			}
			rest := append(splitAbs(path.Clean(target)), parts[i+1:]...)
			if len(rest) == 0 {
				return false
			}
			parts = rest
			cur = "/"
			i = -1
			continue
		}
		last := i == len(parts)-1
		if !rootOnly(fi) {
			return false
		}
		if last {
			if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
				return false
			}
		} else if !fi.IsDir() {
			return false
		}
		cur = next
	}
	return true
}

func safeDir(fs ExecFS, p string) bool {
	fi, err := fs.Lstat(p)
	return err == nil && fi.IsDir() && rootOnly(fi)
}

func rootOnly(fi os.FileInfo) bool {
	uid, ok := ownerUID(fi)
	return ok && uid == 0 && fi.Mode().Perm()&0o022 == 0
}

func ownerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}

func splitAbs(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// hostFileKeys holds the eight raw arrays from runner.toml.
type hostFileKeys struct {
	files, modes, secret, units, runAs, checks, health, idle []string
}

// validateHostFiles builds the host file list. Any violation is "parse",
// so a typo never half-applies. users is the passwd view; fs is what the
// executable check reads, and its result only sets ExecSafe.
func validateHostFiles(k hostFileKeys, users map[string]Account, fs ExecFS) ([]HostFile, string) {
	if len(k.files) > hostfiles.MaxFiles {
		return nil, "parse"
	}
	out := make([]HostFile, 0, len(k.files))
	index := map[string]int{}
	paths := map[string]bool{}
	for _, item := range k.files {
		name, p, ok := strings.Cut(item, "=")
		if !ok || !hostfiles.ValidName(name) {
			return nil, "parse"
		}
		if _, dup := index[name]; dup {
			return nil, "parse"
		}
		dir, file, ok := hostfiles.ValidPath(p)
		if !ok || paths[p] {
			return nil, "parse"
		}
		paths[p] = true
		index[name] = len(out)
		out = append(out, HostFile{Name: name, Path: p, Dir: dir, File: file})
	}
	// pairs maps "<name>:<value>" items onto the declared files, once each.
	pairs := func(items []string, set func(*HostFile, string) bool) bool {
		seen := map[string]bool{}
		for _, item := range items {
			name, val, ok := strings.Cut(item, ":")
			if !ok || val == "" || seen[name] {
				return false
			}
			seen[name] = true
			i, declared := index[name]
			if !declared || !set(&out[i], val) {
				return false
			}
		}
		return true
	}
	if !pairs(k.modes, func(f *HostFile, v string) bool {
		m, ok := hostfiles.ValidMode(v)
		f.Mode = m
		return ok
	}) {
		return nil, "parse"
	}
	if !pairs(k.units, func(f *HostFile, v string) bool {
		f.Unit = v
		return hostfiles.ValidUnit(v)
	}) {
		return nil, "parse"
	}
	if !pairs(k.runAs, func(f *HostFile, v string) bool {
		if !hostFileRunAsOK(v, users) {
			return false
		}
		f.RunAs = users[v]
		return true
	}) {
		return nil, "parse"
	}
	setCmd := func(get func(*HostFile) *[]string) func(*HostFile, string) bool {
		return func(f *HostFile, v string) bool {
			argv, ok := hostfiles.SplitCommand(v)
			if !ok {
				return false
			}
			*get(f) = argv
			return true
		}
	}
	if !pairs(k.checks, setCmd(func(f *HostFile) *[]string { return &f.Check })) ||
		!pairs(k.health, setCmd(func(f *HostFile) *[]string { return &f.Health })) ||
		!pairs(k.idle, setCmd(func(f *HostFile) *[]string { return &f.Idle })) {
		return nil, "parse"
	}
	seenSecret := map[string]bool{}
	for _, name := range k.secret {
		i, declared := index[name]
		if !declared || seenSecret[name] {
			return nil, "parse"
		}
		seenSecret[name] = true
		out[i].Secret = true
	}
	for i, f := range out {
		if f.Mode == 0 || f.Unit == "" || f.RunAs.Name == "" || len(f.Check) == 0 {
			return nil, "parse"
		}
		// A secret file is never world-readable (rev 1a A6).
		if f.Secret && f.Mode == 0o644 {
			return nil, "parse"
		}
		// A health --wait that cannot fit the settle window (rev 1a A5).
		if f.Health != nil && !hostfiles.ValidHealth(f.Health) {
			return nil, "parse"
		}
		out[i].ExecSafe = f.CommandsSafe(fs)
	}
	return out, ""
}

const (
	// hostFileMinID is the uid and primary gid floor for host_file_run_as
	// (spec A8). It keeps out root and the static system accounts 1-99.
	hostFileMinID = 100
	// nobodyID is the uid of nobody and the gid of nogroup.
	nobodyID = 65534
	// agentAccount is the agent's own account, which never runs host-file commands.
	agentAccount = "bot-agent"
)

// hostFileRunAsOK reports whether name may run a host file's check, idle and
// health commands (spec A8, agent-protocol v1.18.1). Unlike programs'
// filterRunAs it accepts a service account such as dd-worker (uid 994): it
// needs an existing account with uid and primary gid of at least 100, a
// primary group other than root, and not nobody, nogroup or bot-agent (nor
// an alias sharing bot-agent's uid or primary gid).
func hostFileRunAsOK(name string, users map[string]Account) bool {
	ac, ok := users[name]
	if !ok || name == agentAccount || name == "nobody" {
		return false
	}
	if agent, ok := users[agentAccount]; ok && (ac.UID == agent.UID || ac.GID == agent.GID) {
		return false
	}
	if ac.UID < hostFileMinID || ac.UID == nobodyID {
		return false
	}
	// A missing group name is the decimal gid, so "0" is caught here too.
	if ac.GID < hostFileMinID || ac.GID == nobodyID || ac.Group == "root" || ac.Group == "0" || ac.Group == "nogroup" {
		return false
	}
	return true
}
