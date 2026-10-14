//go:build !linux

package collect

import "errors"

// HostStatFS is the non-Linux stub. The daemon's disk numbers on a Pi come
// from statfs_linux.go. Tests inject their own StatFS.
func HostStatFS(path string) (FSStat, error) {
	return FSStat{}, errors.New("statfs is only implemented on linux")
}
