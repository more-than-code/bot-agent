//go:build !linux

package collect

import "errors"

// HostStatFS is the non-Linux stub. The daemon's disk numbers on a Pi come
// from statfs_linux.go. Tests inject their own StatFS.
func HostStatFS(path string) (uint64, uint64, error) {
	return 0, 0, errors.New("statfs is only implemented on linux")
}
