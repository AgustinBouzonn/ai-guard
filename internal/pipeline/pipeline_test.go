package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/report"
	"github.com/AgustinBouzonn/ai-guard/internal/stats"
	"github.com/AgustinBouzonn/ai-guard/internal/tools"
)

// The test binary doubles as a configurable fake tool.
func TestMain(m *testing.M) {
	if os.Getenv("AIGUARD_FAKE_TOOL") == "1" && len(os.Args) > 1 {
		fakeTool(os.Args[1:])
	}
	os.Exit(m.Run())
}

func fakeTool(a []string) {
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	switch a[0] {
	case "say": // say <exit code> <text...>
		fmt.Println(strings.Join(a[2:], " "))
		os.Exit(n(a[1]))
	case "touch": // touch <file>
		os.WriteFile(a[1], []byte("ran"), 0o644)
		os.Exit(0)
	case "hog": // hog <MB> <hold ms>
		var keep [][]byte
		for i := 0; i < n(a[1])/4; i++ {
			b := make([]byte, 4<<20)
			for k := 0; k < len(b); k += 4096 {
				b[k] = 1
			}
			keep = append(keep, b)
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(time.Duration(n(a[2])) * time.Millisecond)
		os.Exit(len(keep) - len(keep))
	case "sleep":
		time.Sleep(time.Duration(n(a[1])) * time.Millisecond)
		os.Exit(0)
	case "env": // env <NAME>: print one variable
		fmt.Println(os.Getenv(a[1]))
		os.Exit(0)
	}
	os.Exit(64)
}

func self(args ...string) []string { return append([]string{os.Args[0]}, args...) }

type harness struct {
	t       *testing.T
	root    string
	cfg     *config.Config
	avail   atomic.Int64 // MiB the fake machine reports as free
	trigger []string     // reasons the tier-2 hook was called with
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	h := &harness{t: t, root: t.TempDir(), cfg: config.Default()}
	h.avail.Store(64000)
	h.cfg.Python.Lint, h.cfg.Python.Types, h.cfg.Python.Tests = "off", "off", "off" // only the steps a test declares
	h.cfg.LowPriority = false
	h.cfg.QueueWaitMs = 200
	return h
}

func (h *harness) step(id string, tier, phase int, argv ...string) {
	p := phase
	h.cfg.Steps = append(h.cfg.Steps, config.CustomStep{ID: id, Tier: tier, Phase: &p, Cmd: argv, Parser: "generic"})
}

func (h *harness) marker(name string) string { return filepath.Join(h.root, name) }

func (h *harness) ran(name string) bool {
	_, err := os.Stat(h.marker(name))
	return err == nil
}

func (h *harness) run(tier2 int, files ...string) report.Result {
	h.t.Helper()
	if len(files) == 0 {
		files = []string{"src/a.py"}
	}
	return Run(context.Background(), Options{
		Root: h.root, Files: files, Cfg: h.cfg, MaxTier: 1, Tier2: tier2, NumCPU: 4,
		Env:     tools.Env{GOOS: "linux", GOARCH: "amd64", CacheDir: filepath.Join(h.root, ".aiguard", "cache"), LookPath: func(string) (string, error) { return "", errors.New("none") }},
		Mem:     func() (int, int, bool) { return int(h.avail.Load()), 64000, true },
		CPUBusy: func(time.Duration) (float64, error) { return 0, nil },
		Trigger: func(reason string) (report.Tier2, error) {
			h.trigger = append(h.trigger, reason)
			return report.Tier2{ID: "job1", State: "queued", Log: "log"}, nil
		},
	})
}

func stepStatus(r report.Result, id string) string {
	for _, s := range r.Steps {
		if s.ID == id {
			return s.Status
		}
	}
	return "<absent>"
}

func TestTier0FailureStopsTheChain(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, self("say", "1", "src/a.py:3:1: error: boom")...)
	h.step("types", 1, 0, self("touch", h.marker("t1"))...)
	r := h.run(0)
	if r.Status != report.Fail {
		t.Fatalf("Status = %s, want fail; notes %v", r.Status, r.Notes)
	}
	if h.ran("t1") {
		t.Error("tier 1 ran although tier 0 failed")
	}
	if len(r.Tiers) != 1 || r.Tiers[0].N != 0 {
		t.Errorf("Tiers = %+v, want only tier 0", r.Tiers)
	}
	want := diag.D{File: "src/a.py", Line: 3, Col: 1, Msg: "boom"}
	if len(r.Diags) != 1 || r.Diags[0] != want {
		t.Errorf("Diags = %+v, want [%+v]", r.Diags, want)
	}
}

func TestCleanRunGoesThroughBothTiers(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, self("touch", h.marker("t0"))...)
	h.step("types", 1, 0, self("touch", h.marker("static"))...)
	h.step("tests", 1, 1, self("touch", h.marker("tests"))...)
	r := h.run(0)
	if r.Status != report.Pass {
		t.Fatalf("Status = %s, want pass; notes %v diags %v", r.Status, r.Notes, r.Diags)
	}
	for _, m := range []string{"t0", "static", "tests"} {
		if !h.ran(m) {
			t.Errorf("step %s did not run", m)
		}
	}
	if len(r.Tiers) != 2 || r.Tiers[0].BudgetMs != 300 || r.Tiers[1].BudgetMs != 3000 {
		t.Errorf("Tiers = %+v, want tier 0 and 1 with their 300/3000 ms targets", r.Tiers)
	}
	if got := stepStatus(r, "tests"); got != "ok" {
		t.Errorf("tests status = %q, want ok", got)
	}
}

func TestStaticFailureSkipsTests(t *testing.T) {
	h := newHarness(t)
	h.step("types", 1, 0, self("say", "2", "src/a.py:9:2: error: bad type")...)
	h.step("tests", 1, 1, self("touch", h.marker("tests"))...)
	r := h.run(0)
	if r.Status != report.Fail {
		t.Fatalf("Status = %s, want fail", r.Status)
	}
	if h.ran("tests") {
		t.Error("tests ran although static analysis already failed")
	}
}

func TestMaxTierZeroNeverStartsTier1(t *testing.T) {
	h := newHarness(t)
	h.step("types", 1, 0, self("touch", h.marker("t1"))...)
	r := Run(context.Background(), Options{Root: h.root, Files: []string{"a.py"}, Cfg: h.cfg, MaxTier: 0, NumCPU: 2,
		Env: tools.Env{GOOS: "linux", GOARCH: "amd64", LookPath: func(string) (string, error) { return "", errors.New("none") }}})
	if r.Status != report.Pass || h.ran("t1") {
		t.Errorf("Status = %s, tier-1 ran = %v; want pass without tier 1", r.Status, h.ran("t1"))
	}
}

func TestNoChangedFilesIsAnImmediatePass(t *testing.T) {
	h := newHarness(t)
	h.step("always", 0, 0, self("touch", h.marker("x"))...)
	r := Run(context.Background(), Options{Root: h.root, Cfg: h.cfg, MaxTier: 1, NumCPU: 2,
		Env: tools.Env{GOOS: "linux", LookPath: func(string) (string, error) { return "", errors.New("none") }}})
	if r.Status != report.Pass || len(r.Tiers) != 0 || h.ran("x") {
		t.Errorf("Status = %s, Tiers = %v, ran = %v; want an empty pass", r.Status, r.Tiers, h.ran("x"))
	}
}

func TestStepOverMemoryBudgetIsKilledAndDeferredToTier2(t *testing.T) {
	h := newHarness(t)
	h.step("types", 1, 0, self("hog", "400", "8000")...)
	h.cfg.Overrides = map[string]config.Override{"types": {MemMB: 96}}
	h.cfg.Tier2.Trigger = "on_defer"
	start := time.Now()
	r := h.run(0)
	if r.Status != report.Deferred {
		t.Fatalf("Status = %s, want deferred; notes %v", r.Status, r.Notes)
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %v: the step was not stopped at its budget", time.Since(start))
	}
	if got := stepStatus(r, "types"); got != "killed:mem" {
		t.Errorf("step status = %q, want killed:mem", got)
	}
	if len(r.Deferred) != 1 || !strings.HasPrefix(r.Deferred[0], "types: ") || !strings.Contains(r.Deferred[0], "96MB") {
		t.Errorf("Deferred = %q, want one entry naming the step and its 96MB budget", r.Deferred)
	}
	if len(h.trigger) != 1 || r.Tier2 == nil || r.Tier2.ID != "job1" {
		t.Errorf("tier-2 hook calls = %v, Tier2 = %+v; want exactly one queued job", h.trigger, r.Tier2)
	}
}

func TestStepThatNeverGetsRAMIsDeferredWithoutRunning(t *testing.T) {
	h := newHarness(t)
	h.avail.Store(300) // less than the reserve, whatever the step needs
	h.step("types", 1, 0, self("touch", h.marker("t1"))...)
	r := h.run(0)
	if r.Status != report.Deferred || h.ran("t1") {
		t.Fatalf("Status = %s, ran = %v; want deferred without starting the tool", r.Status, h.ran("t1"))
	}
	if len(r.Deferred) != 1 || !strings.Contains(r.Deferred[0], "free RAM") {
		t.Errorf("Deferred = %q, want the missing RAM as the reason", r.Deferred)
	}
	if len(h.trigger) != 0 {
		t.Errorf("tier 2 triggered %d times although tier2.trigger is off", len(h.trigger))
	}
}

func TestTier0TimeoutIsAnInfrastructureError(t *testing.T) {
	h := newHarness(t)
	h.step("slow", 0, 0, self("sleep", "60000")...)
	h.cfg.Overrides = map[string]config.Override{"slow": {TimeoutMs: 300}}
	r := h.run(0)
	if r.Status != report.Infra {
		t.Fatalf("Status = %s, want error", r.Status)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "slow") || !strings.Contains(r.Notes[0], "timeout") {
		t.Errorf("Notes = %q, want one note about the timeout of step slow", r.Notes)
	}
}

func TestToolCrashWithoutDiagnosticsIsReportedWithItsOutput(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, self("say", "2", "internal error: config file is malformed")...)
	r := h.run(0)
	if r.Status != report.Infra {
		t.Fatalf("Status = %s, want error (the code was never judged)", r.Status)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "lint exited 2") || !strings.Contains(r.Notes[0], "config file is malformed") {
		t.Errorf("Notes = %q, want the exit code and the tool's own message", r.Notes)
	}
}

func TestMissingExecutableIsReportedNotIgnored(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, "aiguard-no-such-tool")
	r := h.run(0)
	if r.Status != report.Infra || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "aiguard-no-such-tool") {
		t.Errorf("Status = %s, Notes = %q; want an error naming the missing tool", r.Status, r.Notes)
	}
}

func TestWarningsFailTheRunWhenTheToolItselfFails(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, self("say", "1", "src/a.py:1:1: warning: treated as error by the tool")...)
	r := h.run(0)
	if r.Status != report.Fail || len(r.Diags) != 1 || r.Diags[0].Sev != diag.Error {
		t.Errorf("Status = %s, Diags = %+v; a non-zero exit with only warnings must fail with them", r.Status, r.Diags)
	}
}

func TestWarningsAloneDoNotFail(t *testing.T) {
	h := newHarness(t)
	h.step("lint", 0, 0, self("say", "0", "src/a.py:1:1: warning: just a hint")...)
	r := h.run(0)
	if r.Status != report.Pass {
		t.Errorf("Status = %s, want pass: the tool exited 0", r.Status)
	}
}

func TestTier2PolicyOnPass(t *testing.T) {
	h := newHarness(t)
	h.cfg.Tier2.Trigger = "on_pass"
	h.step("lint", 0, 0, self("say", "0", "fine")...)
	if r := h.run(0); r.Status != report.Pass || len(h.trigger) != 1 || r.Tier2 == nil {
		t.Errorf("clean run: Status = %s, triggers = %d, Tier2 = %v; want one remote job", r.Status, len(h.trigger), r.Tier2)
	}

	h = newHarness(t)
	h.cfg.Tier2.Trigger = "on_pass"
	h.step("lint", 0, 0, self("say", "1", "a.py:1:1: error: x")...)
	if h.run(0); len(h.trigger) != 0 {
		t.Error("tier 2 was triggered for code that already fails locally")
	}

	h = newHarness(t)
	h.cfg.Tier2.Trigger = "on_pass"
	h.step("lint", 0, 0, self("say", "0", "fine")...)
	if h.run(-1); len(h.trigger) != 0 {
		t.Error("--no-tier2 did not suppress the trigger")
	}

	h = newHarness(t)
	h.cfg.Tier2.Trigger = "manual"
	h.step("lint", 0, 0, self("say", "0", "fine")...)
	if h.run(0); len(h.trigger) != 0 {
		t.Error("trigger=manual fired without --tier2")
	}
	if h.run(1); len(h.trigger) != 1 {
		t.Error("--tier2 did not fire a manual trigger")
	}
}

func TestStepsGetBudgetDerivedEnvironment(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--enable-source-maps")
	got := mergeEnv([]string{"A=1", "NODE_OPTIONS=--enable-source-maps"}, []string{"B=2"}, []string{"NODE_OPTIONS=--max-old-space-size=384"})
	want := map[string]string{"A": "1", "B": "2", "NODE_OPTIONS": "--enable-source-maps --max-old-space-size=384"}
	seen := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = v // later entries win, as they do for the child process
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q", k, seen[k], v)
		}
	}
}

func TestBloatedDaemonIsStoppedBeforeTheNextRun(t *testing.T) {
	h := newHarness(t)
	status := h.marker("daemon.json")
	os.WriteFile(status, []byte(fmt.Sprintf(`{"pid": %d, "connection_name": "x"}`, os.Getpid())), 0o644)
	s := &tools.Step{ID: "py.types", Daemon: &tools.Daemon{StatusFile: status, MaxRSSMB: 1, Stop: self("touch", h.marker("stopped"))}}
	note := recycleDaemon(context.Background(), s, h.root)
	if !h.ran("stopped") {
		t.Fatal("a daemon above its RSS ceiling was not stopped")
	}
	if !strings.Contains(note, "py.types") || !strings.Contains(note, "restarted") {
		t.Errorf("note = %q, want it to say the daemon was restarted", note)
	}

	s.Daemon.MaxRSSMB = 1 << 20
	os.Remove(h.marker("stopped"))
	if note := recycleDaemon(context.Background(), s, h.root); note != "" || h.ran("stopped") {
		t.Errorf("a daemon under its ceiling was stopped (note %q)", note)
	}
}

func TestSystemMemoryPressureStopsTheLargestStepAndDefersIt(t *testing.T) {
	h := newHarness(t)
	h.step("small", 1, 0, self("sleep", "1500")...)
	h.step("big", 1, 0, self("hog", "96", "8000")...)
	time.AfterFunc(600*time.Millisecond, func() { h.avail.Store(100) })   // something else ate the machine's RAM
	time.AfterFunc(900*time.Millisecond, func() { h.avail.Store(64000) }) // ...and stopping one step relieved it
	start := time.Now()
	r := h.run(0)
	if r.Status != report.Deferred {
		t.Fatalf("Status = %s, want deferred; notes %v", r.Status, r.Notes)
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %v: nothing reacted to the memory pressure", time.Since(start))
	}
	if got := stepStatus(r, "big"); got != "killed:pressure" {
		t.Errorf("big status = %q, want killed:pressure (it holds the most memory)", got)
	}
	if got := stepStatus(r, "small"); got != "ok" {
		t.Errorf("small status = %q, want ok: one victim must be enough", got)
	}
	if len(r.Deferred) != 1 || !strings.HasPrefix(r.Deferred[0], "big: ") || !strings.Contains(r.Deferred[0], "system RAM low") {
		t.Errorf("Deferred = %q", r.Deferred)
	}
}

func TestDaemonMemoryCountsAgainstItsClientStep(t *testing.T) {
	h := newHarness(t)
	daemon := exec.Command(os.Args[0], "hog", "200", "20000")
	daemon.Env = append(os.Environ(), "AIGUARD_FAKE_TOOL=1")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer daemon.Process.Kill()
	exited := make(chan struct{})
	go func() { daemon.Wait(); close(exited) }()

	status := h.marker("daemon.json")
	os.WriteFile(status, []byte(fmt.Sprintf(`{"pid": %d}`, daemon.Process.Pid)), 0o644)
	s := &tools.Step{ID: "py.types", Tier: 1, Argv: self("sleep", "8000"), Dir: h.root, Detached: true,
		Daemon: &tools.Daemon{StatusFile: status, MaxRSSMB: 96}}
	p := &run{o: Options{Root: h.root}, cfg: h.cfg}
	start := time.Now()
	out := p.exec(context.Background(), s, 1, func(int) {})
	if out.rec.Status != "killed:mem" || !strings.Contains(out.deferred, "96MB") {
		t.Fatalf("status = %q, deferred = %q; the daemon's 200 MB must break its 96 MB allowance", out.rec.Status, out.deferred)
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Error("the oversized daemon is still running")
	}
}

func TestDaemonStepReservesOnlyWhatTheDaemonHasNotUsedYet(t *testing.T) {
	h := newHarness(t)
	status := h.marker("daemon.json")
	s := &tools.Step{ID: "py.types", Tier: 1, Daemon: &tools.Daemon{StatusFile: status, MaxRSSMB: 1024}}
	p := &run{o: Options{Root: h.root}, cfg: h.cfg}
	if limit, need, _ := p.envelope(s, 1); limit != 1024 || need != 1024 {
		t.Errorf("no daemon yet: limit/need = %d/%d, want 1024/1024 (a cold start may grow to the cap)", limit, need)
	}
	os.WriteFile(status, []byte(fmt.Sprintf(`{"pid": %d}`, os.Getpid())), 0o644)
	limit, need, _ := p.envelope(s, 1)
	if limit != 1024 || need >= 1024 || need < 64 {
		t.Errorf("daemon running: limit/need = %d/%d, want the cap and a smaller reservation", limit, need)
	}
	h.cfg.Tier1.MemMB = 512
	plain := &tools.Step{ID: "ts.types", Tier: 1}
	if limit, need, _ := p.envelope(plain, 1); limit != 512 || need != 512 {
		t.Errorf("plain step with a configured budget: limit/need = %d/%d, want 512/512", limit, need)
	}
}

func TestTier1StepOverItsTimeAllowanceIsDeferredNotFailed(t *testing.T) {
	h := newHarness(t)
	h.step("tests", 1, 1, self("sleep", "60000")...)
	h.cfg.Overrides = map[string]config.Override{"tests": {TimeoutMs: 300}}
	start := time.Now()
	r := h.run(0)
	if r.Status != report.Deferred {
		t.Fatalf("Status = %s, want deferred: too slow for the workstation is tier-2 work, not an error; notes %v", r.Status, r.Notes)
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %v, the step was not stopped at 300ms", time.Since(start))
	}
	if got := stepStatus(r, "tests"); got != "killed:timeout" {
		t.Errorf("step status = %q, want killed:timeout", got)
	}
	if len(r.Deferred) != 1 || !strings.HasPrefix(r.Deferred[0], "tests: ") || !strings.Contains(r.Deferred[0], "300ms") {
		t.Errorf("Deferred = %q", r.Deferred)
	}
}

func TestStepThatDefersItselfWhilePreparingNeverRuns(t *testing.T) {
	h := newHarness(t)
	s := &tools.Step{ID: "go.test", Tier: 1, Argv: self("touch", h.marker("ran")), Dir: h.root}
	s.Prepare = func(tools.RunFunc) { s.Defer = "121 affected packages > go.max_test_packages=16" }
	p := &run{o: Options{Root: h.root}, cfg: h.cfg}
	out := p.exec(context.Background(), s, 1, func(int) {})
	if out.rec.Status != "deferred" || out.deferred != "go.test: 121 affected packages > go.max_test_packages=16" {
		t.Errorf("status = %q, deferred = %q", out.rec.Status, out.deferred)
	}
	if h.ran("ran") {
		t.Error("the step ran although it had deferred itself")
	}
}

func TestWrapPrefixesTier1CommandsWithTheBudgetExpanded(t *testing.T) {
	h := newHarness(t)
	// The wrapper prints one diagnostic naming everything it was asked to run.
	h.cfg.Wrap = self("say", "1", "src/a.py:1:1: error: wrapped mem={mem_mb}")
	h.step("lint", 0, 0, self("say", "0", "tier0-untouched")...)
	h.step("types", 1, 0, self("say", "0", "inner")...)
	h.cfg.Overrides = map[string]config.Override{"types": {MemMB: 768}}
	r := h.run(0)
	if r.Status != report.Fail || len(r.Diags) != 1 {
		t.Fatalf("Status = %s, Diags = %+v; want the single diagnostic printed by the wrapper", r.Status, r.Diags)
	}
	msg := r.Diags[0].Msg
	if !strings.HasPrefix(msg, "wrapped mem=768 ") || !strings.HasSuffix(msg, " say 0 inner") {
		t.Errorf("wrapper saw %q; want the expanded budget followed by the step's own command line", msg)
	}
	if got := stepStatus(r, "lint"); got != "ok" {
		t.Errorf("tier-0 step status = %q: tier 0 must not go through the wrapper", got)
	}
}

func TestAutomaticBudgetIsSizedFromTheMachineAndFromPastRuns(t *testing.T) {
	cfg := config.Default()
	book := stats.Load(filepath.Join(t.TempDir(), "stats.json"))
	envelope := func(totalMB int, id string, tier int) (limit, need int) {
		p := &run{cfg: cfg, hist: book, o: Options{Mem: func() (int, int, bool) { return totalMB, totalMB, true }}}
		limit, need, _ = p.envelope(&tools.Step{ID: id}, tier)
		return limit, need
	}
	// The cap is an eighth of the installed RAM, between 1 and 4 GB; a step
	// nobody has measured yet reserves half of it.
	for total, want := range map[int][2]int{4000: {1024, 512}, 16000: {2000, 1000}, 32000: {4000, 2000}, 128000: {4096, 2048}} {
		if limit, need := envelope(total, "ts.types", 1); limit != want[0] || need != want[1] {
			t.Errorf("%d MB machine: limit, need = %d, %d; want %d, %d", total, limit, need, want[0], want[1])
		}
	}
	// Once measured, it reserves a quarter above its usual peak; the cap stays,
	// so that a cold run is not killed for being unlike the warm ones.
	for _, mb := range []int{2329, 516, 520} {
		book.Record("ts.types", mb, 1000)
	}
	if limit, need := envelope(32000, "ts.types", 1); limit != 4000 || need != 650 {
		t.Errorf("after warm runs: limit, need = %d, %d; want 4000, 650", limit, need)
	}
	book.Record("tiny", 3, 10)
	book.Record("huge", 9000, 10)
	if _, need := envelope(32000, "tiny", 1); need != 128 {
		t.Errorf("tiny step reserves %d MB, want the 128 MB floor", need)
	}
	if _, need := envelope(32000, "huge", 1); need != 4000 {
		t.Errorf("huge step reserves %d MB, want no more than its cap", need)
	}
	// Tier 0 and explicit budgets are taken literally.
	if limit, need := envelope(32000, "ts.lint", 0); limit != 150 || need != 150 {
		t.Errorf("tier 0: limit, need = %d, %d; want 150, 150", limit, need)
	}
	cfg.Overrides = map[string]config.Override{"ts.types": {MemMB: 768}}
	if limit, need := envelope(32000, "ts.types", 1); limit != 768 || need != 768 {
		t.Errorf("override: limit, need = %d, %d; want 768, 768", limit, need)
	}
	cfg.Tier1.MemMB = 512
	if limit, need := envelope(32000, "tiny", 1); limit != 512 || need != 512 {
		t.Errorf("tier1.mem_mb = 512: limit, need = %d, %d; want the strict 512, 512", limit, need)
	}
}

func TestAStepIsAdmittedOnWhatItUsedBeforeNotOnItsCap(t *testing.T) {
	h := newHarness(t)
	h.step("types", 1, 0, self("hog", "64", "150")...)
	if r := h.run(0); r.Status != report.Pass {
		t.Fatalf("first run: %s, notes %v", r.Status, r.Notes)
	}
	book := stats.Load(filepath.Join(h.root, ".aiguard", "cache", "stats.json"))
	if mb, ok := book.Typical("types"); !ok || mb < 50 || mb > 400 {
		t.Fatalf("remembered peak = %d MB, %v; want roughly the 64 MB the step used", mb, ok)
	}
	// 64 GB machine: 2048 MB stay untouched and an unmeasured step would reserve
	// another 2048. With 3000 MB free only the measured one fits.
	h.avail.Store(3000)
	h.step("fresh", 1, 0, self("touch", h.marker("fresh"))...)
	r := h.run(0)
	if got := stepStatus(r, "types"); got != "ok" {
		t.Errorf("measured step = %q, want ok; deferred %v", got, r.Deferred)
	}
	if got := stepStatus(r, "fresh"); got != "deferred" || h.ran("fresh") {
		t.Errorf("unmeasured step = %q (ran %v), want deferred without starting", got, h.ran("fresh"))
	}
}

func TestThreadsFollowIdleCoresButNeverDropBelowAQuarterOfTheMachine(t *testing.T) {
	threads := func(busy float64, steps int) int {
		p := &run{cfg: config.Default(), o: Options{NumCPU: 16, CPUBusy: func(time.Duration) (float64, error) { return busy, nil }}}
		workers, idle := p.capacity()
		return threadsFor(workers, idle, steps)
	}
	for _, k := range []struct {
		busy        float64
		steps, want int
	}{
		{0, 1, 16},   // a lone step may use every idle core
		{0, 3, 5},    // three share them
		{0, 9, 4},    // only four run at once
		{0.95, 1, 4}, // a busy machine: tier 1 runs below normal priority, a quarter of the cores is fair
		{0.5, 2, 4},
	} {
		if got := threads(k.busy, k.steps); got != k.want {
			t.Errorf("busy %.2f, %d steps: %d threads each, want %d", k.busy, k.steps, got, k.want)
		}
	}
}
