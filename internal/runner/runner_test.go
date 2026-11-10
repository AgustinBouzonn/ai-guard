package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary doubles as the child process: with AIGUARD_TEST_HELPER=1 it
// behaves as a small configurable program instead of running the tests.
func TestMain(m *testing.M) {
	if os.Getenv("AIGUARD_TEST_HELPER") == "1" {
		helperMain(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func spawnSelf(args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], args...)
	c.Env = append(os.Environ(), "AIGUARD_TEST_HELPER=1")
	return c
}

func helperMain(a []string) {
	switch a[0] {
	case "echo": // echo <exit code>
		fmt.Fprintln(os.Stdout, "out")
		fmt.Fprintln(os.Stderr, "err")
		os.Exit(atoi(a[1]))
	case "hog": // hog <MB> <hold ms>: allocate and touch memory gradually, then hold it
		var keep [][]byte
		for i := 0; i < atoi(a[1])/4; i++ {
			b := make([]byte, 4<<20)
			for k := 0; k < len(b); k += 4096 {
				b[k] = 1
			}
			keep = append(keep, b)
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(time.Duration(atoi(a[2])) * time.Millisecond)
		_ = keep[len(keep)-1][0]
	case "multihog": // multihog <children> <MB each> <hold ms>
		var cs []*exec.Cmd
		for i := 0; i < atoi(a[1]); i++ {
			c := spawnSelf("hog", a[2], a[3])
			if err := c.Start(); err != nil {
				os.Exit(9)
			}
			cs = append(cs, c)
		}
		for _, c := range cs {
			c.Wait()
		}
	case "tree": // tree <pid file>: start a long-lived grandchild, then stay alive
		c := spawnSelf("sleep", "60000")
		if err := c.Start(); err != nil {
			os.Exit(9)
		}
		os.WriteFile(a[1], []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(60 * time.Second)
	case "orphan": // orphan <pid file>: start a long-lived grandchild and exit at once
		c := spawnSelf("sleep", "60000")
		if err := c.Start(); err != nil {
			os.Exit(9)
		}
		os.WriteFile(a[1], []byte(strconv.Itoa(c.Process.Pid)), 0o644)
	case "late": // late <ms> <file>: announce, wait, then leave a file behind
		fmt.Println("worker started")
		time.Sleep(time.Duration(atoi(a[1])) * time.Millisecond)
		os.WriteFile(a[2], []byte("done"), 0o644)
	case "sleep": // sleep <ms>
		time.Sleep(time.Duration(atoi(a[1])) * time.Millisecond)
	case "burn": // burn <ms>: spin one core
		end := time.Now().Add(time.Duration(atoi(a[1])) * time.Millisecond)
		for time.Now().Before(end) {
		}
	case "spam": // spam <MiB>: write that much to stdout
		chunk := []byte(strings.Repeat("x", 1023) + "\n")
		for i := 0; i < atoi(a[1])*1024; i++ {
			os.Stdout.Write(chunk)
		}
	default:
		os.Exit(8)
	}
}

func helper(args ...string) Spec {
	return Spec{Argv: append([]string{os.Args[0]}, args...), Env: append(os.Environ(), "AIGUARD_TEST_HELPER=1")}
}

const mib = 1 << 20

func readPid(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
			return atoi(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper never wrote its pid file %s", file)
	return 0
}

func goneWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !pidAlive(pid)
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	res := Run(context.Background(), helper("echo", "3"))
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if got := strings.ReplaceAll(string(res.Stdout), "\r\n", "\n"); got != "out\n" {
		t.Errorf("Stdout = %q, want %q", got, "out\n")
	}
	if got := strings.ReplaceAll(string(res.Stderr), "\r\n", "\n"); got != "err\n" {
		t.Errorf("Stderr = %q, want %q", got, "err\n")
	}
	if res.Killed != NotKilled {
		t.Errorf("Killed = %v, want NotKilled", res.Killed)
	}
}

func TestRunReportsStartFailure(t *testing.T) {
	res := Run(context.Background(), Spec{Argv: []string{"aiguard-definitely-not-a-real-binary"}})
	if res.Err == nil {
		t.Error("Err = nil for a missing executable")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestRunRejectsEmptyArgv(t *testing.T) {
	if res := Run(context.Background(), Spec{}); res.Err == nil {
		t.Error("Err = nil for an empty argv")
	}
}

func TestRunKillsTreeThatExceedsMemoryBudget(t *testing.T) {
	s := helper("hog", "400", "8000") // would reach 400 MiB and then hold for 8 s
	s.MemLimitMB = 96
	res := Run(context.Background(), s)
	if res.Killed != KilledMem {
		t.Fatalf("Killed = %v (exit %d, peak %d MiB), want KilledMem", res.Killed, res.ExitCode, res.PeakRSS/mib)
	}
	if res.Wall > 6*time.Second {
		t.Errorf("Wall = %v: the tree was not stopped when it crossed the budget", res.Wall)
	}
	if res.PeakRSS < 96*mib || res.PeakRSS > 300*mib {
		t.Errorf("PeakRSS = %d MiB, want within [96, 300]: the kill must land close to the budget", res.PeakRSS/mib)
	}
}

func TestRunLeavesProcessAloneUnderBudgetAndReportsPeak(t *testing.T) {
	s := helper("hog", "128", "400")
	s.MemLimitMB = 512
	res := Run(context.Background(), s)
	if res.Killed != NotKilled || res.ExitCode != 0 {
		t.Fatalf("Killed = %v, ExitCode = %d; want an undisturbed clean exit", res.Killed, res.ExitCode)
	}
	hi := uint64(260 * mib)
	if raceBuild {
		hi = 900 * mib
	}
	if res.PeakRSS < 120*mib || res.PeakRSS > hi {
		t.Errorf("PeakRSS = %d MiB, want about 128 (within [120, %d])", res.PeakRSS/mib, hi/mib)
	}
}

func TestRunBudgetCountsTheWholeTree(t *testing.T) {
	s := helper("multihog", "3", "64", "8000") // each child stays below the budget; together they do not
	s.MemLimitMB = 150
	res := Run(context.Background(), s)
	if res.Killed != KilledMem {
		t.Fatalf("Killed = %v (peak %d MiB), want KilledMem: children must count against the budget", res.Killed, res.PeakRSS/mib)
	}
	if res.Wall > 6*time.Second {
		t.Errorf("Wall = %v, the children were left running", res.Wall)
	}
}

func TestRunTimeoutKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	s := helper("tree", pidFile)
	s.Timeout = 700 * time.Millisecond
	res := Run(context.Background(), s)
	if res.Killed != KilledTimeout {
		t.Fatalf("Killed = %v, want KilledTimeout", res.Killed)
	}
	if res.Wall > 10*time.Second {
		t.Errorf("Wall = %v, want close to the 700ms timeout", res.Wall)
	}
	if pid := readPid(t, pidFile); !goneWithin(pid, 3*time.Second) {
		killPid(pid)
		t.Errorf("grandchild %d survived the timeout kill", pid)
	}
}

func TestRunCancelStopsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res := Run(ctx, helper("sleep", "60000"))
	if res.Killed != KilledCancel {
		t.Fatalf("Killed = %v, want KilledCancel", res.Killed)
	}
	if res.Wall > 10*time.Second {
		t.Errorf("Wall = %v, cancellation did not stop the process", res.Wall)
	}
}

func TestRunKillsLeftoversWhenTheCommandExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	res := Run(context.Background(), helper("orphan", pidFile))
	if res.ExitCode != 0 || res.Killed != NotKilled {
		t.Fatalf("ExitCode = %d, Killed = %v; want a clean exit", res.ExitCode, res.Killed)
	}
	if pid := readPid(t, pidFile); !goneWithin(pid, 3*time.Second) {
		killPid(pid)
		t.Errorf("leftover grandchild %d still running after a contained run", pid)
	}
}

func TestRunDetachedLetsDaemonsSurvive(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	s := helper("orphan", pidFile)
	s.Detached = true
	res := Run(context.Background(), s)
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	pid := readPid(t, pidFile)
	defer killPid(pid)
	time.Sleep(300 * time.Millisecond)
	if !pidAlive(pid) {
		t.Errorf("grandchild %d was killed although the run was Detached", pid)
	}
}

func TestRunAccountsCPUTimeOfTheTree(t *testing.T) {
	res := Run(context.Background(), helper("burn", "500"))
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d", res.ExitCode)
	}
	if res.CPU < 250*time.Millisecond || res.CPU > 5*time.Second {
		t.Errorf("CPU = %v, want roughly the 500ms the child spent spinning", res.CPU)
	}
	if res.Wall < 450*time.Millisecond {
		t.Errorf("Wall = %v, shorter than the child's own runtime", res.Wall)
	}
}

func TestRunCapsCapturedOutputWithoutBlockingTheChild(t *testing.T) {
	s := helper("spam", "4")
	s.MaxOutputBytes = 64 << 10
	res := Run(context.Background(), s)
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d: the child must still be able to finish writing", res.ExitCode)
	}
	if len(res.Stdout) != 64<<10 {
		t.Errorf("len(Stdout) = %d, want exactly the 65536-byte cap", len(res.Stdout))
	}
	if !res.Truncated {
		t.Error("Truncated = false although 4 MiB were written")
	}
}

func TestRunHardCapStopsAllocationWithoutWatchdog(t *testing.T) {
	mech := HardCapAvailable()
	if mech == "" {
		t.Skip("no OS hard-cap mechanism usable here (needs a writable memory cgroup or a Windows Job Object)")
	}
	s := helper("hog", "400", "4000")
	s.HardCapMB = 96 // no MemLimitMB: only the kernel can stop it
	res := Run(context.Background(), s)
	if res.HardCap != mech {
		t.Errorf("HardCap = %q, want %q", res.HardCap, mech)
	}
	if res.Killed != KilledMem {
		t.Fatalf("Killed = %v (exit %d, peak %d MiB), want KilledMem from the %s cap", res.Killed, res.ExitCode, res.PeakRSS/mib, mech)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0: the process allocated 400 MiB under a 96 MiB cap")
	}
	if res.Wall > 3500*time.Millisecond {
		t.Errorf("Wall = %v: the cap did not stop the process before it finished on its own", res.Wall)
	}
}

func TestRunShellLineWithQuotedExecutable(t *testing.T) {
	// The classic trap on Windows: a quoted program path followed by arguments.
	s := Spec{Shell: `"` + os.Args[0] + `" echo 5`, Env: append(os.Environ(), "AIGUARD_TEST_HELPER=1")}
	res := Run(context.Background(), s)
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if res.ExitCode != 5 {
		t.Errorf("ExitCode = %d, want 5 (stderr %q)", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "out" {
		t.Errorf("Stdout = %q, want %q", got, "out")
	}
}

func TestRunShellLineSupportsOperators(t *testing.T) {
	res := Run(context.Background(), Spec{Shell: "echo one && echo two"})
	if got := strings.Fields(string(res.Stdout)); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("Stdout = %q, want both commands of the && chain to run", res.Stdout)
	}
}

func TestProcessRSSReadsALiveProcessAndRejectsADeadOne(t *testing.T) {
	rss, ok := ProcessRSS(os.Getpid())
	if !ok || rss < 1*mib || rss > 8192*mib {
		t.Errorf("ProcessRSS(self) = %d bytes, %v; want a plausible resident size", rss, ok)
	}
	cmd := spawnSelf("echo", "0")
	cmd.Run()
	if _, ok := ProcessRSS(cmd.Process.Pid); ok {
		t.Errorf("ProcessRSS reported a size for pid %d, which has exited", cmd.Process.Pid)
	}
}

func TestRunCountsExtraProcessesAgainstTheBudgetAndKillsThem(t *testing.T) {
	// A tool daemon lives outside the client's tree but does the real work.
	daemon := spawnSelf("hog", "200", "20000")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	pid := daemon.Process.Pid
	defer killPid(pid)
	go daemon.Wait()

	s := helper("sleep", "8000") // the client itself stays tiny
	s.MemLimitMB = 96
	s.ExtraPIDs = func() []int { return []int{pid} }
	res := Run(context.Background(), s)
	if res.Killed != KilledMem {
		t.Fatalf("Killed = %v (peak %d MiB), want KilledMem: the daemon's memory must count", res.Killed, res.PeakRSS/mib)
	}
	if res.Wall > 6*time.Second {
		t.Errorf("Wall = %v, the run was not stopped when the daemon crossed the budget", res.Wall)
	}
	if !goneWithin(pid, 3*time.Second) {
		t.Errorf("daemon %d survived although it blew the budget", pid)
	}
}

func TestRunKeepLeftoversLetsSurvivorsLive(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	s := helper("orphan", pidFile)
	s.KeepLeftovers = true
	s.MemLimitMB = 512 // still a contained, accounted run
	res := Run(context.Background(), s)
	if res.ExitCode != 0 || res.Killed != NotKilled {
		t.Fatalf("ExitCode = %d, Killed = %v", res.ExitCode, res.Killed)
	}
	pid := readPid(t, pidFile)
	defer killPid(pid)
	time.Sleep(300 * time.Millisecond)
	if !pidAlive(pid) {
		t.Errorf("survivor %d was killed although KeepLeftovers was set", pid)
	}
}

func TestStartDetachedReturnsAtOnceAndLogsTheWorkerOutput(t *testing.T) {
	dir := t.TempDir()
	marker, logFile := filepath.Join(dir, "marker"), filepath.Join(dir, "sub", "worker.log")
	t.Setenv("AIGUARD_TEST_HELPER", "1")
	start := time.Now()
	pid, err := StartDetached([]string{os.Args[0], "late", "400", marker}, dir, logFile)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Errorf("StartDetached blocked for %v; it must not wait for the worker", took)
	}
	if pid <= 0 {
		t.Errorf("pid = %d", pid)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker already exists: the worker cannot have finished yet")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the detached worker never finished: %v", err)
	}
	if out, _ := os.ReadFile(logFile); strings.TrimSpace(string(out)) != "worker started" {
		t.Errorf("log = %q, want the worker's stdout", out)
	}
}

func TestRunStreamsOutputToTheGivenWritersInsteadOfBuffering(t *testing.T) {
	var out, errb bytes.Buffer
	s := helper("echo", "0")
	s.Stdout, s.Stderr = &out, &errb
	res := Run(context.Background(), s)
	if got := strings.ReplaceAll(out.String(), "\r\n", "\n"); got != "out\n" {
		t.Errorf("stdout writer got %q, want %q", got, "out\n")
	}
	if got := strings.ReplaceAll(errb.String(), "\r\n", "\n"); got != "err\n" {
		t.Errorf("stderr writer got %q, want %q", got, "err\n")
	}
	if len(res.Stdout) != 0 || len(res.Stderr) != 0 {
		t.Errorf("output was also buffered (%d + %d bytes): a long-running command would pile it up in memory", len(res.Stdout), len(res.Stderr))
	}
}

// overlapWriter notices two goroutines inside Write at the same time.
type overlapWriter struct {
	busy, overlapped atomic.Bool
	n                atomic.Int64
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	if !w.busy.CompareAndSwap(false, true) {
		w.overlapped.Store(true)
	}
	time.Sleep(30 * time.Millisecond)
	w.n.Add(int64(len(p)))
	w.busy.Store(false)
	return len(p), nil
}

func TestRunNeverWritesConcurrentlyToAWriterSharedByBothStreams(t *testing.T) {
	// Passing one writer for both streams is the natural way to merge them (as
	// with os/exec); most writers, a bytes.Buffer included, are not safe for that.
	w := &overlapWriter{}
	s := helper("echo", "0")
	s.Stdout, s.Stderr = w, w
	Run(context.Background(), s)
	if w.n.Load() == 0 {
		t.Fatal("nothing reached the writer")
	}
	if w.overlapped.Load() {
		t.Error("stdout and stderr were written to the shared writer at the same time")
	}
}
