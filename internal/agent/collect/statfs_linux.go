//go:build linux

package collect

import "syscall"

// HostStatFS reads filesystem block counts with the Linux statfs syscall.
// Bavail is the space available to an unprivileged user. Bfree includes
// blocks reserved for root, which df does not count as used.
func HostStatFS(path string) (FSStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStat{}, err
	}
	if st.Bsize <= 0 {
		return FSStat{}, syscall.EINVAL
	}
	return FSStat{
		Blocks: uint64(st.Blocks),
		Bfree:  uint64(st.Bfree),
		Bavail: uint64(st.Bavail),
		Bsize:  uint64(st.Bsize),
	}, nil
}
