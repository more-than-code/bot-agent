//go:build linux && amd64

package runner

import "syscall"

const sysFstatat = syscall.SYS_NEWFSTATAT
