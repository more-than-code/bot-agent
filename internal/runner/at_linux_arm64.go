//go:build linux && arm64

package runner

import "syscall"

const sysFstatat = syscall.SYS_FSTATAT
