//go:build !linux && !windows

package runner

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// unixTree is the minimal fallback (macOS, BSD): the tree is a process group,
// killed as a unit. There is no cheap way to sample a tree's RSS here, so the
// memory watchdog is inert and the peak comes from rusage of the root.
type unixTree struct {
	pid       int
	contained bool
	keep      bool
}

func startTree(cmd *exec.Cmd, s *Spec) (tree, error) {
	t := &unixTree{contained: !s.Detached, keep: s.KeepLeftovers}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: t.contained}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t.pid = cmd.Process.Pid
	return t, nil
}

// ShellCommand runs a line through the POSIX shell.
func ShellCommand(line string) *exec.Cmd { return exec.Command("sh", "-c", line) }

func (t *unixTree) rss() uint64 { return 0 }

func (t *unixTree) capHit() bool { return false }

func (t *unixTree) kill() {
	if t.contained {
		syscall.Kill(-t.pid, syscall.SIGKILL)
		return
	}
	syscall.Kill(t.pid, syscall.SIGKILL)
}

func (t *unixTree) finish(ps *os.ProcessState) (cpu time.Duration, peakHint uint64, hardKilled bool) {
	if t.contained && !t.keep {
		syscall.Kill(-t.pid, syscall.SIGKILL)
	}
	if ps != nil {
		cpu = ps.UserTime() + ps.SystemTime()
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			peakHint = uint64(ru.Maxrss)
			if runtime.GOOS != "darwin" { // macOS reports bytes, the BSDs kilobytes
				peakHint <<= 10
			}
		}
	}
	return cpu, peakHint, false
}

func (t *unixTree) hardCap() string { return "" }

// HardCapAvailable reports which OS mechanism can enforce Spec.HardCapMB here
// ("" when none is usable).
func HardCapAvailable() string { return "" }

// ProcessRSS is not implemented outside Linux and Windows.
func ProcessRSS(pid int) (bytes uint64, ok bool) { return 0, false }

// KillProcess terminates one process by pid.
func KillProcess(pid int) {
	if pid > 1 {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
