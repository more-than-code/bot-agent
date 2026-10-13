//go:build linux

package collect

import "syscall"

// HostStatFS reads filesystem size with the Linux statfs syscall.
// avail is the space available to an unprivileged user (Bavail).
func HostStatFS(path string) (total, avail uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return 0, 0, syscall.EINVAL
	}
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize, nil
}
