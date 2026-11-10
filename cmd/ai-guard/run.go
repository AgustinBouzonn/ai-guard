package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/runner"
)

// cmdRun runs an arbitrary command as a contained process tree: bounded
// memory, bounded time, lowered priority, everything killed together. It is
// the escape hatch for the heavy commands a tier cannot replace (a full test
// suite before a push): they still run, but they cannot take the machine down.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("run", stderr)
	mem := fs.Int("mem", 0, "")
	timeout := fs.Int("timeout", 0, "")
	normal := fs.Bool("normal-priority", false, "")
	shell := fs.String("shell", "", "")
	dir := fs.String("dir", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	spec := runner.Spec{Argv: fs.Args(), Shell: *shell, Dir: *dir, MemLimitMB: *mem, LowPriority: !*normal,
		Timeout: time.Duration(*timeout) * time.Second, Stdout: stdout, Stderr: stderr}
	if *mem > 0 {
		spec.HardCapMB = *mem * 3 / 2
	}
	if len(spec.Argv) == 0 && spec.Shell == "" {
		fmt.Fprintln(stderr, "ai-guard: run needs a command: ai-guard run [--mem MB] [--timeout SEC] -- cmd args...")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := runner.Run(ctx, spec)
	code := r.ExitCode
	killed := ""
	switch {
	case r.Err != nil:
		fmt.Fprintf(stderr, "ai-guard: cannot run: %v\n", r.Err)
		return 127
	case r.Killed == runner.KilledMem:
		code, killed = 137, " killed=mem"
	case r.Killed == runner.KilledTimeout:
		code, killed = 124, " killed=timeout"
	case r.Killed == runner.KilledCancel:
		code, killed = 130, " killed=cancel"
	}
	fmt.Fprintf(stderr, "[ai-guard] exit=%d wall=%dms peak=%dMB cpu=%dms%s\n", code, r.Wall.Milliseconds(), r.PeakRSS>>20, r.CPU.Milliseconds(), killed)
	return code
}
