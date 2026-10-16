package runner

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// errStoreBad is a failed ownership, mode, or link check. The caller maps
// it to bad_spec and must not delete the offending entry.
// Anything else is an IO failure and maps to docker_unavailable.
var errStoreBad = errors.New("store check")

// fileIDs is the uid/gid a store check expects. Nil on Sys means 0:0.
// Tests that cannot chown to root set the current ids.
type fileIDs struct {
	uid int
	gid int
}

// store is an O_DIRECTORY|O_NOFOLLOW handle on the managed-file root.
// Every later name is a single path component passed to an *at call.
// fileUID and fileGID are the ids a file in a slot must have. openStore
// sets them equal to uid and gid. A test may point them elsewhere through
// Sys.fileCheck; production leaves that nil, so the check is unchanged.
// plant, when set, runs after a slot is written and before the name audit.
// Only tests set it, on the store they opened. A deploy leaves it nil.
type store struct {
	fd      int
	uid     int
	gid     int
	fileUID int
	fileGID int
	plant   func(dirfd int) error
}

func (sys Sys) storeRoot() (string, bool) {
	if sys.fileStore != "" {
		return sys.fileStore, true
	}
	// Unit tests are not root and must not create the production directory.
	if os.Geteuid() != 0 {
		return "", false
	}
	return images.FileStoreDir, true
}

func (sys Sys) fileIDs() (int, int) {
	if sys.fileOwner != nil {
		return sys.fileOwner.uid, sys.fileOwner.gid
	}
	return 0, 0
}

// errStoreAbsent is ENOENT while opening a store this verb does not need.
// The request has no files, and either the store directory or a parent is
// missing. A request with files does not use this error for that ENOENT:
// a missing parent stays an I/O error, and a missing files/ is created.
var errStoreAbsent = errors.New("store absent")

// openEntryStore opens the managed-file root. required is set when this
// request carries files: files/ is created when its parent exists. A missing
// parent is not created. A verb with no files treats a missing store or a
// missing parent as "no store" and does not create either. Any other I/O
// error is docker_unavailable. A failed ownership or mode check is bad_spec.
// Nothing is changed in those cases.
func openEntryStore(sys Sys, required bool) (*store, string) {
	root, ok := sys.storeRoot()
	if !ok {
		if required {
			return nil, programs.ReasonDocker
		}
		return nil, ""
	}
	uid, gid := sys.fileIDs()
	st, err := openStore(root, uid, gid, required)
	if err != nil {
		if errors.Is(err, errStoreAbsent) {
			return nil, ""
		}
		if errors.Is(err, errStoreBad) {
			return nil, programs.ReasonBadSpec
		}
		return nil, programs.ReasonDocker
	}
	if sys.fileCheck != nil {
		st.fileUID = sys.fileCheck.uid
		st.fileGID = sys.fileCheck.gid
	}
	return st, ""
}

func storeCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errStoreBad) {
		return programs.ReasonBadSpec
	}
	return programs.ReasonDocker
}

func closeStore(s *store) {
	if s == nil || s.fd < 0 {
		return
	}
	_ = syscall.Close(s.fd)
	s.fd = -1
}

// openStore walks root from "/" with openat and O_NOFOLLOW. Only the last
// component may be created, and only when create is set. A missing parent
// is never created: systemd's StateDirectory creates
// /var/lib/bot-agent-runner, and this process does not. When create is
// false, ENOENT on any component is errStoreAbsent. A symlink anywhere on
// the walk is errStoreBad. The fd is O_CLOEXEC so a later systemctl exec
// does not inherit a handle on the secret store.
func openStore(root string, uid, gid int, create bool) (*store, error) {
	parts, err := absParts(root)
	if err != nil {
		return nil, err
	}
	parent, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for i, name := range parts {
		last := i == len(parts)-1
		next, oerr := openDirAt(parent, name)
		if errors.Is(oerr, syscall.ENOENT) {
			if !create {
				_ = syscall.Close(parent)
				return nil, errStoreAbsent
			}
			if !last {
				_ = syscall.Close(parent)
				return nil, oerr
			}
			next, oerr = mkdirOpen(parent, name, uid, gid)
		}
		_ = syscall.Close(parent)
		if oerr != nil {
			return nil, oerr
		}
		parent = next
	}
	if err := checkDirFd(parent, uid, gid); err != nil {
		_ = syscall.Close(parent)
		return nil, err
	}
	return &store{fd: parent, uid: uid, gid: gid, fileUID: uid, fileGID: gid}, nil
}

func absParts(root string) ([]string, error) {
	if root == "" || root[0] != '/' || strings.Contains(root, "\x00") {
		return nil, errors.New("store root")
	}
	clean := path.Clean(root)
	if clean == "/" || !strings.HasPrefix(clean, "/") || strings.Contains(clean, "\x00") {
		return nil, errors.New("store root")
	}
	return strings.Split(clean[1:], "/"), nil
}

func openDirAt(parent int, name string) (int, error) {
	fd, err := openat(parent, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return -1, errStoreBad
		}
		return -1, err
	}
	return fd, nil
}

func mkdirOpen(parent int, name string, uid, gid int) (int, error) {
	if err := mkdirat(parent, name, 0700); err != nil {
		return -1, err
	}
	fd, err := openDirAt(parent, name)
	if err != nil {
		_ = unlinkat(parent, name, atRemovedir)
		return -1, err
	}
	if err := finishNewDir(fd, uid, gid); err != nil {
		_ = syscall.Close(fd)
		_ = unlinkat(parent, name, atRemovedir)
		return -1, err
	}
	return fd, nil
}

func finishNewDir(fd, uid, gid int) error {
	if err := syscall.Fchown(fd, uid, gid); err != nil {
		return err
	}
	if err := syscall.Fchmod(fd, 0700); err != nil {
		return err
	}
	return nil
}

func checkDirFd(fd, uid, gid int) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if !isDir(st) || permOf(st) != 0700 || !owned(st, uid, gid) {
		return errStoreBad
	}
	return nil
}

// preflight is read-only. A bad slot is left in place.
func (s *store) preflight(names ...string) error {
	if err := checkDirFd(s.fd, s.uid, s.gid); err != nil {
		return err
	}
	for _, name := range names {
		if err := s.checkSlot(name); err != nil {
			return err
		}
	}
	return nil
}

// checkSlot allows a missing name. A present slot must be a 0700 directory
// owned by the expected ids, containing only regular 0444 files of link
// count 1 with a managed-file name. A previous generation may use a
// different name set; the exact-name audit is only for a slot just written.
func (s *store) checkSlot(name string) error {
	st, err := fstatAt(s.fd, name)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if isLnk(st) || !isDir(st) || permOf(st) != 0700 || !owned(st, s.uid, s.gid) {
		return errStoreBad
	}
	fd, err := openDirAt(s.fd, name)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	names, err := listNames(fd)
	if err != nil {
		return err
	}
	for _, n := range names {
		cst, err := fstatAt(fd, n)
		if err != nil {
			return err
		}
		// fileUID, not the directory ids. Production sets them equal.
		// A test can point fileUID at another uid without chown, and
		// this is the term that then refuses. Slot removal still uses
		// the directory ids.
		if isLnk(cst) || !isReg(cst) || permOf(cst) != 0444 || nlinkOf(cst) != 1 || !owned(cst, s.fileUID, s.fileGID) || !images.ValidFileName(n) {
			return errStoreBad
		}
	}
	return nil
}

// writeSlot creates name and writes files. On any error the slot this call
// created is removed. A slot that already existed is not deleted: the caller
// removes leftovers only after preflight says they are safe.
func (s *store) writeSlot(name string, files []images.MountFile) (err error) {
	if err = mkdirat(s.fd, name, 0700); err != nil {
		return err
	}
	created := true
	defer func() {
		if err != nil && created {
			_ = s.removeOwn(name)
		}
	}()
	fd, err := openDirAt(s.fd, name)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err = finishNewDir(fd, s.uid, s.gid); err != nil {
		return err
	}
	for _, f := range files {
		if err = s.writeFile(fd, f.Name, []byte(f.Content)); err != nil {
			return err
		}
	}
	if s.plant != nil {
		if err = s.plant(fd); err != nil {
			return err
		}
	}
	if err = syscall.Fsync(fd); err != nil {
		return err
	}
	if err = s.auditExact(name, files); err != nil {
		return err
	}
	return nil
}

// fchmod and fchown run before the file fsync. The contract says fsync then
// mode; a crash in that window would persist a umask-widened file (0644
// under umask 022) that the next check would have to refuse. Setting the
// mode first means a crash leaves either no file or a 0444 file.
func (s *store) writeFile(dirfd int, name string, content []byte) error {
	fd, err := openat(dirfd, name, syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_WRONLY|syscall.O_CLOEXEC, 0444)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EEXIST) {
			return errStoreBad
		}
		return err
	}
	defer syscall.Close(fd)
	if err := writeFull(fd, content); err != nil {
		return err
	}
	if err := syscall.Fchown(fd, s.uid, s.gid); err != nil {
		return err
	}
	if err := syscall.Fchmod(fd, 0444); err != nil {
		return err
	}
	if err := syscall.Fsync(fd); err != nil {
		return err
	}
	return nil
}

// auditExact is the name check for a slot this process just wrote.
func (s *store) auditExact(name string, files []images.MountFile) error {
	if err := s.checkSlot(name); err != nil {
		return err
	}
	fd, err := openDirAt(s.fd, name)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	names, err := listNames(fd)
	if err != nil {
		return err
	}
	want := map[string]struct{}{}
	for _, f := range files {
		want[f.Name] = struct{}{}
	}
	if len(names) != len(want) {
		return errStoreBad
	}
	for _, n := range names {
		if _, ok := want[n]; !ok {
			return errStoreBad
		}
	}
	return nil
}

// removeIfSafe deletes a slot that passed checkSlot. A bad slot is not deleted.
// A missing slot is a no-op.
func (s *store) removeIfSafe(name string) error {
	if err := s.checkSlot(name); err != nil {
		return err
	}
	_, err := fstatAt(s.fd, name)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.unlinkSlot(name, false)
}

// removeOwn deletes a slot this call created, including a partial write
// whose mode is not 0444 yet. A non-regular entry is left in place.
func (s *store) removeOwn(name string) error {
	return s.unlinkSlot(name, true)
}

func (s *store) unlinkSlot(name string, own bool) error {
	fd, err := openDirAt(s.fd, name)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	names, err := listNames(fd)
	if err != nil {
		return err
	}
	for _, n := range names {
		st, err := fstatAt(fd, n)
		if err != nil {
			return err
		}
		if !isReg(st) {
			return errStoreBad
		}
		if !own && (permOf(st) != 0444 || nlinkOf(st) != 1 || !owned(st, s.uid, s.gid) || !images.ValidFileName(n)) {
			return errStoreBad
		}
		if err := unlinkat(fd, n, 0); err != nil {
			return err
		}
	}
	if err := unlinkat(s.fd, name, atRemovedir); err != nil {
		return err
	}
	return nil
}

// rename moves old to new. A missing old name is a no-op (a v1.4 container
// has no slot). The caller has already preflighted old.
func (s *store) rename(oldName, newName string) error {
	_, err := s.renameMoved(oldName, newName)
	return err
}

// renameMoved reports whether the directory entry was renamed. moved is
// true once renameat has succeeded, including when the following fsync
// fails, so the caller can put the slot back on a later error.
func (s *store) renameMoved(oldName, newName string) (bool, error) {
	_, err := fstatAt(s.fd, oldName)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := renameat(s.fd, oldName, s.fd, newName); err != nil {
		return false, err
	}
	if err := syscall.Fsync(s.fd); err != nil {
		return true, err
	}
	return true, nil
}

func fstatAt(fd int, name string) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	err := fstatat(fd, name, &st, atSymlinkNofollow)
	return st, err
}

func listNames(fd int) ([]string, error) {
	dup, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "dir")
	names, err := f.Readdirnames(-1)
	cerr := f.Close()
	if err != nil {
		return nil, err
	}
	return names, cerr
}

func writeFull(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := syscall.Write(fd, b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func permOf(st syscall.Stat_t) uint32 {
	return uint32(st.Mode) & 07777
}

func kindOf(st syscall.Stat_t) uint32 {
	return uint32(st.Mode) & uint32(syscall.S_IFMT)
}

func isDir(st syscall.Stat_t) bool { return kindOf(st) == uint32(syscall.S_IFDIR) }
func isReg(st syscall.Stat_t) bool { return kindOf(st) == uint32(syscall.S_IFREG) }
func isLnk(st syscall.Stat_t) bool { return kindOf(st) == uint32(syscall.S_IFLNK) }

func owned(st syscall.Stat_t, uid, gid int) bool {
	return int(st.Uid) == uid && int(st.Gid) == gid
}

func nlinkOf(st syscall.Stat_t) uint64 {
	return uint64(st.Nlink)
}
