// Package bench measures a legacy pipeline against AI-Guard on simulated
// edits: it applies a patch, runs one pipeline under measurement, restores the
// file, and repeats for the other pipeline.
package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/runner"
	"github.com/AgustinBouzonn/ai-guard/internal/sysres"
)

// FileName is the benchmark definition at the repository root.
const FileName = "aiguard.bench.json"

// Command is one step of a pipeline. "{file}" expands to the patched file
// (repository-relative) and "{abs}" to its absolute path.
type Command struct {
	Name  string   `json:"name"`
	Shell string   `json:"shell,omitempty"` // a line for the platform shell
	Cmd   []string `json:"cmd,omitempty"`   // or an argv
	Dir   string   `json:"dir,omitempty"`   // relative to the repository root
	Langs []string `json:"langs,omitempty"` // run only for patches of these families
	// DeferredExit is the exit status that means "nothing failed, but part of
	// the work was handed to the remote tier" (4 for ai-guard). 0 = none.
	DeferredExit int `json:"deferred_exit,omitempty"`
}

// Patch is one simulated edit. "{n}" expands to a number that is unique per
// run, so no run can be answered from a cache of the previous one.
type Patch struct {
	Name    string `json:"name"`
	Lang    string `json:"lang"`
	Kind    string `json:"kind"` // valid | syntax | type
	File    string `json:"file"`
	Append  string `json:"append,omitempty"`
	Find    string `json:"find,omitempty"`
	Replace string `json:"replace,omitempty"`
}

// Config is aiguard.bench.json.
type Config struct {
	Reps       int       `json:"reps"`
	Warmup     int       `json:"warmup"`
	FailFast   bool      `json:"baseline_fail_fast"` // stop the legacy chain at its first failure
	TimeoutSec int       `json:"timeout_sec"`        // per pipeline run
	Baseline   []Command `json:"baseline"`
	Optimized  Command   `json:"optimized"`
	Patches    []Patch   `json:"patches"`
}

// Sample is one measured pipeline run.
type Sample struct {
	WallMs   int64  `json:"wall_ms"`
	PeakMB   int    `json:"peak_mb"`
	CPUMs    int64  `json:"cpu_ms"`
	OK       bool   `json:"ok"`
	Deferred bool   `json:"deferred,omitempty"`
	Failed   string `json:"failed,omitempty"` // first command that failed
}

// Row holds every run of one patch.
type Row struct {
	Patch       Patch    `json:"patch"`
	BasePass    bool     `json:"baseline_pass"`
	OptPass     bool     `json:"aiguard_pass"`
	OptDeferred bool     `json:"aiguard_deferred,omitempty"`
	BaseRuns    []Sample `json:"baseline_runs"`
	OptRuns     []Sample `json:"aiguard_runs"`
	BaseFirst   Sample   `json:"baseline_first"`
	OptFirst    Sample   `json:"aiguard_first"`
}

// Results is a whole benchmark session.
type Results struct {
	Repo    string    `json:"repo"`
	When    time.Time `json:"when"`
	Machine string    `json:"machine"`
	Cores   int       `json:"cores"`
	Reps    int       `json:"reps"`
	Warmup  int       `json:"warmup"`
	Rows    []Row     `json:"rows"`
}

func expand(s, root, file string) string {
	return strings.NewReplacer("{file}", file, "{abs}", filepath.Join(root, filepath.FromSlash(file))).Replace(s)
}

// Measure runs a chain of commands and returns its total wall-clock and CPU
// time and the highest memory any command's process tree reached. extra names
// processes outside the tree whose memory also counts (persistent daemons).
func Measure(ctx context.Context, root string, chain []Command, file string, failFast bool, extra func() []int) Sample {
	s := Sample{OK: true}
	for _, c := range chain {
		spec := runner.Spec{Dir: root, KeepLeftovers: true, ExtraPIDs: extra}
		if c.Dir != "" {
			spec.Dir = filepath.Join(root, filepath.FromSlash(c.Dir))
		}
		if c.Shell != "" {
			spec.Shell = expand(c.Shell, root, file)
		} else {
			for _, a := range c.Cmd {
				spec.Argv = append(spec.Argv, expand(a, root, file))
			}
		}
		r := runner.Run(ctx, spec)
		s.WallMs += r.Wall.Milliseconds()
		s.CPUMs += r.CPU.Milliseconds()
		if mb := int(r.PeakRSS >> 20); mb > s.PeakMB {
			s.PeakMB = mb
		}
		if r.Err == nil && r.Killed == runner.NotKilled && c.DeferredExit != 0 && r.ExitCode == c.DeferredExit {
			s.Deferred = true
			continue
		}
		if r.Err != nil || r.ExitCode != 0 || r.Killed != runner.NotKilled {
			if s.OK {
				s.OK, s.Failed = false, c.Name
				if r.Killed != runner.NotKilled {
					s.Failed += " (" + r.Killed.String() + ")"
				} else if r.Err != nil {
					s.Failed += " (" + r.Err.Error() + ")"
				}
			}
			if failFast {
				break
			}
		}
	}
	return s
}

func forLang(cmds []Command, lang string) []Command {
	var out []Command
	for _, c := range cmds {
		if len(c.Langs) == 0 {
			out = append(out, c)
			continue
		}
		for _, l := range c.Langs {
			if l == lang {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// daemons returns a sampler for the pids of AI-Guard's persistent servers,
// re-reading their status files at most twice a second.
func daemons(root string) func() []int {
	var last time.Time
	var pids []int
	return func() []int {
		if time.Since(last) < 500*time.Millisecond {
			return pids
		}
		last, pids = time.Now(), pids[:0]
		names, _ := filepath.Glob(filepath.Join(root, ".aiguard", "cache", "dmypy-*.json"))
		for _, n := range names {
			var pid int
			if data, err := os.ReadFile(n); err == nil {
				if i := strings.Index(string(data), `"pid":`); i >= 0 {
					fmt.Sscanf(strings.TrimSpace(string(data)[i+6:]), "%d", &pid)
				}
			}
			if pid > 0 {
				pids = append(pids, pid)
			}
		}
		return pids
	}
}

// Run executes the benchmark. Every pipeline run sees a fresh, unique edit;
// the two pipelines alternate so drift affects both alike. The work tree is
// restored after every run, including on cancellation.
func Run(ctx context.Context, root string, cfg Config, progress io.Writer) (Results, error) {
	if progress == nil {
		progress = io.Discard
	}
	if n, err := Restore(root); err != nil {
		return Results{}, fmt.Errorf("cannot restore files from an interrupted run: %w", err)
	} else if n > 0 {
		fmt.Fprintf(progress, "restored %d file(s) left patched by an interrupted run\n", n)
	}
	if cfg.Reps < 1 {
		cfg.Reps = 1
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = 1800
	}
	res := Results{Repo: filepath.Base(root), When: time.Now().UTC(), Cores: runtime.NumCPU(), Reps: cfg.Reps, Warmup: cfg.Warmup}
	res.Machine = fmt.Sprintf("%s/%s, %d cores", runtime.GOOS, runtime.GOARCH, res.Cores)
	if m, err := sysres.Memory(); err == nil {
		res.Machine += fmt.Sprintf(", %d MB RAM", m.TotalBytes>>20)
	}
	serial := int(time.Now().Unix()%1000000) * 100 // edits differ between sessions too
	extra := daemons(root)
	for _, p := range cfg.Patches {
		base := forLang(cfg.Baseline, p.Lang)
		opt := []Command{cfg.Optimized}
		row := Row{Patch: p, BasePass: true, OptPass: true}
		for i := 0; i < cfg.Warmup+cfg.Reps; i++ {
			order := []bool{true, false} // true = baseline
			if i%2 == 1 {
				order = []bool{false, true}
			}
			for _, isBase := range order {
				serial++
				restore, err := Apply(root, p, serial)
				if err != nil {
					return res, err
				}
				runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSec)*time.Second)
				var s Sample
				if isBase {
					s = Measure(runCtx, root, base, p.File, cfg.FailFast, nil)
				} else {
					s = Measure(runCtx, root, opt, p.File, true, extra)
				}
				cancel()
				if err := restore(); err != nil {
					return res, fmt.Errorf("%s is still patched (run `ai-guard bench restore`): %w", p.File, err)
				}
				if ctx.Err() != nil {
					return res, ctx.Err()
				}
				name := "ai-guard"
				if isBase {
					name = "baseline"
				}
				note := s.Failed
				if s.Deferred {
					note = "deferred"
				}
				fmt.Fprintf(progress, "%-16s run %d/%d %-8s %6d ms %5d MB ok=%v %s\n", p.Name, i+1, cfg.Warmup+cfg.Reps, name, s.WallMs, s.PeakMB, s.OK, note)
				switch {
				case isBase && i == 0:
					row.BaseFirst = s
				case !isBase && i == 0:
					row.OptFirst = s
				}
				if i >= cfg.Warmup {
					if isBase {
						row.BaseRuns, row.BasePass = append(row.BaseRuns, s), row.BasePass && s.OK
					} else {
						row.OptRuns, row.OptPass = append(row.OptRuns, s), row.OptPass && s.OK
						row.OptDeferred = row.OptDeferred || s.Deferred
					}
				}
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64{}, xs...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func walls(ss []Sample) float64 {
	var xs []float64
	for _, s := range ss {
		xs = append(xs, float64(s.WallMs))
	}
	if m := median(xs); m >= 1 {
		return m
	}
	return 1
}

func peaks(ss []Sample) float64 {
	var xs []float64
	for _, s := range ss {
		xs = append(xs, float64(s.PeakMB))
	}
	return median(xs)
}

// cpuPct is the median share of the machine a pipeline kept busy.
func cpuPct(ss []Sample, cores int) float64 {
	var xs []float64
	for _, s := range ss {
		wall := float64(s.WallMs)
		if wall < 1 {
			wall = 1
		}
		xs = append(xs, float64(s.CPUMs)/(wall*float64(cores))*100)
	}
	return median(xs)
}

// Speedup is how many times faster AI-Guard returned its verdict.
func (r Row) Speedup() float64 { return walls(r.BaseRuns) / walls(r.OptRuns) }

func saved(base, opt float64) float64 {
	if base <= 0 {
		return 0
	}
	return (1 - opt/base) * 100
}

// RAMSaved is the reduction in peak memory, in percent.
func (r Row) RAMSaved() float64 { return saved(peaks(r.BaseRuns), peaks(r.OptRuns)) }
