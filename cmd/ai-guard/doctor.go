package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/gitx"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
	"github.com/AgustinBouzonn/ai-guard/internal/pipeline"
	"github.com/AgustinBouzonn/ai-guard/internal/runner"
	"github.com/AgustinBouzonn/ai-guard/internal/sysres"
	"github.com/AgustinBouzonn/ai-guard/internal/tools"
)

// cmdDoctor prints what the scheduler sees and which tool would run for each
// toolchain present in the repository.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("doctor", stderr)
	rootFlag := fs.String("root", "", "")
	cfgPath := fs.String("config", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, isRepo, err := resolveRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 2
	}
	cfg, err := config.Load(root, *cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "ai-guard %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
	if busy, err := sysres.CPUBusy(100 * time.Millisecond); err == nil {
		fmt.Fprintf(stdout, "cpu: %d logical cores, %.0f%% busy\n", runtime.NumCPU(), busy*100)
	} else {
		fmt.Fprintf(stdout, "cpu: %d logical cores (load unknown: %v)\n", runtime.NumCPU(), err)
	}
	if avail, total, ok := memMB(); ok {
		fmt.Fprintf(stdout, "ram: %d MB available of %d MB (scheduler keeps %d MB free)\n", avail, total, cfg.MinFree(total))
	} else {
		fmt.Fprintln(stdout, "ram: unknown on this OS (RAM gating disabled)")
	}
	watchdog := "tree RSS sampling"
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		watchdog = "off (unsupported OS)"
	}
	hard := runner.HardCapAvailable()
	if hard == "" {
		hard = "none (no writable memory cgroup); watchdog only"
	}
	fmt.Fprintf(stdout, "limits: watchdog=%s; OS hard cap=%s\n", watchdog, hard)
	tier1Mem := fmt.Sprintf("%dMB per step", cfg.Tier1.MemMB)
	if cfg.Tier1.MemMB <= 0 {
		_, total, _ := memMB()
		tier1Mem = fmt.Sprintf("auto: cap %dMB per step (RAM/8 within 1024..4096), reservation learned from past runs", pipeline.AutoCapMB(total))
	}
	fmt.Fprintf(stdout, "budgets: tier0 %dms/%dMB per step  tier1 %dms/%s\n", cfg.Tier0.BudgetMs, cfg.Tier0.MemMB, cfg.Tier1.BudgetMs, tier1Mem)
	toolsStatus(stdout, toolsDir())
	src := "built-in defaults"
	if _, err := os.Stat(filepath.Join(root, config.FileName)); err == nil || *cfgPath != "" {
		src = config.FileName
	}
	fmt.Fprintf(stdout, "repo: %s (config: %s)\n", root, src)
	if !isRepo {
		fmt.Fprintln(stdout, "not a git repository: no plan to show")
		return 0
	}
	tracked, err := gitx.Tracked(root, 4000)
	if err != nil {
		fmt.Fprintf(stdout, "cannot list tracked files: %v\n", err)
		return 0
	}
	var sample []string
	for _, f := range tracked {
		if lang.Of(f) != lang.None {
			sample = append(sample, f)
		}
	}
	plan := tools.Build(root, sample, cfg, machineEnv(root))
	fmt.Fprintf(stdout, "plan if every tracked source file (%d) changed:\n", len(sample))
	for _, s := range plan.Steps {
		tool := s.Shell
		if len(s.Argv) > 0 {
			tool = s.Argv[0]
			if rel, err := filepath.Rel(root, tool); err == nil && !strings.HasPrefix(rel, "..") {
				tool = filepath.ToSlash(rel)
			}
			if len(s.Argv) > 1 && (strings.HasSuffix(s.Argv[0], "node") || strings.HasSuffix(s.Argv[0], "node.exe")) {
				if rel, err := filepath.Rel(root, s.Argv[1]); err == nil && !strings.HasPrefix(rel, "..") {
					tool = "node " + filepath.ToSlash(rel)
				}
			}
		}
		mem, timeout := cfg.Budget(s.ID, s.Tier)
		budget := fmt.Sprintf("%4dMB", mem)
		if mem <= 0 {
			budget = "  auto"
		}
		fmt.Fprintf(stdout, "  t%d %-28s %s %4.0fs  %s\n", s.Tier, s.ID, budget, timeout.Seconds(), tool)
	}
	for _, n := range plan.Notes {
		fmt.Fprintf(stdout, "  !  %s\n", n.Text)
	}
	return 0
}
