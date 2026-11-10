//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// StartDetached launches a background worker in its own session, with its
// output appended to logFile, and returns without waiting for it. The worker
// outlives the caller.
func StartDetached(argv []string, dir, logFile string) (pid int, err error) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid = cmd.Process.Pid
	return pid, cmd.Process.Release()
}
