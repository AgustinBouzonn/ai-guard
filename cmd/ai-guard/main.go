// Command ai-guard validates AI-generated changes in tiers: instant checks on
// the delta, bounded incremental checks around it, and remote offloading for
// everything too heavy for the workstation.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/gitx"
	"github.com/AgustinBouzonn/ai-guard/internal/pipeline"
	"github.com/AgustinBouzonn/ai-guard/internal/report"
	"github.com/AgustinBouzonn/ai-guard/internal/sysres"
	"github.com/AgustinBouzonn/ai-guard/internal/tools"
)

const version = "0.3.0"

const usage = `ai-guard ` + version + ` - tiered validation for AI-generated code

usage:
  ai-guard [check] [flags] [files...]   validate changed files (default command)
  ai-guard run [flags] -- cmd args...   run any command inside a memory/time budget
  ai-guard hook [check flags]           Claude Code Stop/SubagentStop hook: check the delta when
                                        a turn ends and hand the errors back to the agent
  ai-guard doctor                       show machine limits and the tools that would run
  ai-guard tools [install] [--dir DIR]  show or install the tools ai-guard can bring itself
                                        (the native TypeScript compiler, via npm)
  ai-guard selftest                     verify process containment on this machine
  ai-guard init                         write aiguard.json with the defaults
  ai-guard tier2 run|list|status|wait   queue and follow remote jobs (--config FILE as in check)
  ai-guard bench init|run|restore       measure against a legacy pipeline

check flags:
  --staged            only staged files
  --since REF         everything changed since REF (branch point), plus untracked
  --files a,b         explicit files (no git needed); also accepted as arguments
  --files-from FILE   newline-separated list; "-" reads stdin
  --tier N            highest local tier to run: 0 or 1 (default 1)
  --enable ID         also run an opt-in step for this run, e.g. ts.eslint
  --disable ID        skip a step for this run, e.g. ts.test
  --tier2             queue the remote tier after a local pass
  --no-tier2          never queue the remote tier
  --format F          agent (default) or json
  --warnings          include warnings
  --max-errors N      cap on reported diagnostics
  --config FILE       config file (default <root>/aiguard.json)
  --root DIR          repository root (default: found from the current directory)

run flags:
  --mem MB            kill the whole process tree past this much RAM (0 = no limit)
  --timeout SEC       kill it after this long (0 = no limit)
  --normal-priority   do not lower the CPU priority
  --shell "LINE"      run a shell command line instead of an argv
  (exit status: the command's own; 137 killed for memory, 124 for time)

exit status: 0 pass, 1 errors found, 2 usage or config error, 3 a check could not
complete, 4 nothing failed but some checks were deferred (see tier 2)
`

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	cmd := "check"
	if len(args) > 0 {
		switch args[0] {
		case "check", "run", "hook", "doctor", "tools", "selftest", "init", "tier2", "bench", "version", "help", "__hog", "__sleep", "__tree", "__tier2-job":
			cmd, args = args[0], args[1:]
		case "-h", "--help", "-help":
			cmd = "help"
		default:
			if !strings.HasPrefix(args[0], "-") && !looksLikeFile(args[0]) {
				fmt.Fprintf(stderr, "ai-guard: unknown command %q\n\n%s", args[0], usage)
				return 2
			}
		}
	}
	switch cmd {
	case "check":
		return cmdCheck(args, stdout, stderr)
	case "run":
		return cmdRun(args, stdout, stderr)
	case "hook":
		return cmdHook(args, stdout, stderr)
	case "doctor":
		return cmdDoctor(args, stdout, stderr)
	case "tools":
		return cmdTools(args, stdout, stderr)
	case "selftest":
		return cmdSelftest(stdout)
	case "init":
		return cmdInit(args, stdout, stderr)
	case "tier2":
		return cmdTier2(args, stdout, stderr)
	case "bench":
		return cmdBench(args, stdout, stderr)
	case "__tier2-job":
		return cmdTier2Job(args, stderr)
	case "__hog", "__sleep", "__tree":
		return helperChild(cmd, args)
	case "version":
		fmt.Fprintf(stdout, "ai-guard %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return 0
	}
	fmt.Fprint(stdout, usage)
	return 0
}

// looksLikeFile tells "ai-guard src/a.ts" (a file to check) from a mistyped command.
func looksLikeFile(arg string) bool {
	if strings.ContainsAny(arg, `/\.`) {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}

// stringList is a repeatable, comma-separated flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

// resolveRoot returns the repository root and whether it is a git work tree.
func resolveRoot(flagRoot string) (root string, isRepo bool, err error) {
	if flagRoot != "" {
		if root, err = filepath.Abs(flagRoot); err != nil {
			return "", false, err
		}
		_, statErr := os.Lstat(filepath.Join(root, ".git"))
		return root, statErr == nil, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, err
	}
	if root, err = gitx.Root(cwd); err == nil {
		return root, true, nil
	}
	return cwd, false, nil
}

// ensureState creates <root>/.aiguard with a .gitignore that hides it, so
// caches never show up as changes (least of all to our own change detection).
func ensureState(root string) error {
	dir := filepath.Join(root, ".aiguard")
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	return nil
}

// normalize maps user-supplied file arguments to repository-relative slash
// paths, keeping only regular files inside the repository.
func normalize(root string, inputs []string) []string {
	cwd, _ := os.Getwd()
	seen := map[string]struct{}{}
	var out []string
	for _, in := range inputs {
		p := filepath.FromSlash(in)
		if !filepath.IsAbs(p) {
			if c := filepath.Join(cwd, p); isRegular(c) {
				p = c
			} else {
				p = filepath.Join(root, p)
			}
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !isRegular(p) {
			continue
		}
		rel = filepath.ToSlash(rel)
		if _, dup := seen[rel]; !dup {
			seen[rel] = struct{}{}
			out = append(out, rel)
		}
	}
	return out
}

func isRegular(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func readList(path string) ([]string, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// lookPath is shared by every plan of the process: PATH is scanned once.
var lookPath = tools.NewLookPath()

func machineEnv(root string) tools.Env {
	return tools.Env{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LookPath: lookPath,
		CacheDir: filepath.Join(root, ".aiguard", "cache"), VirtualEnv: os.Getenv("VIRTUAL_ENV"), ToolsDir: toolsDir()}
}

func memMB() (availMB, totalMB int, ok bool) {
	m, err := sysres.Memory()
	if err != nil {
		return 0, 0, false
	}
	return int(m.AvailBytes >> 20), int(m.TotalBytes >> 20), true
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("check", stderr)
	var (
		files              stringList
		enable, disable    stringList
		filesFrom, since   string
		format, cfgPath    string
		rootFlag           string
		staged, warnings   bool
		forceT2, noT2      bool
		maxTier, maxErrors int
	)
	fs.Var(&files, "files", "")
	fs.Var(&enable, "enable", "")
	fs.Var(&disable, "disable", "")
	fs.StringVar(&filesFrom, "files-from", "", "")
	fs.StringVar(&since, "since", "", "")
	fs.BoolVar(&staged, "staged", false, "")
	fs.IntVar(&maxTier, "tier", 1, "")
	fs.BoolVar(&forceT2, "tier2", false, "")
	fs.BoolVar(&noT2, "no-tier2", false, "")
	fs.StringVar(&format, "format", "agent", "")
	fs.BoolVar(&warnings, "warnings", false, "")
	fs.IntVar(&maxErrors, "max-errors", 0, "")
	fs.StringVar(&cfgPath, "config", "", "")
	fs.StringVar(&rootFlag, "root", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "ai-guard: "+format+"\n", a...)
		return 2
	}
	if format != "agent" && format != "json" {
		return fail("--format must be agent or json, not %q", format)
	}
	if maxTier != 0 && maxTier != 1 {
		return fail("--tier must be 0 or 1")
	}
	root, isRepo, err := resolveRoot(rootFlag)
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := config.Load(root, cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	cfg.Enable = append(cfg.Enable, enable...)
	cfg.Disable = append(cfg.Disable, disable...)

	explicit := append(append([]string{}, files...), fs.Args()...)
	if filesFrom != "" {
		list, err := readList(filesFrom)
		if err != nil {
			return fail("--files-from: %v", err)
		}
		explicit = append(explicit, list...)
	}
	var changed []string
	switch {
	case len(explicit) > 0 || filesFrom != "":
		changed = normalize(root, explicit)
	case !isRepo:
		return fail("%s is not a git repository: run inside one, or name the files to check", root)
	default:
		opt := gitx.Options{}
		if staged {
			opt.Mode = gitx.Staged
		} else if since != "" {
			opt.Mode, opt.Ref = gitx.Since, since
		}
		if changed, err = gitx.Changed(root, opt); err != nil {
			return fail("%v", err)
		}
	}
	kept := changed[:0]
	for _, f := range changed {
		if !strings.HasPrefix(f, ".aiguard/") {
			kept = append(kept, f)
		}
	}
	changed = kept

	if len(changed) > 0 {
		if err := ensureState(root); err != nil {
			return fail("%v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t2 := 0
	if forceT2 {
		t2 = 1
	} else if noT2 {
		t2 = -1
	}
	res := pipeline.Run(ctx, pipeline.Options{
		Root: root, Files: changed, Cfg: cfg, MaxTier: maxTier, Tier2: t2,
		Env: machineEnv(root), NumCPU: runtime.NumCPU(), Mem: memMB, CPUBusy: sysres.CPUBusy,
		Trigger: func(reason string) (report.Tier2, error) { return queueTier2(root, cfg, cfgPath, reason) },
	})

	opt := report.Options{MaxErrors: cfg.Report.MaxErrors, MaxPerFile: cfg.Report.MaxPerFile, MaxMsg: cfg.Report.MaxMsg, Warnings: cfg.Report.Warnings || warnings}
	if maxErrors > 0 {
		opt.MaxErrors = maxErrors
	}
	if format == "json" {
		report.JSON(stdout, res, opt)
	} else {
		report.Agent(stdout, res, opt)
	}
	return res.ExitCode()
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("init", stderr)
	rootFlag := fs.String("root", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, _, err := resolveRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 2
	}
	path := filepath.Join(root, config.FileName)
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(stderr, "ai-guard: %s already exists; not overwriting\n", path)
		return 2
	}
	if err := os.WriteFile(path, config.DefaultJSON(), 0o644); err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 2
	}
	if err := ensureState(root); err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return 0
}
