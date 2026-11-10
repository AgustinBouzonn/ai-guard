package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/bench"
	"github.com/AgustinBouzonn/ai-guard/internal/gitx"
)

// cmdBench drives the comparison between a legacy pipeline and AI-Guard:
//
//	bench init      draft aiguard.bench.json for this repository
//	bench run       apply each probe, measure both pipelines, print the table
//	bench restore   put back files left patched by an interrupted run
func cmdBench(args []string, stdout, stderr io.Writer) int {
	sub := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := newFlags("bench", stderr)
	rootFlag := fs.String("root", "", "")
	cfgPath := fs.String("config", "", "")
	reps := fs.Int("reps", 0, "")
	warmup := fs.Int("warmup", -1, "")
	only := fs.String("only", "", "")
	force := fs.Bool("force", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "ai-guard: "+format+"\n", a...)
		return 2
	}
	root, isRepo, err := resolveRoot(*rootFlag)
	if err != nil {
		return fail("%v", err)
	}
	path := *cfgPath
	if path == "" {
		path = filepath.Join(root, bench.FileName)
	}
	switch sub {
	case "init":
		if !isRepo {
			return fail("%s is not a git repository", root)
		}
		if _, err := os.Stat(path); err == nil && !*force {
			return fail("%s already exists (use --force to replace it)", path)
		}
		tracked, err := gitx.Tracked(root, 0)
		if err != nil {
			return fail("%v", err)
		}
		self, err := os.Executable()
		if err != nil {
			return fail("%v", err)
		}
		cfg := bench.Init(root, tracked, self)
		if len(cfg.Patches) == 0 {
			return fail("no TypeScript, Python, Go or Rust sources found in %s", root)
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
		for _, p := range cfg.Patches {
			fmt.Fprintf(stdout, "  patch    %-10s %s\n", p.Name, p.File)
		}
		for _, c := range cfg.Baseline {
			fmt.Fprintf(stdout, "  baseline %-12s %s  (dir: %s)\n", c.Name, c.Shell, filepath.Join(".", c.Dir))
		}
		fmt.Fprintln(stdout, "edit the baseline until it is the pipeline you run today, then: ai-guard bench run")
		return 0
	case "restore":
		n, err := bench.Restore(root)
		if err != nil {
			return fail("%v", err)
		}
		if n == 0 {
			fmt.Fprintln(stdout, "nothing to restore")
		} else {
			fmt.Fprintf(stdout, "restored %d file(s)\n", n)
		}
		return 0
	case "run":
		data, err := os.ReadFile(path)
		if err != nil {
			return fail("%v (create it with: ai-guard bench init)", err)
		}
		var cfg bench.Config
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return fail("%s: %v", path, err)
		}
		if *reps > 0 {
			cfg.Reps = *reps
		}
		if *warmup >= 0 {
			cfg.Warmup = *warmup
		}
		if *only != "" {
			want := map[string]bool{}
			for _, l := range strings.Split(*only, ",") {
				want[strings.TrimSpace(l)] = true
			}
			kept := cfg.Patches[:0]
			for _, p := range cfg.Patches {
				if want[p.Lang] || want[p.Name] {
					kept = append(kept, p)
				}
			}
			cfg.Patches = kept
		}
		if len(cfg.Patches) == 0 {
			return fail("no patches to run")
		}
		if err := ensureState(root); err != nil {
			return fail("%v", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		res, runErr := bench.Run(ctx, root, cfg, stderr)
		var md bytes.Buffer
		bench.Markdown(&md, res)
		dir := filepath.Join(root, ".aiguard", "bench")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "report.md"), md.Bytes(), 0o644)
		raw, _ := json.MarshalIndent(res, "", "  ")
		os.WriteFile(filepath.Join(dir, "results.json"), raw, 0o644)
		stdout.Write(md.Bytes())
		if runErr != nil {
			fmt.Fprintf(stderr, "ai-guard: benchmark stopped early: %v\n", runErr)
			return 3
		}
		return 0
	}
	return fail("unknown bench command %q (init, run, restore)", sub)
}
