//go:build linux

package runner

import "testing"

// TestLinuxUapiConstants checks the numbers at_linux.go passes to the kernel.
// AT_SYMLINK_NOFOLLOW and AT_REMOVEDIR are not exported by syscall; 0x100 and
// 0x200 are the Linux uapi values. checkLinuxSys compares the SYS_* numbers
// with the exported syscall constants and the uapi table for this architecture.
func TestLinuxUapiConstants(t *testing.T) {
	if atSymlinkNofollow != 0x100 {
		t.Fatalf("AT_SYMLINK_NOFOLLOW %#x", atSymlinkNofollow)
	}
	if atRemovedir != 0x200 {
		t.Fatalf("AT_REMOVEDIR %#x", atRemovedir)
	}
	checkLinuxSys(t)
}
