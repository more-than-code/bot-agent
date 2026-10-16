//go:build darwin

package runner

/*
#include <fcntl.h>
#include <stdio.h>
#include <sys/stat.h>
#include <unistd.h>

static int bot_openat(int fd, const char *path, int flags, mode_t mode) {
	return openat(fd, path, flags, mode);
}
static int bot_mkdirat(int fd, const char *path, mode_t mode) {
	return mkdirat(fd, path, mode);
}
static int bot_unlinkat(int fd, const char *path, int flags) {
	return unlinkat(fd, path, flags);
}
static int bot_renameat(int oldfd, const char *old, int newfd, const char *newp) {
	return renameat(oldfd, old, newfd, newp);
}
static int bot_fstatat(int fd, const char *path, struct stat *st, int flags) {
	return fstatat(fd, path, st, flags);
}
*/
import "C"
import (
	"syscall"
	"unsafe"
)

// Darwin's syscall package has no openat: the call lives in libSystem.
// These values are the ones in <fcntl.h>.
const (
	atSymlinkNofollow = 0x20
	atRemovedir       = 0x80
)

func openat(dirfd int, path string, flags int, mode uint32) (int, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return -1, err
	}
	fd, err := C.bot_openat(C.int(dirfd), (*C.char)(unsafe.Pointer(p)), C.int(flags), C.mode_t(mode))
	if fd < 0 {
		if err == nil {
			return -1, syscall.EINVAL
		}
		return -1, err
	}
	return int(fd), nil
}

func mkdirat(dirfd int, path string, mode uint32) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	r, err := C.bot_mkdirat(C.int(dirfd), (*C.char)(unsafe.Pointer(p)), C.mode_t(mode))
	if r < 0 {
		if err == nil {
			return syscall.EINVAL
		}
		return err
	}
	return nil
}

func unlinkat(dirfd int, path string, flags int) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	r, err := C.bot_unlinkat(C.int(dirfd), (*C.char)(unsafe.Pointer(p)), C.int(flags))
	if r < 0 {
		if err == nil {
			return syscall.EINVAL
		}
		return err
	}
	return nil
}

func renameat(olddirfd int, oldpath string, newdirfd int, newpath string) error {
	oldp, err := syscall.BytePtrFromString(oldpath)
	if err != nil {
		return err
	}
	newp, err := syscall.BytePtrFromString(newpath)
	if err != nil {
		return err
	}
	r, err := C.bot_renameat(C.int(olddirfd), (*C.char)(unsafe.Pointer(oldp)), C.int(newdirfd), (*C.char)(unsafe.Pointer(newp)))
	if r < 0 {
		if err == nil {
			return syscall.EINVAL
		}
		return err
	}
	return nil
}

func fstatat(dirfd int, path string, st *syscall.Stat_t, flags int) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	var cst C.struct_stat
	r, err := C.bot_fstatat(C.int(dirfd), (*C.char)(unsafe.Pointer(p)), &cst, C.int(flags))
	if r < 0 {
		if err == nil {
			return syscall.EINVAL
		}
		return err
	}
	st.Mode = uint16(cst.st_mode)
	st.Nlink = uint16(cst.st_nlink)
	st.Uid = uint32(cst.st_uid)
	st.Gid = uint32(cst.st_gid)
	return nil
}
