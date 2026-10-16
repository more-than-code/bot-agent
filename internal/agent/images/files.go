package images

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxFiles     = 16
	maxFileBytes = 65536
	// FileStoreDir is the production store. Mount Source is always this
	// path plus the slot and the name. Tests may write somewhere else;
	// Docker still receives this string, which the helper builds itself.
	FileStoreDir = "/var/lib/bot-agent-runner/files"
)

var fileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// SpecFile is one api-v1 v1.9 spec.files row. Content is not in the spec.
type SpecFile struct {
	File   string `json:"file"`
	Target string `json:"target"`
}

// MountFile is one container_deploy file. There is no secret field:
// the helper request must not carry it.
type MountFile struct {
	Name    string `json:"name"`
	Target  string `json:"target"`
	Content string `json:"content"`
}

// TaskFile is one image_deploy / image_rollback file. Secret selects
// exact-value redaction. Omitted secret is false (a plain file).
type TaskFile struct {
	Name    string `json:"name"`
	Target  string `json:"target"`
	Content string `json:"content"`
	Secret  bool   `json:"secret"`
}

// ValidFileName is api-v1 v1.9 rule 1. The pattern already excludes "." and "..".
func ValidFileName(s string) bool {
	return fileNameRE.MatchString(s)
}

// ValidFileContent is api-v1 v1.9 rule 2: UTF-8, 1–65536 bytes, no NUL.
func ValidFileContent(s string) bool {
	if s == "" || len(s) > maxFileBytes || strings.IndexByte(s, 0) >= 0 {
		return false
	}
	return utf8.ValidString(s)
}

// validFileTarget is rule 13's target half. Volume targets stay on validTarget,
// which still allows "/".
func validFileTarget(s string) bool {
	if s == "/" || !validTarget(s) {
		return false
	}
	for _, root := range []string{"/proc", "/sys", "/dev"} {
		if s == root || strings.HasPrefix(s, root+"/") {
			return false
		}
	}
	return true
}

// strictlyUnder reports whether child is a proper path descendant of parent.
// "/var/data" is not under "/var/data2". A volume target of "/" covers every
// other absolute path.
func strictlyUnder(child, parent string) bool {
	if parent == "/" {
		return child != "/" && strings.HasPrefix(child, "/")
	}
	return strings.HasPrefix(child, parent+"/")
}

// checkSpecFiles is the spec half of rule 13: names, targets, duplicates,
// and volume overlap. Content is checked on the resolved list.
func checkSpecFiles(files []SpecFile, vols []Volume) string {
	if len(files) > maxFiles {
		return "files"
	}
	names := map[string]bool{}
	targets := map[string]bool{}
	for _, v := range vols {
		targets[v.Target] = true
	}
	for _, f := range files {
		if !ValidFileName(f.File) || !validFileTarget(f.Target) || names[f.File] || targets[f.Target] {
			return "files"
		}
		for _, v := range vols {
			if strictlyUnder(f.Target, v.Target) || strictlyUnder(v.Target, f.Target) {
				return "files"
			}
		}
		names[f.File] = true
		targets[f.Target] = true
	}
	return ""
}

// CheckResolved is the one-for-one check plus rule 2. spec and files are in
// the same order. Names and targets must already have passed checkSpecFiles;
// a length or content mismatch is still "files".
func CheckResolved(spec []SpecFile, files []MountFile) string {
	if len(spec) != len(files) {
		return "files"
	}
	seen := map[string]bool{}
	for i := range spec {
		f := files[i]
		if f.Name != spec[i].File || f.Target != spec[i].Target || seen[f.Name] {
			return "files"
		}
		if !ValidFileName(f.Name) || !validFileTarget(f.Target) || !ValidFileContent(f.Content) {
			return "files"
		}
		seen[f.Name] = true
	}
	return ""
}

// FilesHash is rule 8. Empty files produce "" and the label is omitted.
// Lines are "<name>:<hex sha256 of content>\n", sorted by name.
func FilesHash(files []MountFile) string {
	if len(files) == 0 {
		return ""
	}
	cp := append([]MountFile(nil), files...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Name < cp[j].Name })
	h := sha256.New()
	for _, f := range cp {
		sum := sha256.Sum256([]byte(f.Content))
		_, _ = h.Write([]byte(f.Name))
		_, _ = h.Write([]byte(":"))
		_, _ = h.Write([]byte(hex.EncodeToString(sum[:])))
		_, _ = h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MountSource is the bind source Docker receives. It is not an OS open.
// project, entry, and name must already be validated tokens.
func MountSource(project, entry, name string) (string, bool) {
	slot, ok := ContainerName(project, entry)
	if !ok || !ValidFileName(name) {
		return "", false
	}
	return FileStoreDir + "/" + slot + "/" + name, true
}

// AsMount drops the secret flag. The helper request has no secret key.
func AsMount(files []TaskFile) []MountFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]MountFile, len(files))
	for i, f := range files {
		out[i] = MountFile{Name: f.Name, Target: f.Target, Content: f.Content}
	}
	return out
}
