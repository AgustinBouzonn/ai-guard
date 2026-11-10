package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/remote"
	"github.com/AgustinBouzonn/ai-guard/internal/report"
	"github.com/AgustinBouzonn/ai-guard/internal/runner"
)

// queueTier2 records a remote job and starts its detached worker (this same
// binary), so the caller gets its verdict back without waiting for the sync.
// cfgPath is the --config the caller was given ("" = the repository's own):
// the worker must act on the same settings that decided to queue the job,
// not on the defaults, which would push a snapshot to origin.
func queueTier2(root string, cfg *config.Config, cfgPath, reason string) (report.Tier2, error) {
	// Without a command, tier 2 only pushes the snapshot. That is a choice the
	// configuration has to make (a trigger other than "off"): the defaults
	// alone must never send the work tree to a remote.
	if len(cfg.Tier2.Command) == 0 && cfg.Tier2.Shell == "" && (cfg.Tier2.Sync == "none" || cfg.Tier2.Trigger == "off") {
		return report.Tier2{}, errors.New("tier 2 is not configured: set tier2.command in aiguard.json (or a tier2.trigger, to only push the snapshot)")
	}
	if err := ensureState(root); err != nil {
		return report.Tier2{}, err
	}
	self, err := os.Executable()
	if err != nil {
		return report.Tier2{}, err
	}
	worker := []string{self, "__tier2-job", root}
	if cfgPath != "" {
		abs, err := filepath.Abs(cfgPath) // the worker runs from the repository root
		if err != nil {
			return report.Tier2{}, err
		}
		cfgPath = abs
	}
	job, err := remote.Queue(root, reason, func(id string) (int, error) {
		argv := append(append([]string{}, worker...), id)
		if cfgPath != "" {
			argv = append(argv, cfgPath)
		}
		return runner.StartDetached(argv, root, filepath.Join(remote.Dir(root), id+".log"))
	})
	if err != nil {
		return report.Tier2{}, err
	}
	return report.Tier2{ID: job.ID, State: job.State, Log: job.Log}, nil
}

// cmdTier2Job is the worker process: __tier2-job <root> <id> [<config file>].
func cmdTier2Job(args []string, stderr io.Writer) int {
	if len(args) != 2 && len(args) != 3 {
		return 2
	}
	root, id, cfgPath := args[0], args[1], ""
	if len(args) == 3 {
		cfgPath = args[2]
	}
	cfg, err := config.Load(root, cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "[ai-guard] %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if _, err := remote.Execute(ctx, root, cfg.Tier2, id, os.Stdout); err != nil {
		return 1
	}
	return 0
}

func jobLine(j remote.Job) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", j.ID, j.State)
	if j.ExitCode != nil {
		fmt.Fprintf(&b, " exit=%d", *j.ExitCode)
	}
	end := time.Now()
	if j.Finished != nil {
		end = *j.Finished
	}
	fmt.Fprintf(&b, " %ds", int(end.Sub(j.Created).Seconds()))
	if len(j.SHA) >= 10 {
		fmt.Fprintf(&b, " sha=%s", j.SHA[:10])
	}
	if j.Error != "" {
		fmt.Fprintf(&b, " error=%q", j.Error)
	}
	return b.String()
}

func logTail(root string, j remote.Job, n int) []string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(j.Log)))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func printJob(w io.Writer, root string, j remote.Job) {
	fmt.Fprintln(w, jobLine(j))
	for _, l := range logTail(root, j, 15) {
		fmt.Fprintf(w, "  %s\n", l)
	}
}

// settle fails a job whose worker died before finishing (reboot, kill).
func settle(root string, j remote.Job) remote.Job {
	if j.Final() || j.Pid <= 0 || time.Since(j.Created) < 2*time.Second {
		return j
	}
	if _, alive := runner.ProcessRSS(j.Pid); alive {
		return j
	}
	if cur, err := remote.Load(root, j.ID); err == nil && cur.Final() { // it finished in between
		return cur
	}
	return remote.Fail(root, j, "worker exited without finishing")
}

func cmdTier2(args []string, stdout, stderr io.Writer) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := newFlags("tier2", stderr)
	rootFlag := fs.String("root", "", "")
	cfgPath := fs.String("config", "", "")
	timeout := fs.Int("timeout", 1800, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "ai-guard: "+format+"\n", a...)
		return 2
	}
	root, _, err := resolveRoot(*rootFlag)
	if err != nil {
		return fail("%v", err)
	}
	pick := func() (remote.Job, error) {
		if id := fs.Arg(0); id != "" {
			return remote.Load(root, id)
		}
		jobs, _ := remote.List(root)
		if len(jobs) == 0 {
			return remote.Job{}, errors.New("no tier-2 jobs yet")
		}
		return jobs[0], nil
	}
	switch sub {
	case "run":
		cfg, err := config.Load(root, *cfgPath)
		if err != nil {
			return fail("%v", err)
		}
		t2, err := queueTier2(root, cfg, *cfgPath, "manual")
		if err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(stdout, "> tier2 %s id=%s log=%s\n", t2.State, t2.ID, t2.Log)
		return 0
	case "list":
		jobs, _ := remote.List(root)
		if len(jobs) > 10 {
			jobs = jobs[:10]
		}
		for _, j := range jobs {
			fmt.Fprintln(stdout, jobLine(settle(root, j)))
		}
		return 0
	case "status":
		j, err := pick()
		if err != nil {
			return fail("%v", err)
		}
		j = settle(root, j)
		printJob(stdout, root, j)
		if cfg, err := config.Load(root, *cfgPath); err == nil && len(cfg.Tier2.Status) > 0 {
			r := runner.Run(context.Background(), runner.Spec{Argv: cfg.Tier2.Status, Dir: root, Timeout: 30 * time.Second})
			for _, l := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
				if l != "" {
					fmt.Fprintf(stdout, "remote: %s\n", l)
				}
			}
		}
		return 0
	case "wait":
		j, err := pick()
		if err != nil {
			return fail("%v", err)
		}
		deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
		for {
			if cur, err := remote.Load(root, j.ID); err == nil {
				j = settle(root, cur)
			}
			if j.Final() {
				printJob(stdout, root, j)
				if j.State == "done" {
					return 0
				}
				return 1
			}
			if time.Now().After(deadline) {
				printJob(stdout, root, j)
				fmt.Fprintf(stderr, "ai-guard: tier-2 job %s still %s after %ds\n", j.ID, j.State, *timeout)
				return 3
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return fail("unknown tier2 command %q (run, list, status, wait)", sub)
}
