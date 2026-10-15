//go:build !linux

package main

func peercred(int) (int, bool) {
	return 0, false
}
