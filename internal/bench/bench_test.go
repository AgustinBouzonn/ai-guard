package bench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the "legacy" and "optimized" pipelines.
func TestMain(m *testing.M) {
	if os.Getenv("AIGUARD_FAKE_TOOL") == "1" && len(os.Args) > 1 {
		a := os.Args[1:]
		n := func(i int) int { v, _ := strconv.Atoi(a[i]); return v }
		switch a[0] {
		case "check": // check <ms> <MB> <file>: work, then fail if the file contains BROKEN
			buf := make([]byte, n(2)<<20)
			for i := 0; i < len(buf); i += 4096 {
				buf[i] = 1
			}
			end := time.Now().Add(time.Duration(n(1)) * time.Millisecond)
			for time.Now().Before(end) { // burn CPU so the load figure is not zero
			}
			data, _ := os.ReadFile(a[3])
			if bytes.Contains(data, []byte("BROKEN")) {
				fmt.Println("found BROKEN", buf[0])
				os.Exit(1)
			}
			os.Exit(0)
		case "exit":
			os.Exit(n(1))
		case "touch":
			os.WriteFile(a[1], []byte("x"), 0o644)
			os.Exit(0)
		}
		os.Exit(64)
	}
	os.Exit(m.Run())
}

func self(args ...string) []string { return append([]string{os.Args[0]}, args...) }

func project(t *testing.T) (root, file string) {
	t.Helper()
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	root = t.TempDir()
	file = filepath.Join(root, "src", "mod.py")
	os.MkdirAll(filepath.Dir(file), 0o755)
	if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, file
}

func content(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyAppendsUniqueTextAndRestoreIsExact(t *testing.T) {
	root, file := project(t)
	restore, err := Apply(root, Patch{File: "src/mod.py", Append: "\ny{n} = {n}\n"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := content(t, file); got != "x = 1\n\ny7 = 7\n" {
		t.Errorf("patched file = %q", got)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if got := content(t, file); got != "x = 1\n" {
		t.Errorf("restored file = %q, want the original bytes", got)
	}
	if n, _ := Restore(root); n != 0 {
		t.Errorf("Restore found %d pending files after a clean restore", n)
	}
}

func TestApplyFindReplace(t *testing.T) {
	root, file := project(t)
	restore, err := Apply(root, Patch{File: "src/mod.py", Find: "x = 1", Replace: "x = {n}"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if got := content(t, file); got != "x = 3\n" {
		t.Errorf("patched file = %q", got)
	}
	if _, err := Apply(root, Patch{File: "src/mod.py", Find: "not there", Replace: "y"}, 1); err == nil {
		t.Error("Apply accepted a patch whose anchor text is missing")
	}
}

func TestRestoreRecoversFilesLeftPatchedByACrash(t *testing.T) {
	root, file := project(t)
	if _, err := Apply(root, Patch{File: "src/mod.py", Append: "BROKEN\n"}, 1); err != nil {
		t.Fatal(err)
	}
	// the process "dies" here: the restore function is never called
	n, err := Restore(root)
	if err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v; want 1 file put back", n, err)
	}
	if got := content(t, file); got != "x = 1\n" {
		t.Errorf("file after crash recovery = %q", got)
	}
}

func TestApplyRefusesToStackOnAnUnrestoredPatch(t *testing.T) {
	root, _ := project(t)
	if _, err := Apply(root, Patch{File: "src/mod.py", Append: "a\n"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(root, Patch{File: "src/mod.py", Append: "b\n"}, 2); err == nil {
		t.Error("a second patch was stacked on a file that is still patched: its backup would no longer be the original")
	}
}

func TestMeasureAggregatesAChainAndStopsAtTheFirstFailure(t *testing.T) {
	root, _ := project(t)
	marker := filepath.Join(root, "marker")
	chain := []Command{
		{Name: "work", Cmd: self("check", "150", "96", "{file}")},
		{Name: "boom", Cmd: self("exit", "2")},
		{Name: "after", Cmd: self("touch", marker)},
	}
	s := Measure(context.Background(), root, chain, "src/mod.py", true, nil)
	if s.OK || s.Failed != "boom" {
		t.Errorf("OK = %v, Failed = %q; want a failure attributed to boom", s.OK, s.Failed)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the chain went on after a failure although fail-fast is on")
	}
	if s.WallMs < 150 || s.WallMs > 5000 {
		t.Errorf("WallMs = %d, want at least the 150 ms of work", s.WallMs)
	}
	if s.PeakMB < 90 || s.PeakMB > 400 {
		t.Errorf("PeakMB = %d, want about the 96 MB the first command held", s.PeakMB)
	}
	if s.CPUMs < 100 {
		t.Errorf("CPUMs = %d, want roughly the 150 ms the command spent spinning", s.CPUMs)
	}

	s = Measure(context.Background(), root, chain, "src/mod.py", false, nil)
	if _, err := os.Stat(marker); err != nil {
		t.Error("without fail-fast every command must run")
	}
	if s.OK {
		t.Error("a chain with a failing command reported OK")
	}
}

func TestRunComparesBothModesAndRestoresTheTree(t *testing.T) {
	root, file := project(t)
	cfg := Config{
		Reps: 2, Warmup: 1, FailFast: true, TimeoutSec: 60,
		Baseline:  []Command{{Name: "legacy", Cmd: self("check", "400", "128", "{file}")}},
		Optimized: Command{Name: "ai-guard", Cmd: self("check", "40", "16", "{file}")},
		Patches: []Patch{
			{Name: "py-valid", Lang: "py", Kind: "valid", File: "src/mod.py", Append: "\ny{n} = {n}\n"},
			{Name: "py-syntax", Lang: "py", Kind: "syntax", File: "src/mod.py", Append: "\nBROKEN {n}\n"},
		},
	}
	res, err := Run(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := content(t, file); got != "x = 1\n" {
		t.Fatalf("the benchmark left the file modified: %q", got)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("%d rows, want 2", len(res.Rows))
	}
	for _, r := range res.Rows {
		if len(r.BaseRuns) != 2 || len(r.OptRuns) != 2 {
			t.Errorf("%s: %d/%d measured runs, want 2/2 (the warm-up is reported apart)", r.Patch.Name, len(r.BaseRuns), len(r.OptRuns))
		}
		lo := 3.0
		if raceBuild {
			lo = 1.05 // still faster, but process start-up dominates
		}
		if sp := r.Speedup(); sp < lo || sp > 40 {
			t.Errorf("%s: speedup = %.1fx, want roughly 10x (400 ms vs 40 ms)", r.Patch.Name, sp)
		}
		if saved := r.RAMSaved(); saved < 40 || saved > 99 {
			t.Errorf("%s: RAM saved = %.0f%%, want well above 40%% (128 MB vs 16 MB)", r.Patch.Name, saved)
		}
	}
	valid, broken := res.Rows[0], res.Rows[1]
	if !valid.BasePass || !valid.OptPass {
		t.Errorf("valid patch: verdicts base=%v opt=%v, want both to pass", valid.BasePass, valid.OptPass)
	}
	if broken.BasePass || broken.OptPass {
		t.Errorf("broken patch: verdicts base=%v opt=%v, want both to fail", broken.BasePass, broken.OptPass)
	}
}

func TestRunReportsWhenTheModesDisagree(t *testing.T) {
	root, _ := project(t)
	cfg := Config{
		Reps: 1, FailFast: true, TimeoutSec: 60,
		Baseline:  []Command{{Name: "legacy", Cmd: self("exit", "0")}}, // blind to the breakage
		Optimized: Command{Name: "ai-guard", Cmd: self("check", "1", "1", "{file}")},
		Patches:   []Patch{{Name: "py-type", Lang: "py", Kind: "type", File: "src/mod.py", Append: "\nBROKEN\n"}},
	}
	res, err := Run(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var md bytes.Buffer
	Markdown(&md, res)
	if r := res.Rows[0]; !r.BasePass || r.OptPass {
		t.Fatalf("verdicts base=%v opt=%v", r.BasePass, r.OptPass)
	}
	if !strings.Contains(md.String(), "pass / FAIL ⚠") {
		t.Errorf("the table hides that the two pipelines disagree:\n%s", md.String())
	}
}

func TestBaselineCommandsAreFilteredByLanguage(t *testing.T) {
	root, _ := project(t)
	marker := filepath.Join(root, "ts-ran")
	cfg := Config{
		Reps: 1, FailFast: true, TimeoutSec: 60,
		Baseline: []Command{
			{Name: "eslint", Langs: []string{"ts"}, Cmd: self("touch", marker)},
			{Name: "flake8", Langs: []string{"py"}, Cmd: self("exit", "0")},
		},
		Optimized: Command{Name: "ai-guard", Cmd: self("exit", "0")},
		Patches:   []Patch{{Name: "py-valid", Lang: "py", Kind: "valid", File: "src/mod.py", Append: "\n# {n}\n"}},
	}
	if _, err := Run(context.Background(), root, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the TypeScript baseline ran for a Python patch")
	}
}

func sample(wall int64, mb int, cpu int64) Sample {
	return Sample{WallMs: wall, PeakMB: mb, CPUMs: cpu, OK: true}
}

func TestMarkdownTable(t *testing.T) {
	res := Results{
		Repo: "demo", When: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), Machine: "linux/amd64, 8 cores, 16000 MB RAM", Cores: 8, Reps: 3, Warmup: 1,
		Rows: []Row{
			{Patch: Patch{Name: "ts-valid", Lang: "ts", Kind: "valid", File: "src/a.ts"}, BasePass: true, OptPass: true,
				BaseRuns:  []Sample{sample(8000, 900, 16000), sample(9000, 1000, 18000), sample(10000, 800, 20000)},
				OptRuns:   []Sample{sample(900, 240, 900), sample(1100, 250, 1100), sample(1000, 260, 1000)},
				BaseFirst: sample(12000, 1000, 24000), OptFirst: sample(3000, 300, 3000)},
			{Patch: Patch{Name: "py-syntax", Lang: "py", Kind: "syntax", File: "app/x.py"}, BasePass: false, OptPass: false,
				BaseRuns: []Sample{sample(4000, 400, 4000)}, OptRuns: []Sample{sample(40, 20, 40)},
				BaseFirst: sample(4000, 400, 4000), OptFirst: sample(50, 20, 50)},
		},
	}
	var b bytes.Buffer
	Markdown(&b, res)
	want := `# AI-Guard benchmark: demo

2026-11-10 12:00 UTC · linux/amd64, 8 cores, 16000 MB RAM · 3 measured run(s) per patch after 1 warm-up · medians

| Patch | File | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB | RAM saved | Baseline CPU | AI-Guard CPU | Verdict (baseline / AI-Guard) |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|:---:|
| ts-valid | src/a.ts | 9000 | 1000 | 9.0x | 900 | 250 | 72.2% | 25% | 13% | pass / pass |
| py-syntax | app/x.py | 4000 | 40 | 100.0x | 400 | 20 | 95.0% | 13% | 13% | FAIL / FAIL |
| **ts** (1 patch) | | 9000 | 1000 | **9.0x** | 900 | 250 | **72.2%** | | | |
| **py** (1 patch) | | 4000 | 40 | **100.0x** | 400 | 20 | **95.0%** | | | |
| **all** (2 patches) | | 13000 | 1040 | **30.0x** | 900 | 250 | **72.2%** | | | |

First run of each patch (cold caches, not included above):

| Patch | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB |
|---|---:|---:|---:|---:|---:|
| ts-valid | 12000 | 3000 | 4.0x | 1000 | 300 |
| py-syntax | 4000 | 50 | 80.0x | 400 | 20 |

- ms: wall-clock until the pipeline returns its verdict. Group rows add the patch medians; their speedup is the geometric mean of the patch speedups.
- RSS: peak resident memory of the whole process tree (for AI-Guard, plus any persistent type-check daemon). Group rows show the highest peak.
- CPU: CPU-seconds divided by wall-clock and by the 8 logical cores (100% = every core busy for the whole run).
- Verdict: whether each pipeline accepted the patch; ⚠ marks patches on which they disagree; "defer" means AI-Guard found no error but handed part of the work to the remote tier instead of running it locally.
`
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestInitPlansPatchesAndALegacyBaselinePerLanguage(t *testing.T) {
	root := t.TempDir()
	files := map[string]int{ // path -> size in bytes
		"package.json": 2, "tsconfig.json": 2, "eslint.config.js": 2,
		"src/small.ts": 10, "src/mid.ts": 500, "src/big.ts": 9000, "src/mid.test.ts": 600, "src/types.d.ts": 400,
		"pyproject.toml": 2, "app/a.py": 100, "app/b.py": 300, "app/c.py": 800, "tests/test_b.py": 350,
		"go.mod": 2, "svc/one.go": 200, "svc/one_test.go": 250,
		"Cargo.toml": 2, "src/lib.rs": 150,
	}
	var tracked []string
	for rel, size := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644)
		tracked = append(tracked, rel)
	}
	cfg := Init(root, tracked, "/opt/ai-guard")

	picked := map[string]string{}
	kinds := map[string][]string{}
	for _, p := range cfg.Patches {
		picked[p.Lang] = p.File
		kinds[p.Lang] = append(kinds[p.Lang], p.Kind)
		if !strings.Contains(p.Append, "{n}") {
			t.Errorf("%s: the patch text has no {n}: every run would repeat the same edit and hit caches", p.Name)
		}
	}
	want := map[string]string{"ts": "src/mid.ts", "py": "app/b.py", "go": "svc/one.go", "rs": "src/lib.rs"}
	for lang, file := range want {
		if picked[lang] != file {
			t.Errorf("%s patches target %q, want the median-sized non-test source %q", lang, picked[lang], file)
		}
		if got := strings.Join(kinds[lang], ","); got != "valid,syntax,type" {
			t.Errorf("%s patch kinds = %s, want valid,syntax,type", lang, got)
		}
	}
	var names []string
	for _, c := range cfg.Baseline {
		names = append(names, c.Name+"["+strings.Join(c.Langs, ",")+"]")
	}
	if got := strings.Join(names, " "); got != "eslint[ts] tsc[ts] flake8[py] black[py] pytest[py] go build[go] cargo build[rs]" {
		t.Errorf("baseline = %s", got)
	}
	if last := cfg.Baseline[len(cfg.Baseline)-1]; last.Dir != "" {
		t.Errorf("cargo build runs in %q, want the repository root", last.Dir)
	}
	if got := strings.Join(cfg.Optimized.Cmd, " "); got != "/opt/ai-guard check --no-tier2 --files {file}" {
		t.Errorf("optimized command = %q", got)
	}
}

func TestInitBuildsTheWholeCargoWorkspace(t *testing.T) {
	root := t.TempDir()
	for rel, text := range map[string]string{
		"Cargo.toml":             "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/core/Cargo.toml": "[package]\nname = \"core\"\n",
		"crates/core/src/lib.rs": "pub fn f() {}\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(text), 0o644)
	}
	cfg := Init(root, []string{"Cargo.toml", "crates/core/Cargo.toml", "crates/core/src/lib.rs"}, "ai-guard")
	if len(cfg.Baseline) != 1 || cfg.Baseline[0].Name != "cargo build" || cfg.Baseline[0].Dir != "" {
		t.Errorf("baseline = %+v, want one cargo build at the workspace root (a member build would skip linking the binaries)", cfg.Baseline)
	}
}

func TestDeferredRunsAreNeitherPassNorFail(t *testing.T) {
	root, _ := project(t)
	cfg := Config{
		Reps: 1, FailFast: true, TimeoutSec: 60,
		Baseline:  []Command{{Name: "legacy", Cmd: self("exit", "0")}},
		Optimized: Command{Name: "ai-guard", Cmd: self("exit", "4"), DeferredExit: 4},
		Patches:   []Patch{{Name: "py-valid", Lang: "py", Kind: "valid", File: "src/mod.py", Append: "\n# {n}\n"}},
	}
	res, err := Run(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Rows[0]; !r.OptDeferred || !r.OptPass {
		t.Fatalf("OptDeferred = %v, OptPass = %v; exit 4 means part of the work went to tier 2, not that the patch was rejected", r.OptDeferred, r.OptPass)
	}
	var md bytes.Buffer
	Markdown(&md, res)
	if !strings.Contains(md.String(), "| pass / defer |") {
		t.Errorf("the table does not show the deferral:\n%s", md.String())
	}
}
