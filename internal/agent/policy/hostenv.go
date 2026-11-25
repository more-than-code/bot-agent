package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// Host network and host env file limits (agent-protocol v1.21 item 1).
const (
	MaxHostNetworkProjects = 8
	MaxHostEnvFiles        = 32
	// MaxHostEnvBytes bounds one host env file.
	MaxHostEnvBytes = 64 * 1024
)

// ReasonHostEnvFile is the invalid-policy reason for a host_env_files line
// whose item or file fails a check. Policy.ReasonEntry names the entry.
const ReasonHostEnvFile = "host_env_file"

var (
	hostEnvKeyRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	hostEnvPartRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// HostEnvFile is one host_env_files line. Keys and SHA256 are set only when
// the loader read the file (root contexts); the agent leaves them zero.
type HostEnvFile struct {
	Project string
	Entry   string
	Path    string
	// Image is the entry's host_env_images pattern (v1.21 item 7).
	Image string
	// Command and Healthcheck are the entry's v1.26 run pins: the exact
	// argv of host_env_command and host_env_healthcheck, nil without a line
	// (the image's own CMD and HEALTHCHECK).
	Command     []string
	Healthcheck []string
	Keys        int
	SHA256      string
}

// Label is "<project>/<entry>", the name the policy check and the
// invalid reason print.
func (f HostEnvFile) Label() string { return f.Project + "/" + f.Entry }

// HostEnvOwner returns the uid and gid of a file. Production leaves it nil
// and reads the stat; tests set it because they cannot chown to root.
type HostEnvOwner func(os.FileInfo) (uid, gid uint32, ok bool)

// ErrHostEnv is any host env file failure. It never carries a value.
var ErrHostEnv = errors.New("host_env_file")

// HostNetworkAllowed is the v1.21 item 3 gate: a valid policy that lists
// the project in host_network_projects.
func (p Policy) HostNetworkAllowed(project string) bool {
	if !p.Valid {
		return false
	}
	for _, x := range p.HostNetworkProjects {
		if x == project {
			return true
		}
	}
	return false
}

// HostNetworkEnabled is hello programs.host_network (v1.21 item 2).
func (p Policy) HostNetworkEnabled() bool {
	return p.Valid && len(p.HostNetworkProjects) > 0
}

// HostEnv returns the host_env_files line for project/entry.
func (p Policy) HostEnv(project, entry string) (HostEnvFile, bool) {
	if !p.Valid {
		return HostEnvFile{}, false
	}
	for _, f := range p.HostEnvFiles {
		if f.Project == project && f.Entry == entry {
			return f, true
		}
	}
	return HostEnvFile{}, false
}

// validateHostNetwork checks host_network_projects: at most 8 unique slugs,
// each also in projects. Any failure is "parse".
func validateHostNetwork(items, projects []string) ([]string, string) {
	if items == nil {
		return []string{}, ""
	}
	if len(items) > MaxHostNetworkProjects {
		return nil, "parse"
	}
	seen := map[string]bool{}
	for _, slug := range items {
		if seen[slug] || !projectListed(projects, slug) {
			return nil, "parse"
		}
		seen[slug] = true
	}
	return append([]string{}, items...), ""
}

func projectListed(projects []string, slug string) bool {
	for _, p := range projects {
		if p == slug {
			return true
		}
	}
	return false
}

// validateHostEnvFiles checks host_env_files and host_env_images. An item
// that does not split into "<project>/<entry>=<value>", or more than 32
// items of either key, is "parse". Any other failure is ReasonHostEnvFile;
// entry is "<project>/<entry>" when both names are valid, so it is safe to
// print, else "". Every file line needs exactly one image line for the same
// entry, whose pattern is valid and within patterns (v1.21 item 7), and no
// path may be a host_files path or sit in a host_files directory. With read
// set the files themselves are read and checked (policy check); the agent
// and the helper's verbs check the lines only.
//
// cmdItems and healthItems are the v1.26 host_env_command and
// host_env_healthcheck lines, "<project>/<entry>=<argv>": at most one per
// entry, each for an entry with a file line, with a valid argv (a
// healthcheck's first argument absolute).
func validateHostEnvFiles(items, imageItems, cmdItems, healthItems, projects, patterns []string, hostFiles []HostFile, read bool, root string, owner HostEnvOwner) ([]HostEnvFile, string, string) {
	if items == nil && imageItems == nil && cmdItems == nil && healthItems == nil {
		return []HostEnvFile{}, "", ""
	}
	if len(items) > MaxHostEnvFiles || len(imageItems) > MaxHostEnvFiles || len(cmdItems) > MaxHostEnvFiles || len(healthItems) > MaxHostEnvFiles {
		return nil, "parse", ""
	}
	cmdFor, reason, label := hostEnvPins(cmdItems, false)
	if reason != "" {
		return nil, reason, label
	}
	healthFor, reason, label := hostEnvPins(healthItems, true)
	if reason != "" {
		return nil, reason, label
	}
	imagesFor := map[string]string{}
	for _, item := range imageItems {
		project, entry, pattern, ok := splitEnvItem(item)
		if !ok {
			return nil, "parse", ""
		}
		label := envLabel(project, entry)
		if _, dup := imagesFor[project+"/"+entry]; dup || label == "" || !images.ValidPattern(pattern) || !images.PatternWithin(pattern, patterns) {
			return nil, ReasonHostEnvFile, label
		}
		imagesFor[project+"/"+entry] = pattern
	}
	out := make([]HostEnvFile, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		project, entry, path, ok := splitEnvItem(item)
		if !ok {
			return nil, "parse", ""
		}
		label := envLabel(project, entry)
		if label == "" || !projectListed(projects, project) || seen[label] || !ValidHostEnvPath(path) || overlapsHostFiles(path, hostFiles) {
			return nil, ReasonHostEnvFile, label
		}
		pattern, bound := imagesFor[label]
		if !bound {
			return nil, ReasonHostEnvFile, label
		}
		seen[label] = true
		f := HostEnvFile{Project: project, Entry: entry, Path: path, Image: pattern, Command: cmdFor[label], Healthcheck: healthFor[label]}
		if read {
			env, sum, err := ReadHostEnv(root, path, owner)
			if err != nil {
				return nil, ReasonHostEnvFile, label
			}
			f.Keys = len(env)
			f.SHA256 = sum
			clearEnv(env)
		}
		out = append(out, f)
	}
	// An image or pin line without a file line is a typo, not a no-op.
	for _, lines := range []map[string]string{imagesFor, keysOf(cmdFor), keysOf(healthFor)} {
		for label := range lines {
			if !seen[label] {
				return nil, ReasonHostEnvFile, label
			}
		}
	}
	return out, "", ""
}

// Host env run pin limits (agent-protocol v1.26 item 1), the same argv
// shape as the host_file commands.
const (
	MaxHostEnvArgs     = 17
	MaxHostEnvArgBytes = 256
)

// hostEnvPins parses v1.26 pin lines into argv by entry label. A line that
// does not split is "parse"; a bad label, argv or repeat is
// ReasonHostEnvFile with the label (or "" when the names are invalid).
// abs requires the first argument to be an absolute path (healthcheck).
func hostEnvPins(items []string, abs bool) (map[string][]string, string, string) {
	out := map[string][]string{}
	for _, item := range items {
		project, entry, argv, ok := splitEnvItem(item)
		if !ok {
			return nil, "parse", ""
		}
		label := envLabel(project, entry)
		if _, dup := out[label]; dup || label == "" {
			return nil, ReasonHostEnvFile, label
		}
		args, ok := splitPinArgv(argv, abs)
		if !ok {
			return nil, ReasonHostEnvFile, label
		}
		out[label] = args
	}
	return out, "", ""
}

// splitPinArgv splits on single spaces: 1 to 17 arguments (a program and
// at most 16 arguments), each 1 to 256 bytes of printable ASCII without
// space, quotes or backslash. No shell is ever involved.
func splitPinArgv(s string, abs bool) ([]string, bool) {
	args := strings.Split(s, " ")
	if len(args) > MaxHostEnvArgs {
		return nil, false
	}
	for _, a := range args {
		if a == "" || len(a) > MaxHostEnvArgBytes {
			return nil, false
		}
		for i := 0; i < len(a); i++ {
			c := a[i]
			if c <= ' ' || c > '~' || c == '"' || c == '\'' || c == '\\' {
				return nil, false
			}
		}
	}
	if abs && !strings.HasPrefix(args[0], "/") {
		return nil, false
	}
	return args, true
}

func keysOf(m map[string][]string) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = ""
	}
	return out
}

// splitEnvItem splits "<project>/<entry>=<value>".
func splitEnvItem(item string) (project, entry, value string, ok bool) {
	name, value, ok := strings.Cut(item, "=")
	if !ok {
		return "", "", "", false
	}
	project, entry, ok = strings.Cut(name, "/")
	return project, entry, value, ok
}

// envLabel is "<project>/<entry>" when both names are valid, else "".
func envLabel(project, entry string) string {
	if images.ValidProject(project) && images.ValidEntry(entry) {
		return project + "/" + entry
	}
	return ""
}

// overlapsHostFiles reports a host env path that is a host_files path or
// sits in a host_files directory (v1.21 item 7): the console could
// otherwise write the file the helper merges into containers.
func overlapsHostFiles(path string, hostFiles []HostFile) bool {
	for _, hf := range hostFiles {
		if hf.Path == path || filepath.Dir(path) == filepath.Dir(hf.Path) {
			return true
		}
	}
	return false
}

// HostEnvImageAllowed reports whether image (a reference, optionally with
// a digest) matches the entry's host_env_images pattern.
func (f HostEnvFile) HostEnvImageAllowed(ref images.Reference) bool {
	return f.Image != "" && images.Match(ref, []string{f.Image})
}

// ValidHostEnvPath is /etc/<dir>/<name>.env: absolute, exactly one
// directory under /etc, no "..", and plain names.
func ValidHostEnvPath(p string) bool {
	parts := strings.Split(p, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "etc" {
		return false
	}
	dir, file := parts[2], parts[3]
	if !hostEnvPartRE.MatchString(dir) || !hostEnvPartRE.MatchString(file) {
		return false
	}
	if strings.Contains(dir, "..") || strings.Contains(file, "..") {
		return false
	}
	return strings.HasSuffix(file, ".env") && len(file) > len(".env")
}

// ReadHostEnv opens path (under root, a filesystem prefix that only tests
// set; "" is the host) without following a link and checks it: a regular
// file, owned by root:root, mode exactly 0600, one link, at most 64 KiB,
// made of KEY=VALUE lines (blank lines and # comments allowed, no duplicate
// keys). It returns the values and the file's SHA-256 hex. On any failure
// the map is nil and the error is ErrHostEnv; no value is in the error.
// Callers must not log the map and should clear it when done.
func ReadHostEnv(root, path string, owner HostEnvOwner) (map[string]string, string, error) {
	if !ValidHostEnvPath(path) {
		return nil, "", ErrHostEnv
	}
	if root != "" && root != "/" {
		path = filepath.Join(root, path)
	}
	// v1.21 item 7: /etc and the file's directory are root-owned and not
	// group- or world-writable, and neither is a link.
	if !safeEnvDir(filepath.Dir(filepath.Dir(path)), owner) || !safeEnvDir(filepath.Dir(path), owner) {
		return nil, "", ErrHostEnv
	}
	li, err := os.Lstat(path)
	if err != nil || !li.Mode().IsRegular() {
		return nil, "", ErrHostEnv
	}
	fh, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", ErrHostEnv
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return nil, "", ErrHostEnv
	}
	if fi.Mode() != 0o600 || fi.Size() > MaxHostEnvBytes {
		return nil, "", ErrHostEnv
	}
	uid, gid, nlink, ok := statIDs(fi)
	if owner != nil {
		uid, gid, ok = owner(fi)
	}
	if !ok || uid != 0 || gid != 0 || nlink != 1 {
		return nil, "", ErrHostEnv
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxHostEnvBytes+1))
	if err != nil || len(data) > MaxHostEnvBytes {
		clear(data)
		return nil, "", ErrHostEnv
	}
	env, ok := parseHostEnv(data)
	sum := sha256.Sum256(data)
	clear(data)
	if !ok {
		return nil, "", ErrHostEnv
	}
	return env, hex.EncodeToString(sum[:]), nil
}

// safeEnvDir is a real directory (not a link), owned by root, and not
// group- or world-writable. owner replaces the ids in tests.
func safeEnvDir(dir string, owner HostEnvOwner) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		return false
	}
	uid, _, _, ok := statIDs(fi)
	if owner != nil {
		uid, _, ok = owner(fi)
	}
	return ok && uid == 0
}

// statIDs is the owner and link count from the stat. nlink is read here
// even when an owner hook replaces the ids, so tests still check it.
func statIDs(fi os.FileInfo) (uid, gid uint32, nlink uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0, false
	}
	return st.Uid, st.Gid, uint64(st.Nlink), true
}

// parseHostEnv reads KEY=VALUE lines. The value is everything after the
// first "=", taken as is (no quotes or escapes). A carriage return, a NUL,
// invalid UTF-8, a bad key or a repeated key fails the file.
func parseHostEnv(data []byte) (map[string]string, bool) {
	text := string(data)
	if strings.ContainsAny(text, "\x00\r") || !utf8.ValidString(text) {
		return nil, false
	}
	env := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || !hostEnvKeyRE.MatchString(key) {
			clearEnv(env)
			return nil, false
		}
		if _, dup := env[key]; dup {
			clearEnv(env)
			return nil, false
		}
		env[key] = val
	}
	return env, true
}

// HostEnvKeyDenied is the env denylist for a host env file key: the keys an
// image env may not set either (PATH, HOME, and the runtime keys).
func HostEnvKeyDenied(key string) bool {
	return key == "PATH" || key == "HOME" || images.ChangesRuntime(key)
}

func clearEnv(env map[string]string) {
	for k := range env {
		env[k] = ""
		delete(env, k)
	}
}
