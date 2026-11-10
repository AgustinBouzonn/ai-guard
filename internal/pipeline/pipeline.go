// Package pipeline runs the tiers: tier 0 on the changed files, then tier 1
// under the memory-aware scheduler, failing fast between them, and finally
// hands whatever could not run locally to tier 2.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/report"
	"github.com/AgustinBouzonn/ai-guard/internal/runner"
	"github.com/AgustinBouzonn/ai-guard/internal/sched"
	"github.com/AgustinBouzonn/ai-guard/internal/stats"
	"github.com/AgustinBouzonn/ai-guard/internal/tools"
)

// Options is everything a run needs; the machine probes are injectable.
type Options struct {
	Root    string
	Files   []string // changed files, repository-relative
	Cfg     *config.Config
	MaxTier int // highest local tier to run: 0 or 1
	Tier2   int // -1 never, 0 follow tier2.trigger, 1 force
	Env     tools.Env
	NumCPU  int
	// Mem reports free and installed RAM in MiB; ok=false disables RAM gating.
	Mem func() (availMB, totalMB int, ok bool)
	// CPUBusy samples the busy fraction of all cores over a window.
	CPUBusy func(time.Duration) (float64, error)
	// Trigger queues the remote job and returns its handle.
	Trigger func(reason string) (report.Tier2, error)
}

type outcome struct {
	rec      report.Step
	diags    []diag.D
	failed   bool   // the code has errors
	infra    string // the check could not complete
	info     string // worth telling, not a failure
	deferred string // did not run to completion for lack of memory
}

type run struct {
	o    Options
	cfg  *config.Config
	hist *stats.Book // what each step cost in earlier runs; nil = not learning
}

// Run executes the pipeline and returns the verdict.
func Run(ctx context.Context, o Options) report.Result {
	res := report.Result{Status: report.Pass, Files: len(o.Files)}
	if len(o.Files) == 0 {
		return res
	}
	if o.NumCPU < 1 {
		o.NumCPU = runtime.NumCPU()
	}
	p := &run{o: o, cfg: o.Cfg}
	if o.Env.CacheDir != "" {
		p.hist = stats.Load(filepath.Join(o.Env.CacheDir, "stats.json"))
		defer p.hist.Save() // best effort: a lost sample only costs a guess
	}
	plan := tools.Build(o.Root, o.Files, o.Cfg, o.Env)

	var outs []outcome
	collect := func(batch []outcome) (failed bool) {
		for _, out := range batch {
			outs = append(outs, out)
			failed = failed || out.failed
		}
		return failed
	}
	notes := func(pl tools.Plan, tier int) {
		for _, n := range pl.Notes {
			if n.Tier == tier {
				res.Notes = append(res.Notes, n.Text)
			}
		}
	}

	// Tier 0: cheap native tools on the delta. No RAM gate: each is tens of MB.
	failed := false
	if t0 := pick(plan.Steps, 0, -1); len(t0) > 0 || hasNotes(plan, 0) {
		start := time.Now()
		workers := o.NumCPU
		if workers > 8 {
			workers = 8
		}
		gate := sched.New(workers, 0, func() (int, bool) { return 0, false })
		failed = collect(p.runBatch(ctx, t0, 0, gate))
		notes(plan, 0)
		res.Tiers = append(res.Tiers, report.Tier{N: 0, Ms: time.Since(start).Milliseconds(), BudgetMs: int64(o.Cfg.Tier0.BudgetMs)})
	}

	// Tier 1: heavier checks, admitted one by one against free RAM.
	infraSoFar := false
	for _, out := range outs {
		infraSoFar = infraSoFar || out.infra != ""
	}
	if !failed && !infraSoFar && o.MaxTier >= 1 && (len(pick(plan.Steps, 1, -1)) > 0 || hasNotes(plan, 1)) {
		start := time.Now()
		workers, idle := p.capacity()
		// The same plan again, each phase with the threads its steps may use.
		phase := func(n int) []*tools.Step {
			env := o.Env
			env.Threads = threadsFor(workers, idle, len(pick(plan.Steps, 1, n)))
			return pick(tools.Build(o.Root, o.Files, o.Cfg, env).Steps, 1, n)
		}
		gate := sched.New(workers, p.minFree(), func() (int, bool) {
			if o.Mem == nil {
				return 0, false
			}
			avail, _, ok := o.Mem()
			return avail, ok
		})
		// Static analysis first; tests only run on code that type-checks.
		if failed = collect(p.runBatch(ctx, phase(0), 1, gate)); !failed {
			failed = collect(p.runBatch(ctx, phase(1), 1, gate))
		}
		notes(plan, 1)
		res.Tiers = append(res.Tiers, report.Tier{N: 1, Ms: time.Since(start).Milliseconds(), BudgetMs: int64(o.Cfg.Tier1.BudgetMs)})
	}

	infra := false
	for _, out := range outs {
		res.Steps = append(res.Steps, out.rec)
		res.Diags = append(res.Diags, out.diags...)
		if out.rec.PeakMB > res.PeakMB {
			res.PeakMB = out.rec.PeakMB
		}
		if out.infra != "" {
			infra = true
			res.Notes = append(res.Notes, out.infra)
		}
		if out.info != "" {
			res.Notes = append(res.Notes, out.info)
		}
		if out.deferred != "" {
			res.Deferred = append(res.Deferred, out.deferred)
		}
	}
	switch {
	case failed:
		res.Status = report.Fail
	case infra:
		res.Status = report.Infra
	case len(res.Deferred) > 0:
		res.Status = report.Deferred
	}

	if reason, ok := p.wantTier2(res); ok {
		if t2, err := o.Trigger(reason); err != nil {
			res.Notes = append(res.Notes, "tier2 trigger failed: "+err.Error())
		} else {
			res.Tier2 = &t2
		}
	}
	return res
}

func pick(steps []*tools.Step, tier, phase int) []*tools.Step {
	var out []*tools.Step
	for _, s := range steps {
		if s.Tier == tier && (phase < 0 || s.Phase == phase) {
			out = append(out, s)
		}
	}
	return out
}

func hasNotes(p tools.Plan, tier int) bool {
	for _, n := range p.Notes {
		if n.Tier == tier {
			return true
		}
	}
	return false
}

// capacity sizes tier 1 from idle cores: how many tools run at once, and how
// many cores they may share. Tier 1 runs below normal priority, so even on a
// busy machine a quarter of the cores is a fair claim: the OS takes them back
// the moment anything else wants them.
func (p *run) capacity() (workers, idle int) {
	busy := 0.0
	if p.o.CPUBusy != nil {
		if b, err := p.o.CPUBusy(25 * time.Millisecond); err == nil {
			busy = b
		}
	}
	idle = sched.AutoWorkers(p.o.NumCPU, busy, 0)
	if floor := p.o.NumCPU / 4; idle < floor {
		idle = floor
	}
	workers = p.cfg.MaxWorkers
	if workers <= 0 {
		limit := p.o.NumCPU / 2
		if limit > 4 {
			limit = 4
		}
		workers = sched.AutoWorkers(p.o.NumCPU, busy, limit)
	}
	return workers, idle
}

// threadsFor splits the idle cores among the steps that run side by side.
func threadsFor(workers, idle, steps int) int {
	if steps > workers {
		steps = workers
	}
	if steps < 1 {
		steps = 1
	}
	if idle < steps {
		return 1
	}
	return idle / steps
}

// AutoCapMB is the tier-1 memory limit of a step nobody budgeted: an eighth
// of the installed RAM, between 1 and 4 GB (2 GB when the RAM is unknown).
func AutoCapMB(totalMB int) int {
	switch mb := totalMB / 8; {
	case totalMB <= 0:
		return 2048
	case mb < 1024:
		return 1024
	case mb > 4096:
		return 4096
	default:
		return mb
	}
}

func (p *run) autoCap() int {
	total := 0
	if p.o.Mem != nil {
		if _, t, ok := p.o.Mem(); ok {
			total = t
		}
	}
	return AutoCapMB(total)
}

func (p *run) minFree() int {
	if p.o.Mem == nil {
		return 0
	}
	_, total, ok := p.o.Mem()
	if !ok {
		return 0
	}
	return p.cfg.MinFree(total)
}

// live is the scheduler-visible state of one step in a batch.
type live struct {
	cancel   context.CancelFunc
	running  atomic.Bool
	rssMB    atomic.Int64
	pressure atomic.Bool  // stopped by the guard
	freeMB   atomic.Int64 // free RAM that made the guard act
}

// runBatch runs steps concurrently through the gate. The first step that
// finds errors cancels the rest: more output would not change the verdict.
func (p *run) runBatch(ctx context.Context, steps []*tools.Step, tier int, gate *sched.Gate) []outcome {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	outs := make([]outcome, len(steps))
	lives := make([]*live, len(steps))
	for i := range lives {
		lives[i] = &live{}
	}
	if tier >= 1 {
		go p.guard(ctx, lives)
	}
	wait := time.Duration(p.cfg.QueueWaitMs) * time.Millisecond
	var wg sync.WaitGroup
	for i, s := range steps {
		wg.Add(1)
		go func(i int, s *tools.Step) {
			defer wg.Done()
			l := lives[i]
			_, needMB, _ := p.envelope(s, tier)
			outs[i].rec = report.Step{ID: s.ID, Tier: tier}
			tk, err := gate.Acquire(ctx, needMB, wait)
			switch {
			case errors.Is(err, sched.ErrNoMemory):
				outs[i].rec.Status = "deferred"
				outs[i].deferred = fmt.Sprintf("%s: no free RAM for its %dMB budget", s.ID, needMB)
				return
			case err != nil:
				outs[i].rec.Status = "cancelled"
				return
			}
			defer tk.Release()
			stepCtx, stop := context.WithCancel(ctx)
			defer stop()
			l.cancel = stop
			l.running.Store(true)
			outs[i] = p.exec(stepCtx, s, tier, func(mb int) {
				tk.Report(mb)
				l.rssMB.Store(int64(mb))
			})
			l.running.Store(false)
			if st := outs[i].rec.Status; tier >= 1 && s.Daemon == nil && (st == "ok" || st == "fail" || st == "killed:mem") {
				p.hist.Record(s.ID, outs[i].rec.PeakMB, outs[i].rec.Ms)
			}
			if l.pressure.Load() {
				outs[i].rec.Status, outs[i].failed, outs[i].infra = "killed:pressure", false, ""
				outs[i].diags = nil
				outs[i].deferred = fmt.Sprintf("%s: stopped, system RAM low (%dMB free)", s.ID, l.freeMB.Load())
			}
			if outs[i].failed {
				cancel()
			}
		}(i, s)
	}
	wg.Wait()
	return outs
}

// guard protects the machine rather than a budget: when free RAM drops under
// half the reserve (whoever is using it), the step holding the most memory is
// stopped and deferred. Admission control keeps new steps out meanwhile.
func (p *run) guard(ctx context.Context, lives []*live) {
	floor := p.minFree() / 2
	if floor <= 0 || p.o.Mem == nil {
		return
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		avail, _, ok := p.o.Mem()
		if !ok || avail >= floor {
			continue
		}
		var victim *live
		for _, l := range lives {
			if l.running.Load() && !l.pressure.Load() && (victim == nil || l.rssMB.Load() > victim.rssMB.Load()) {
				victim = l
			}
		}
		if victim == nil {
			continue
		}
		victim.freeMB.Store(int64(avail))
		victim.pressure.Store(true)
		victim.cancel()
		select { // let the OS reclaim before judging again
		case <-ctx.Done():
			return
		case <-time.After(750 * time.Millisecond):
		}
	}
}

// daemonPID reads the server pid from a daemon status file (0 = none).
func daemonPID(statusFile string) int {
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return 0
	}
	var st struct {
		Pid int `json:"pid"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&st) != nil {
		return 0
	}
	return st.Pid
}

// envelope returns a step's watchdog limit, the RAM to set aside when it is
// admitted, and its timeout. A configured budget is both limit and
// reservation. Without one the limit comes from the machine and the
// reservation from the step's own history: a quarter above its usual peak,
// or half the limit before it has ever been measured. The limit does not
// shrink with history, or a cold run would be killed for not being warm.
// For a daemon client the limit is the daemon's allowance, and memory the
// daemon already holds is not reserved twice.
func (p *run) envelope(s *tools.Step, tier int) (limitMB, needMB int, timeout time.Duration) {
	limitMB, timeout = p.cfg.Budget(s.ID, tier)
	needMB = limitMB
	if limitMB <= 0 {
		limitMB = p.autoCap()
		needMB = limitMB / 2
		if usual, ok := p.hist.Typical(s.ID); ok {
			if needMB = usual * 5 / 4; needMB < 128 {
				needMB = 128
			}
		}
		if needMB > limitMB {
			needMB = limitMB
		}
	}
	if s.Daemon == nil {
		return limitMB, needMB, timeout
	}
	if s.Daemon.MaxRSSMB > 0 {
		limitMB = s.Daemon.MaxRSSMB
	}
	needMB = limitMB
	if pid := daemonPID(s.Daemon.StatusFile); pid > 0 {
		if rss, ok := runner.ProcessRSS(pid); ok {
			needMB -= int(rss >> 20)
		}
	}
	if needMB < 64 {
		needMB = 64
	}
	return limitMB, needMB, timeout
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// exec runs one step inside its resource envelope and classifies the result.
func (p *run) exec(ctx context.Context, s *tools.Step, tier int, onRSS func(mb int)) outcome {
	memMB, _, timeout := p.envelope(s, tier)
	out := outcome{rec: report.Step{ID: s.ID, Tier: tier}}
	start := time.Now()
	if s.Prepare != nil {
		s.Prepare(func(argv []string, dir string) ([]byte, error) {
			r := runner.Run(ctx, runner.Spec{Argv: argv, Dir: dir, Timeout: 30 * time.Second, MemLimitMB: memMB})
			if r.Err != nil {
				return nil, r.Err
			}
			if r.ExitCode != 0 {
				return r.Stdout, fmt.Errorf("exit %d", r.ExitCode)
			}
			return r.Stdout, nil
		})
	}
	if s.Defer != "" {
		out.rec.Ms = time.Since(start).Milliseconds()
		out.rec.Status, out.deferred = "deferred", s.ID+": "+s.Defer
		return out
	}
	if s.Daemon != nil {
		out.info = recycleDaemon(ctx, s, p.o.Root)
	}
	var memEnv []string
	if s.MemEnv != nil {
		memEnv = s.MemEnv(memMB)
	}
	spec := runner.Spec{
		Argv: s.Argv, Shell: s.Shell, Dir: s.Dir, Env: mergeEnv(os.Environ(), s.Env, memEnv),
		MemLimitMB: memMB, Timeout: timeout, Detached: s.Detached,
		OnSample: func(rss uint64) { onRSS(int(rss >> 20)) },
	}
	if s.Daemon != nil {
		status := s.Daemon.StatusFile
		spec.ExtraPIDs = func() []int { // the server does the work: its memory is this step's memory
			if pid := daemonPID(status); pid > 0 {
				return []int{pid}
			}
			return nil
		}
	}
	if tier >= 1 {
		spec.LowPriority = p.cfg.LowPriority
		if !s.Detached {
			spec.HardCapMB = int(float64(memMB) * p.cfg.HardCapFactor)
			spec.RlimitASMB = p.cfg.RlimitASMB
			if len(p.cfg.Wrap) > 0 && len(s.Argv) > 0 {
				wrap := make([]string, len(p.cfg.Wrap))
				for i, w := range p.cfg.Wrap {
					wrap[i] = strings.ReplaceAll(w, "{mem_mb}", fmt.Sprint(memMB))
				}
				spec.Argv = append(wrap, s.Argv...)
			}
		}
	}
	r := runner.Run(ctx, spec)
	out.rec.Ms = time.Since(start).Milliseconds()
	out.rec.PeakMB = int(r.PeakRSS >> 20)

	switch {
	case r.Err != nil:
		name := s.Shell
		if len(spec.Argv) > 0 {
			name = spec.Argv[0]
		}
		out.rec.Status = "error"
		out.infra = fmt.Sprintf("%s: cannot run %s: %v", s.ID, name, r.Err)
	case r.Killed == runner.KilledMem:
		out.rec.Status = "killed:mem"
		if out.rec.PeakMB > memMB {
			out.deferred = fmt.Sprintf("%s: rss %dMB > %dMB budget", s.ID, out.rec.PeakMB, memMB)
		} else {
			out.deferred = fmt.Sprintf("%s: hit the OS memory cap of its %dMB budget", s.ID, memMB)
		}
	case r.Killed == runner.KilledTimeout:
		out.rec.Status = "killed:timeout"
		if tier >= 1 { // too slow for the workstation is tier-2 work, not a malfunction
			out.deferred = fmt.Sprintf("%s: still running after %s", s.ID, timeout)
		} else {
			out.infra = fmt.Sprintf("%s killed: timeout after %s", s.ID, timeout)
		}
	case r.Killed == runner.KilledCancel:
		out.rec.Status = "cancelled"
	default:
		var text []byte
		switch s.Stream {
		case tools.Stdout:
			text = r.Stdout
		case tools.Stderr:
			text = r.Stderr
		default:
			text = append(append(append([]byte{}, r.Stdout...), '\n'), r.Stderr...)
		}
		parse := s.Parser
		if parse == nil {
			parse = diag.Generic
		}
		ds := parse(reANSI.ReplaceAll(text, nil), diag.Ctx{Root: p.o.Root, Cwd: s.Dir, PkgDir: s.PkgDir})
		errs := 0
		for _, d := range ds {
			if d.Sev == diag.Error {
				errs++
			}
		}
		exitOK := r.ExitCode == 0
		for _, c := range s.OKExit {
			exitOK = exitOK || r.ExitCode == c
		}
		if !exitOK && errs == 0 {
			if len(ds) > 0 { // the tool treats its warnings as fatal: so do we
				for i := range ds {
					ds[i].Sev = diag.Error
				}
				errs = len(ds)
			} else {
				out.rec.Status = "error"
				out.infra = fmt.Sprintf("%s exited %d: %s", s.ID, r.ExitCode, tail(r.Stderr, r.Stdout))
			}
		}
		out.diags = ds
		if errs > 0 {
			out.rec.Status, out.failed = "fail", true
		} else if out.rec.Status == "" {
			out.rec.Status = "ok"
		}
	}
	return out
}

// tail returns the last lines a tool printed, as one line.
func tail(streams ...[]byte) string {
	for _, b := range streams {
		var keep []string
		ls := strings.Split(string(reANSI.ReplaceAll(b, nil)), "\n")
		for i := len(ls) - 1; i >= 0 && len(keep) < 3; i-- {
			if l := strings.TrimSpace(ls[i]); l != "" {
				keep = append([]string{l}, keep...)
			}
		}
		if len(keep) > 0 {
			s := strings.Join(keep, " | ")
			if r := []rune(s); len(r) > 300 {
				s = string(r[:300]) + "…"
			}
			return s
		}
	}
	return "no output"
}

// appendVars are flag lists: our addition must not replace the user's own.
var appendVars = map[string]bool{"NODE_OPTIONS": true, "GOFLAGS": true}

// mergeEnv layers step and budget variables over the base environment. The
// child sees the last occurrence of a name, so additions go at the end.
func mergeEnv(base, extra, mem []string) []string {
	out := append([]string{}, base...)
	for _, kv := range append(append([]string{}, extra...), mem...) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if appendVars[k] {
			for i := len(out) - 1; i >= 0; i-- {
				if ek, ev, _ := strings.Cut(out[i], "="); strings.EqualFold(ek, k) {
					if ev != "" {
						v = ev + " " + v
					}
					break
				}
			}
		}
		out = append(out, k+"="+v)
	}
	return out
}

// recycleDaemon stops a tool server that has outgrown its allowance; the
// client step then starts a fresh one. Returns a note when it did.
func recycleDaemon(ctx context.Context, s *tools.Step, root string) string {
	pid := daemonPID(s.Daemon.StatusFile)
	if pid <= 0 {
		return ""
	}
	rss, ok := runner.ProcessRSS(pid)
	if !ok || int(rss>>20) <= s.Daemon.MaxRSSMB || len(s.Daemon.Stop) == 0 {
		return ""
	}
	dir := s.Dir
	if dir == "" {
		dir = root
	}
	runner.Run(ctx, runner.Spec{Argv: s.Daemon.Stop, Dir: dir, Timeout: 15 * time.Second})
	return fmt.Sprintf("%s: daemon restarted (rss %dMB > %dMB)", s.ID, rss>>20, s.Daemon.MaxRSSMB)
}

// wantTier2 applies the trigger policy to the local verdict.
func (p *run) wantTier2(res report.Result) (reason string, ok bool) {
	if p.o.Trigger == nil || p.o.Tier2 < 0 || res.Status == report.Fail || res.Status == report.Infra {
		return "", false
	}
	deferred := len(res.Deferred) > 0
	switch {
	case p.o.Tier2 > 0:
		reason = "requested"
	case deferred && (p.cfg.Tier2.Trigger == "on_defer" || p.cfg.Tier2.Trigger == "on_pass" || p.cfg.Tier2.Trigger == "always"):
		reason = "deferred: " + strings.Join(res.Deferred, "; ")
	case p.cfg.Tier2.Trigger == "on_pass" || p.cfg.Tier2.Trigger == "always":
		reason = "local tiers passed"
	default:
		return "", false
	}
	return reason, true
}
