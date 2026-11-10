package runner

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// pidAlive treats zombies as dead: nothing in the sandbox may reap orphans.
func pidAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] != 'Z'
}

func killPid(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
