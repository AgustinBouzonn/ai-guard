package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// StartDetached launches a background worker with its output appended to
// logFile and returns without waiting for it. The worker gets a hidden
// console (so the console programs it runs do not pop up windows) and, where
// the enclosing job allows it, leaves that job so it outlives the caller.
func StartDetached(argv []string, dir, logFile string) (pid int, err error) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		return cmd, cmd.Start()
	}
	cmd, err := start(createNoWindow | createNewProcessGroup | createBreakawayFromJob)
	if err != nil { // the job we run in forbids breakaway: stay inside it
		if cmd, err = start(createNoWindow | createNewProcessGroup); err != nil {
			return 0, err
		}
	}
	pid = cmd.Process.Pid
	return pid, cmd.Process.Release()
}
