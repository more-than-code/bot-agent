//go:build linux

package runner

import (
	"unsafe"

	"syscall"
)

// Linux uapi values. They are the same on every architecture. The syscall
// package does not export them, and its Unlinkat drops the flags argument,
// so a directory cannot be removed through that wrapper. amd64 also hides
// fstatat. The numbers below are the exported SYS_* constants.
const (
	atSymlinkNofollow = 0x100
	atRemovedir       = 0x200
)

func openat(dirfd int, path string, flags int, mode uint32) (int, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return -1, err
	}
	r0, _, e1 := syscall.Syscall6(syscall.SYS_OPENAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags), uintptr(mode), 0, 0)
	if e1 != 0 {
		return -1, e1
	}
	return int(r0), nil
}

func mkdirat(dirfd int, path string, mode uint32) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	_, _, e1 := syscall.Syscall(syscall.SYS_MKDIRAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(mode))
	if e1 != 0 {
		return e1
	}
	return nil
}

func unlinkat(dirfd int, path string, flags int) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	_, _, e1 := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags))
	if e1 != 0 {
		return e1
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
	_, _, e1 := syscall.Syscall6(syscall.SYS_RENAMEAT, uintptr(olddirfd), uintptr(unsafe.Pointer(oldp)), uintptr(newdirfd), uintptr(unsafe.Pointer(newp)), 0, 0)
	if e1 != 0 {
		return e1
	}
	return nil
}

func fstatat(dirfd int, path string, st *syscall.Stat_t, flags int) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	_, _, e1 := syscall.Syscall6(sysFstatat, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(st)), uintptr(flags), 0, 0)
	if e1 != 0 {
		return e1
	}
	return nil
}
