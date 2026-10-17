// Package policy reads the root-owned host policy at /etc/bot-agent/runner.toml.
// The agent loads it once at start-up. The root helper loads it on every call.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const (
	// DefaultPath is the only path production code reads.
	// Tests pass Files.Policy.
	DefaultPath   = "/etc/bot-agent/runner.toml"
	DefaultPasswd = "/etc/passwd"
	DefaultGroup  = "/etc/group"

	// MaxBytes bounds a policy file. Larger files are invalid.
	MaxBytes = 64 * 1024

	minUID = 1000
)

// Account is a kept run_as user. Home and Group come from passwd and group.
type Account struct {
	Name  string
	UID   uint32
	GID   uint32
	Home  string
	Group string
}

// Drop is a run_as entry the policy listed and the loader rejected.
type Drop struct {
	Name   string
	Reason string
}

// Policy is the host policy. When Valid is false the effective Enabled flag
// is false and RunAs, Dirs, and ExecAllow are empty. Hash is the hex SHA-256
// of the file bytes when those bytes were read.
type Policy struct {
	Exists           bool
	Valid            bool
	Enabled          bool
	AllowShell       bool
	MaxPrograms      int
	RunAs            []string
	Dirs             []string
	ExecAllow        []string
	Images           []string
	Projects         []string
	MaxContainers    int
	AllowPublicPorts bool
	Accounts         map[string]Account
	Hash             string
	Reason           string
	Dropped          []Drop
}

// ImagesEnabled is the hello flag: a valid enabled policy with at least one
// image pattern and one project, and no overlapping project names. An empty
// list is valid and leaves images off. Overlap leaves the policy valid.
func (p Policy) ImagesEnabled() bool {
	if _, _, overlap := OverlappingProjects(p.Projects); overlap {
		return false
	}
	return p.Valid && p.Enabled && len(p.Images) > 0 && len(p.Projects) > 0
}

// OverlappingProjects reports the first pair, in list order, where one slug
// is the other plus "-" plus one or more characters. a is the earlier slug.
// A bare trailing "-" is not a suffix: the contract's "anything" is not empty.
// acme and acmeweb do not overlap. acme-web and web do not overlap.
func OverlappingProjects(projects []string) (a, b string, ok bool) {
	for i := 0; i < len(projects); i++ {
		for j := i + 1; j < len(projects); j++ {
			if projectOverlaps(projects[i], projects[j]) {
				return projects[i], projects[j], true
			}
		}
	}
	return "", "", false
}

func projectOverlaps(x, y string) bool {
	return (strings.HasPrefix(y, x+"-") && len(y) > len(x)+1) ||
		(strings.HasPrefix(x, y+"-") && len(x) > len(y)+1)
}

// Files selects the policy and the account databases.
// UID, when set, replaces the uid from the file's stat. Production leaves it
// nil. Tests set it because this process cannot chown a file to root.
// The override does not affect the mode check.
type Files struct {
	Policy string
	Passwd string
	Group  string
	UID    func(os.FileInfo) (uint32, bool)
}

// Load reads the policy. A missing file returns a zero Policy (Exists false).
// It does not return an error: a missing or invalid policy disables programs.
func Load(f Files) Policy {
	path := f.Policy
	if path == "" {
		path = DefaultPath
	}
	passwd := f.Passwd
	if passwd == "" {
		passwd = DefaultPasswd
	}
	group := f.Group
	if group == "" {
		group = DefaultGroup
	}
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Policy{}
		}
		return Policy{Exists: true, Reason: "unreadable"}
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return Policy{Exists: true, Reason: "unreadable"}
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxBytes+1))
	if err != nil {
		return Policy{Exists: true, Reason: "unreadable"}
	}
	out := Policy{Exists: true, Hash: hashBytes(data)}
	if len(data) > MaxBytes || fi.IsDir() {
		out.Reason = "parse"
		return out
	}
	uid, ok := fileUID(fi, f.UID)
	if !ok || uid != 0 {
		out.Reason = "not_root"
		return out
	}
	perm := fi.Mode().Perm()
	if perm&0o020 != 0 {
		out.Reason = "group_writable"
		return out
	}
	if perm&0o002 != 0 {
		out.Reason = "world_writable"
		return out
	}
	parsed, reason := parseTOML(data)
	if reason != "" {
		out.Reason = reason
		return out
	}
	if reason := validateImagePolicy(&parsed); reason != "" {
		out.Reason = reason
		return out
	}
	users := readAccounts(passwd, group)
	kept, dropped := filterRunAs(parsed.runAs, users)
	out.Valid = true
	out.Enabled = parsed.enabled
	out.AllowShell = parsed.allowShell
	out.MaxPrograms = parsed.maxPrograms
	out.RunAs = kept
	out.Dirs = parsed.dirs
	out.ExecAllow = parsed.execAllow
	out.Images = parsed.images
	out.Projects = parsed.projects
	out.MaxContainers = parsed.maxContainers
	out.AllowPublicPorts = parsed.allowPublic
	out.Accounts = map[string]Account{}
	for _, name := range kept {
		out.Accounts[name] = users[name]
	}
	out.Dropped = dropped
	if out.RunAs == nil {
		out.RunAs = []string{}
	}
	return out
}

// validateImagePolicy checks patterns, project slugs, and max_containers.
// A bad value invalidates the whole file. Absent max_containers is 20.
func validateImagePolicy(p *parsed) string {
	if p.images == nil {
		p.images = []string{}
	}
	if p.projects == nil {
		p.projects = []string{}
	}
	for _, pat := range p.images {
		if !images.ValidPattern(pat) {
			return "parse"
		}
	}
	seen := map[string]bool{}
	for _, proj := range p.projects {
		if !images.ValidProject(proj) || seen[proj] {
			return "parse"
		}
		seen[proj] = true
	}
	if !p.maxSet {
		p.maxContainers = 20
	} else if p.maxContainers < 1 || p.maxContainers > 200 {
		return "parse"
	}
	return ""
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileUID(fi os.FileInfo, override func(os.FileInfo) (uint32, bool)) (uint32, bool) {
	if override != nil {
		return override(fi)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}

func readAccounts(passwdPath, groupPath string) map[string]Account {
	pb, err := os.ReadFile(passwdPath)
	if err != nil {
		return map[string]Account{}
	}
	gb, _ := os.ReadFile(groupPath)
	groups := parseGroups(string(gb))
	return parsePasswd(string(pb), groups)
}

func parseGroups(data string) map[uint32]string {
	out := map[uint32]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		id := uint32(gid)
		if _, seen := out[id]; !seen {
			out[id] = f[0]
		}
	}
	return out
}

func parsePasswd(data string, groups map[uint32]string) map[string]Account {
	out := map[string]Account{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 7 || f[0] == "" {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		gid, err := strconv.ParseUint(f[3], 10, 32)
		if err != nil {
			continue
		}
		if _, seen := out[f[0]]; seen {
			continue
		}
		g := groups[uint32(gid)]
		if g == "" {
			g = strconv.FormatUint(gid, 10)
		}
		out[f[0]] = Account{
			Name: f[0], UID: uint32(uid), GID: uint32(gid), Home: f[5], Group: g,
		}
	}
	return out
}

// filterRunAs drops root, unknown names, uid below 1000, and a primary gid
// below 1000 (including gid 0). The kept names stay in file order.
func filterRunAs(names []string, users map[string]Account) (kept []string, dropped []Drop) {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if name == "root" {
			dropped = append(dropped, Drop{Name: name, Reason: "root"})
			continue
		}
		ac, ok := users[name]
		if !ok {
			dropped = append(dropped, Drop{Name: name, Reason: "missing"})
			continue
		}
		if ac.UID < minUID {
			dropped = append(dropped, Drop{Name: name, Reason: "uid"})
			continue
		}
		// gid 0, or a primary group below the same floor, is not a run_as account.
		// A missing group name is the decimal gid, so "0" is caught here too.
		if ac.GID < minUID || ac.Group == "root" || ac.Group == "0" {
			dropped = append(dropped, Drop{Name: name, Reason: "gid"})
			continue
		}
		kept = append(kept, name)
	}
	if kept == nil {
		kept = []string{}
	}
	return kept, dropped
}
