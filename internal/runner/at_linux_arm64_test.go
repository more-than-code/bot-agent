//go:build linux && arm64

package runner

import (
	"syscall"
	"testing"
)

func checkLinuxSys(t *testing.T) {
	t.Helper()
	if syscall.SYS_MKDIRAT != 34 || syscall.SYS_UNLINKAT != 35 || syscall.SYS_RENAMEAT != 38 || syscall.SYS_OPENAT != 56 {
		t.Fatalf("mkdirat %d unlinkat %d renameat %d openat %d", syscall.SYS_MKDIRAT, syscall.SYS_UNLINKAT, syscall.SYS_RENAMEAT, syscall.SYS_OPENAT)
	}
	if sysFstatat != syscall.SYS_FSTATAT {
		t.Fatalf("fstatat %d is not SYS_FSTATAT", sysFstatat)
	}
	if sysFstatat != 79 {
		t.Fatalf("fstatat %d", sysFstatat)
	}
}
