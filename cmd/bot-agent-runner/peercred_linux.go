//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// soPeercred is Linux SOL_SOCKET/SO_PEERCRED. The standard library does not
// wrap it, and this tree does not take an extra module for one integer.
const soPeercred = 17

func peercred(fd int) (int, bool) {
	var u struct {
		pid int32
		uid uint32
		gid uint32
	}
	n := uint32(unsafe.Sizeof(u))
	_, _, errno := syscall.Syscall6(
		syscall.SYS_GETSOCKOPT,
		uintptr(fd),
		uintptr(syscall.SOL_SOCKET),
		uintptr(soPeercred),
		uintptr(unsafe.Pointer(&u)),
		uintptr(unsafe.Pointer(&n)),
		0,
	)
	if errno != 0 {
		return 0, false
	}
	return int(u.uid), true
}
