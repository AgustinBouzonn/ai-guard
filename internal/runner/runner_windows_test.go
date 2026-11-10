package runner

import (
	"syscall"
	"testing"
	"time"
)

// NtResumeProcess is the primary way to thaw a suspended child; this proves
// the documented fallback (thread enumeration) works on its own.
func TestResumeThreadsThawsSuspendedProcess(t *testing.T) {
	cmd := spawnSelf("echo", "0")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Fatal("process finished although it was created suspended")
	case <-time.After(300 * time.Millisecond):
	}
	if err := resumeThreads(uint32(cmd.Process.Pid)); err != nil {
		cmd.Process.Kill()
		t.Fatalf("resumeThreads: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("process exited with %v after being resumed", err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process still frozen 10s after resumeThreads")
	}
}
