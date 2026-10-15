package programs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// resolveDir returns the realpath of dir when it sits inside a dirs entry.
// The comparison is on resolved paths, so a prefix sibling
// (/home/joe/workspace-evil) and a symlink that leaves the tree are refused.
func resolveDir(dir string, roots []string) (string, bool) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	real = filepath.Clean(real)
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if within(real, filepath.Clean(rr)) {
			return real, true
		}
	}
	return "", false
}

func within(dir, root string) bool {
	if dir == root {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(dir, root+sep)
}

// resolveExec applies the contract order. A bare name uses
// <dir>/node_modules/.bin/<name> when that path exists and its realpath stays
// inside dir. Otherwise it uses the exec_allow entry with the same basename.
// An absolute path must equal an exec_allow entry, or its realpath must sit
// in the node_modules/.bin tree and inside dir.
// Shell mode always resolves to /bin/sh, which must be listed in exec_allow.
func resolveExec(req request, dir string, p policy.Policy) (string, bool) {
	if req.Shell {
		if !p.AllowShell || !allowEqual("/bin/sh", p.ExecAllow) {
			return "", false
		}
		return "/bin/sh", true
	}
	if strings.HasPrefix(req.Exec, "/") {
		return resolveAbsolute(req.Exec, dir, p.ExecAllow)
	}
	if bin, ok := binInsideDir(dir, req.Exec); ok {
		return bin, true
	}
	for _, allow := range p.ExecAllow {
		if filepath.Base(filepath.Clean(allow)) == req.Exec && filepath.IsAbs(filepath.Clean(allow)) {
			return filepath.Clean(allow), true
		}
	}
	return "", false
}

func resolveAbsolute(exec, dir string, allow []string) (string, bool) {
	clean := filepath.Clean(exec)
	if allowEqual(clean, allow) {
		return clean, true
	}
	// The request path and the resolved dir can differ by an ancestor symlink
	// (/var -> /private/var). Compare the real parent to <dir>/node_modules/.bin,
	// then require the file's own realpath to stay inside dir. A .bin symlink
	// whose target leaves dir fails that second check.
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return "", false
	}
	binRoot := filepath.Join(dir, "node_modules", ".bin")
	if !within(filepath.Clean(parent), binRoot) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(exec)
	if err != nil {
		return "", false
	}
	real = filepath.Clean(real)
	if !within(real, dir) {
		return "", false
	}
	st, err := os.Lstat(real)
	if err != nil || st.IsDir() {
		return "", false
	}
	return real, true
}

func allowEqual(path string, allow []string) bool {
	for _, a := range allow {
		if filepath.Clean(a) == path && filepath.IsAbs(path) {
			return true
		}
	}
	return false
}

// binInsideDir reports the realpath of node_modules/.bin/name when it exists
// and that realpath is inside dir. A symlink that leaves dir is not used.
func binInsideDir(dir, name string) (string, bool) {
	link := filepath.Join(dir, "node_modules", ".bin", name)
	fi, err := os.Lstat(link)
	if err != nil || fi.IsDir() {
		return "", false
	}
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", false
	}
	real = filepath.Clean(real)
	if !within(real, dir) {
		return "", false
	}
	st, err := os.Lstat(real)
	if err != nil || st.IsDir() {
		return "", false
	}
	return real, true
}
