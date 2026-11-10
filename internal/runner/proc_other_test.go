//go:build !linux && !windows

package runner

import "syscall"

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func killPid(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
