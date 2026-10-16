//go:build linux && amd64

package runner

import (
	"syscall"
	"testing"
)

func checkLinuxSys(t *testing.T) {
	t.Helper()
	if syscall.SYS_OPENAT != 257 || syscall.SYS_MKDIRAT != 258 || syscall.SYS_UNLINKAT != 263 || syscall.SYS_RENAMEAT != 264 {
		t.Fatalf("openat %d mkdirat %d unlinkat %d renameat %d", syscall.SYS_OPENAT, syscall.SYS_MKDIRAT, syscall.SYS_UNLINKAT, syscall.SYS_RENAMEAT)
	}
	if sysFstatat != syscall.SYS_NEWFSTATAT {
		t.Fatalf("fstatat %d is not SYS_NEWFSTATAT", sysFstatat)
	}
	if sysFstatat != 262 {
		t.Fatalf("fstatat %d", sysFstatat)
	}
}
