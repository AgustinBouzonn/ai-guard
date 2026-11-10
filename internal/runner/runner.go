// Package runner executes one external tool as a contained process tree:
// bounded memory (watchdog over the whole tree plus an optional OS hard cap),
// bounded time, guaranteed cleanup, and resource accounting (peak RSS, CPU).
package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// lockedWriter serialises writes to a writer shared by several goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// sameWriter reports whether a and b are the same writer. Comparing
// interfaces panics for uncomparable dynamic types; those are not the same.
func sameWriter(a, b io.Writer) (same bool) {
	defer func() { recover() }()
	return a == b
}

// KillReason says why the runner terminated a tree.
type KillReason int

const (
	NotKilled     KillReason = iota
	KilledMem                // tree RSS exceeded MemLimitMB, or the OS hard cap fired
	KilledTimeout            // Timeout elapsed
	KilledCancel             // the context was cancelled
)

func (k KillReason) String() string {
	switch k {
	case KilledMem:
		return "mem"
	case KilledTimeout:
		return "timeout"
	case KilledCancel:
		return "cancel"
	}
	return ""
}

// Spec describes one process to run.
type Spec struct {
	Argv  []string
	Shell string // alternative to Argv: a command line for the platform shell
	Dir   string
	Env   []string // nil inherits the parent environment
	Stdin io.Reader

	MemLimitMB int           // watchdog budget for the whole tree (RSS); 0 = unlimited
	HardCapMB  int           // OS-enforced cap (Job Object / cgroup); 0 = none
	RlimitASMB int           // Linux only: RLIMIT_AS for the tree; 0 = none
	Timeout    time.Duration // 0 = none

	// Detached runs the command uncontained: descendants that outlive it (tool
	// daemons) are neither killed nor capped. Only the direct child is governed.
	Detached bool
	// KeepLeftovers contains the tree for accounting and limits, but whatever
	// is still running when the command exits is left alone.
	KeepLeftovers bool
	LowPriority   bool
	// ExtraPIDs names processes outside the tree (a tool's daemon) whose memory
	// counts against MemLimitMB and which are killed together with the tree.
	ExtraPIDs func() []int

	MaxOutputBytes int                   // per stream; 0 = 8 MiB
	OnSample       func(rssBytes uint64) // called on every watchdog sample
	// Stdout and Stderr, when set, receive the stream as it is produced and it
	// is not buffered into the Result.
	Stdout, Stderr io.Writer
}

// Result is what happened.
type Result struct {
	ExitCode  int // -1 when the process did not start or died from a signal
	Stdout    []byte
	Stderr    []byte
	Wall      time.Duration
	PeakRSS   uint64        // bytes, whole tree
	CPU       time.Duration // user+system, whole tree
	Killed    KillReason
	Err       error  // start failure
	HardCap   string // OS mechanism that enforced HardCapMB: "jobobject", "cgroup2", "cgroup1" or ""
	Truncated bool   // output exceeded MaxOutputBytes
}

// tree is the per-OS handle on a started process tree. All methods are called
// from the single goroutine that runs the watchdog loop.
type tree interface {
	rss() uint64 // current resident memory of the whole tree, bytes (0 = unknown)
	// capHit reports, right after rss(), that the tree is pinned against the OS
	// hard cap. Such a tree cannot make progress (allocations fail or the
	// kernel is picking victims), so the runner ends it at once.
	capHit() bool
	kill() // terminate every process in the tree
	// finish runs once the root has exited: it removes leftovers, releases OS
	// objects and returns the final accounting.
	finish(ps *os.ProcessState) (cpu time.Duration, peakHint uint64, hardKilled bool)
	hardCap() string
}

const defaultMaxOutput = 8 << 20

// capBuffer keeps the first max bytes and discards the rest, so a chatty child
// never blocks on a full pipe and never balloons our own heap.
type capBuffer struct {
	mu        sync.Mutex
	b         []byte
	max       int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	if room := c.max - len(c.b); room >= len(p) {
		c.b = append(c.b, p...)
	} else {
		if room > 0 {
			c.b = append(c.b, p[:room]...)
		}
		c.truncated = true
	}
	c.mu.Unlock()
	return len(p), nil
}

func (c *capBuffer) snapshot() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b, c.truncated
}

// sampleInterval trades peak-detection resolution for watchdog overhead: fine
// while a process is young (most tools live for milliseconds), coarser later.
func sampleInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 60*time.Millisecond:
		return 3 * time.Millisecond
	case elapsed < 600*time.Millisecond:
		return 10 * time.Millisecond
	case elapsed < 6*time.Second:
		return 25 * time.Millisecond
	}
	return 50 * time.Millisecond
}

// Run executes s and blocks until its tree is gone.
func Run(ctx context.Context, s Spec) Result {
	var cmd *exec.Cmd
	switch {
	case s.Shell != "":
		cmd = ShellCommand(s.Shell)
	case len(s.Argv) > 0:
		cmd = exec.Command(s.Argv[0], s.Argv[1:]...)
	default:
		return Result{ExitCode: -1, Err: errors.New("runner: empty argv")}
	}
	start := time.Now()
	cmd.Dir, cmd.Env, cmd.Stdin = s.Dir, s.Env, s.Stdin

	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return Result{ExitCode: -1, Err: err}
	}
	cmd.Stdout, cmd.Stderr = outW, errW

	tr, err := startTree(cmd, &s)
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return Result{ExitCode: -1, Err: err, Wall: time.Since(start)}
	}

	max := s.MaxOutputBytes
	if max <= 0 {
		max = defaultMaxOutput
	}
	outBuf, errBuf := &capBuffer{max: max}, &capBuffer{max: max}
	var outW2, errW2 io.Writer = outBuf, errBuf
	if s.Stdout != nil {
		outW2 = s.Stdout
	}
	if s.Stderr != nil {
		errW2 = s.Stderr
	}
	if s.Stdout != nil && sameWriter(s.Stdout, s.Stderr) {
		// One writer for both streams (the way to merge them): like os/exec,
		// never let the two copiers write to it at once.
		merged := &lockedWriter{w: s.Stdout}
		outW2, errW2 = merged, merged
	}
	drained := make(chan struct{}, 2)
	go func() { io.Copy(outW2, outR); drained <- struct{}{} }()
	go func() { io.Copy(errW2, errR); drained <- struct{}{} }()

	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	var (
		res      = Result{HardCap: tr.hardCap()}
		limit    = uint64(s.MemLimitMB) << 20
		cancel   = ctx.Done()
		deadline <-chan time.Time
		giveUp   <-chan time.Time
	)
	if s.Timeout > 0 {
		t := time.NewTimer(s.Timeout)
		defer t.Stop()
		deadline = t.C
	}
	var extra []int
	kill := func(why KillReason) {
		if res.Killed == NotKilled {
			res.Killed = why
			tr.kill()
			if why == KilledMem {
				for _, pid := range extra {
					KillProcess(pid)
				}
			}
			giveUp = time.After(10 * time.Second) // never hang on an unkillable tree
		}
	}
	tick := time.NewTimer(sampleInterval(0))
	defer tick.Stop()
loop:
	for {
		select {
		case <-exited:
			break loop
		case <-cancel:
			cancel = nil
			kill(KilledCancel)
		case <-deadline:
			deadline = nil
			kill(KilledTimeout)
		case <-giveUp:
			break loop
		case <-tick.C:
			r := tr.rss()
			if s.ExtraPIDs != nil {
				extra = s.ExtraPIDs()
				for _, pid := range extra {
					if b, ok := ProcessRSS(pid); ok {
						r += b
					}
				}
			}
			if r > res.PeakRSS {
				res.PeakRSS = r
			}
			if s.OnSample != nil {
				s.OnSample(r)
			}
			if (limit > 0 && r > limit) || tr.capHit() {
				kill(KilledMem)
			}
			tick.Reset(sampleInterval(time.Since(start)))
		}
	}

	cpu, hint, hardKilled := tr.finish(cmd.ProcessState)
	res.Wall = time.Since(start)
	res.CPU = cpu
	if hint > res.PeakRSS {
		res.PeakRSS = hint
	}
	if hardKilled && res.Killed == NotKilled {
		res.Killed = KilledMem
	}
	res.ExitCode = -1
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	// Readers end when every holder of the pipe is gone. A detached daemon may
	// keep it open forever, so do not wait long for one.
	grace := 3 * time.Second
	if s.Detached || s.KeepLeftovers {
		grace = 250 * time.Millisecond
	}
	wait := time.After(grace)
	for n := 0; n < 2; {
		select {
		case <-drained:
			n++
		case <-wait:
			n = 2
		}
	}
	outR.Close()
	errR.Close()
	res.Stdout, res.Truncated = outBuf.snapshot()
	var t2 bool
	res.Stderr, t2 = errBuf.snapshot()
	res.Truncated = res.Truncated || t2
	return res
}
