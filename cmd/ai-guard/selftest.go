package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/runner"
	"github.com/AgustinBouzonn/ai-guard/internal/sysres"
)

// helperChild implements the hidden commands the self-test runs under limits.
func helperChild(cmd string, a []string) int {
	n := func(i int) int {
		if i < len(a) {
			v, _ := strconv.Atoi(a[i])
			return v
		}
		return 0
	}
	switch cmd {
	case "__hog": // __hog <MB> <hold ms>
		var keep [][]byte
		for i := 0; i < n(0)/4; i++ {
			b := make([]byte, 4<<20)
			for k := 0; k < len(b); k += 4096 {
				b[k] = 1
			}
			keep = append(keep, b)
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(time.Duration(n(1)) * time.Millisecond)
		runtime.KeepAlive(keep)
	case "__sleep": // __sleep <ms>
		time.Sleep(time.Duration(n(0)) * time.Millisecond)
	case "__tree": // __tree <pid file>: leave a grandchild running
		c := exec.Command(os.Args[0], "__sleep", "60000")
		if err := c.Start(); err != nil {
			return 9
		}
		if len(a) > 0 {
			os.WriteFile(a[0], []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		}
		time.Sleep(60 * time.Second)
	}
	return 0
}

// cmdSelftest proves, on this machine, that a runaway tool is stopped: by the
// watchdog, by the timeout (children included) and by the OS cap.
func cmdSelftest(stdout io.Writer) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stdout, "FAIL  cannot locate own executable: %v\n", err)
		return 1
	}
	failed := 0
	report := func(ok bool, name, detail string) {
		word := "PASS"
		if !ok {
			word = "FAIL"
			failed++
		}
		fmt.Fprintf(stdout, "%s  %s (%s)\n", word, name, detail)
	}
	sampled := runtime.GOOS == "linux" || runtime.GOOS == "windows"

	if m, err := sysres.Memory(); err == nil {
		report(m.AvailBytes > 0 && m.AvailBytes <= m.TotalBytes, "free RAM readable", fmt.Sprintf("%d of %d MB available", m.AvailBytes>>20, m.TotalBytes>>20))
	} else {
		fmt.Fprintf(stdout, "SKIP  free RAM readable (%v)\n", err)
	}
	if busy, err := sysres.CPUBusy(100 * time.Millisecond); err == nil {
		report(busy >= 0 && busy <= 1, "CPU load readable", fmt.Sprintf("%.0f%% busy", busy*100))
	} else {
		fmt.Fprintf(stdout, "SKIP  CPU load readable (%v)\n", err)
	}

	if sampled {
		r := runner.Run(context.Background(), runner.Spec{Argv: []string{self, "__hog", "300", "8000"}, MemLimitMB: 64})
		report(r.Killed == runner.KilledMem && r.Wall < 6*time.Second, "memory watchdog",
			fmt.Sprintf("300 MB hog under a 64 MB budget: %s after %d ms at %d MB", killWord(r), r.Wall.Milliseconds(), r.PeakRSS>>20))
	} else {
		fmt.Fprintln(stdout, "SKIP  memory watchdog (tree RSS sampling is implemented for Linux and Windows)")
	}

	pidFile := filepath.Join(os.TempDir(), fmt.Sprintf("aiguard-selftest-%d.pid", os.Getpid()))
	defer os.Remove(pidFile)
	r := runner.Run(context.Background(), runner.Spec{Argv: []string{self, "__tree", pidFile}, Timeout: 700 * time.Millisecond})
	gone, detail := true, "grandchild pid unknown"
	if b, err := os.ReadFile(pidFile); err == nil {
		pid, _ := strconv.Atoi(string(b))
		if sampled {
			for i := 0; i < 100; i++ {
				if _, alive := runner.ProcessRSS(pid); !alive {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			_, alive := runner.ProcessRSS(pid)
			gone = !alive
		}
		detail = fmt.Sprintf("grandchild %d gone=%v", pid, gone)
	}
	report(r.Killed == runner.KilledTimeout && r.Wall < 8*time.Second && gone, "timeout kills the whole tree",
		fmt.Sprintf("%s after %d ms, %s", killWord(r), r.Wall.Milliseconds(), detail))

	if mech := runner.HardCapAvailable(); mech != "" {
		r := runner.Run(context.Background(), runner.Spec{Argv: []string{self, "__hog", "300", "4000"}, HardCapMB: 64, Timeout: 20 * time.Second})
		report(r.Killed == runner.KilledMem && r.HardCap == mech, "OS hard cap ("+mech+")",
			fmt.Sprintf("300 MB hog under a 64 MB cap, watchdog off: %s after %d ms", killWord(r), r.Wall.Milliseconds()))
	} else {
		fmt.Fprintln(stdout, "SKIP  OS hard cap (no writable memory cgroup here; the watchdog still enforces budgets)")
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func killWord(r runner.Result) string {
	if r.Err != nil {
		return "start failed: " + r.Err.Error()
	}
	if r.Killed == runner.NotKilled {
		return fmt.Sprintf("not killed (exit %d)", r.ExitCode)
	}
	return "killed:" + r.Killed.String()
}
