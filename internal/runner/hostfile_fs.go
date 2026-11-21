package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// hostTarget is an open O_NOFOLLOW handle on /etc/<dir> of one host file
// (agent-protocol v1.18). Every later name is one component passed to an
// *at call, so a symlink swapped in for the directory after the open is
// never followed.
type hostTarget struct {
	dirfd  int
	file   string
	mode   uint32
	uid    int
	gid    int
	altGID int // the run_as primary group, allowed for a 0640 file; -1 otherwise
	// dirSync fsyncs the directory after a rename. Tests inject a failure.
	dirSync func(fd int) error
}

// openHostTarget walks <root>/etc/<dir> with O_NOFOLLOW. /etc and the
// directory must be root-owned directories that are not group- or
// world-writable. The reason is a hostfiles.Status* token. The expected
// owner is the store's (sys.fileIDs): 0:0 in production.
func openHostTarget(sys Sys, hf policy.HostFile) (*hostTarget, string) {
	uid, gid := sys.fileIDs()
	root := sys.Root
	if root == "" {
		root = "/"
	}
	rfd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, hostfiles.StatusIO
	}
	parent := rfd
	for _, name := range []string{"etc", hf.Dir} {
		fd, err := openat(parent, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(parent)
		if err != nil {
			switch {
			case errors.Is(err, syscall.ENOENT):
				return nil, hostfiles.StatusMissing
			case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
				return nil, hostfiles.StatusDir
			default:
				return nil, hostfiles.StatusIO
			}
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			_ = syscall.Close(fd)
			return nil, hostfiles.StatusIO
		}
		if !isDir(st) || int(st.Uid) != uid || permOf(st)&0o022 != 0 {
			_ = syscall.Close(fd)
			return nil, hostfiles.StatusDir
		}
		parent = fd
	}
	t := &hostTarget{dirfd: parent, file: hf.File, mode: hf.Mode, uid: uid, gid: gid, altGID: -1, dirSync: syscall.Fsync}
	if sys.hostFault != nil {
		t.dirSync = func(fd int) error {
			if err := sys.hostFault("dir_fsync"); err != nil {
				return err
			}
			return syscall.Fsync(fd)
		}
	}
	if hf.Mode == 0o640 {
		t.altGID = int(hf.RunAs.GID)
	}
	return t, ""
}

func (t *hostTarget) close() {
	if t != nil && t.dirfd >= 0 {
		_ = syscall.Close(t.dirfd)
		t.dirfd = -1
	}
}

// checkStat is the file rule: a regular file, the expected owner, group
// root (or the run_as group for 0640), exactly the policy mode, nlink 1.
func (t *hostTarget) checkStat(st syscall.Stat_t) string {
	switch {
	case isLnk(st):
		return hostfiles.StatusSymlink
	case !isReg(st):
		return hostfiles.StatusNotRegular
	case int(st.Uid) != t.uid || (int(st.Gid) != t.gid && int(st.Gid) != t.altGID):
		return hostfiles.StatusOwner
	case permOf(st) != t.mode:
		return hostfiles.StatusMode
	case nlinkOf(st) != 1:
		return hostfiles.StatusNlink
	}
	return ""
}

// read opens the target with O_NOFOLLOW, checks it again on the open fd,
// and reads at most MaxContent bytes. It returns the group the file has.
func (t *hostTarget) read() ([]byte, int, string) {
	st, err := fstatAt(t.dirfd, t.file)
	if errors.Is(err, syscall.ENOENT) {
		return nil, 0, hostfiles.StatusMissing
	}
	if err != nil {
		return nil, 0, hostfiles.StatusIO
	}
	if reason := t.checkStat(st); reason != "" {
		return nil, 0, reason
	}
	fd, err := openat(t.dirfd, t.file, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, 0, hostfiles.StatusSymlink
		}
		return nil, 0, hostfiles.StatusIO
	}
	f := os.NewFile(uintptr(fd), t.file)
	defer f.Close()
	var fst syscall.Stat_t
	if err := syscall.Fstat(fd, &fst); err != nil {
		return nil, 0, hostfiles.StatusIO
	}
	if reason := t.checkStat(fst); reason != "" {
		return nil, 0, reason
	}
	data, err := io.ReadAll(io.LimitReader(f, hostfiles.MaxContent+1))
	if err != nil {
		return nil, 0, hostfiles.StatusIO
	}
	if len(data) > hostfiles.MaxContent {
		return nil, 0, hostfiles.StatusTooLarge
	}
	return data, int(fst.Gid), ""
}

func (t *hostTarget) candidate() string {
	return "." + t.file + hostfiles.CandidateSuffix
}

// writeCandidate removes a leftover candidate, then creates a new one with
// O_CREAT|O_EXCL|O_NOFOLLOW, root and the policy mode, writes and fsyncs.
// The mode is set before the bytes are written.
func (t *hostTarget) writeCandidate(data []byte, gid int) error {
	if err := t.removeCandidate(); err != nil {
		return err
	}
	fd, err := openat(t.dirfd, t.candidate(), syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_WRONLY|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Fchown(fd, t.uid, gid); err != nil {
		return err
	}
	if err := syscall.Fchmod(fd, t.mode); err != nil {
		return err
	}
	if err := writeFull(fd, data); err != nil {
		return err
	}
	return syscall.Fsync(fd)
}

// openCandidate is the check command's stdin.
func (t *hostTarget) openCandidate() (*os.File, error) {
	fd, err := openat(t.dirfd, t.candidate(), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), t.candidate()), nil
}

// removeCandidate unlinks the candidate name. A missing one is fine; a
// directory there is refused (unlinkat without AT_REMOVEDIR fails on it).
func (t *hostTarget) removeCandidate() error {
	err := unlinkat(t.dirfd, t.candidate(), 0)
	if err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

// swap renames the candidate over the target and fsyncs the directory.
// renamed is true once renameat has succeeded, also when the fsync that
// follows fails: the new bytes are in place either way.
func (t *hostTarget) swap() (renamed bool, err error) {
	if err := renameat(t.dirfd, t.candidate(), t.dirfd, t.file); err != nil {
		return false, err
	}
	return true, t.dirSync(t.dirfd)
}

// candidateKind reports whether a candidate name exists and is a directory.
func (t *hostTarget) candidateKind() (exists, isDirectory bool, err error) {
	st, err := fstatAt(t.dirfd, t.candidate())
	if errors.Is(err, syscall.ENOENT) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, isDir(st), nil
}

// hostStateDir is /var/lib/bot-agent-runner/host-files, root 0700.
func hostStateDir(sys Sys) (string, error) {
	dir := joinRoot(sys.Root, strings.TrimPrefix(hostfiles.StateDir, "/"))
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func writeHostState(sys Sys, name, suffix string, data []byte) error {
	dir, err := hostStateDir(sys)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, name+suffix), data, 0o600)
}

// readApplied returns the recorded sha256, "" when nothing was applied yet.
// The state directory is not created by a read.
func readApplied(sys Sys, name string) (string, error) {
	p := joinRoot(sys.Root, strings.TrimPrefix(hostfiles.StateDir, "/")+"/"+name+".applied")
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errSymlink
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) != 64 {
		return "", errors.New("applied")
	}
	return s, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
