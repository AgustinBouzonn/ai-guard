package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The test binary is also the fake tool and the CLI's own helper child.
func TestMain(m *testing.M) {
	if os.Getenv("AIGUARD_FAKE_TOOL") == "1" && len(os.Args) > 2 && os.Args[1] == "say" {
		code, _ := strconv.Atoi(os.Args[2])
		fmt.Println(strings.Join(os.Args[3:], " "))
		os.Exit(code)
	}
	if os.Getenv("AIGUARD_AS_CLI") == "1" || (len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__")) {
		os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = realMain(args, &out, &errb)
	return code, out.String(), errb.String()
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo is a committed repository whose only check is a fake tier-0 linter
// that fails with the given output when failWith is non-empty.
func repo(t *testing.T, failWith string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	code, text := "0", "clean"
	if failWith != "" {
		code, text = "1", failWith
	}
	cfg := map[string]any{
		"python": map[string]string{"lint": "off", "types": "off", "tests": "off"},
		"steps":  []map[string]any{{"id": "fake.lint", "tier": 0, "langs": []string{"py"}, "cmd": []string{os.Args[0], "say", code, text}, "parser": "generic"}},
	}
	b, _ := json.Marshal(cfg)
	write(t, root, "aiguard.json", string(b))
	write(t, root, "app/x.py", "x = 1\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	return root
}

func TestCheckReportsNothingToDoOnACleanTree(t *testing.T) {
	root := repo(t, "")
	code, out, _ := cli(t, "check", "--root", root)
	if code != 0 || out != "OK no changed source files\n" {
		t.Errorf("exit %d, stdout %q; want exit 0 and the no-changes line", code, out)
	}
}

func TestCheckFailsWithCompactDiagnosticsForChangedFiles(t *testing.T) {
	root := repo(t, "app/x.py:1:5: error: bad thing")
	write(t, root, "app/x.py", "x = 2\n")
	code, out, _ := cli(t, "check", "--root", root)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout:\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "FAIL t0 ") || !strings.Contains(lines[0], " files=1 ") || !strings.HasSuffix(lines[0], " errors=1") || lines[1] != "app/x.py" || lines[2] != "  1:5 bad thing" {
		t.Errorf("stdout:\n%s\nwant a FAIL header, the file, and \"  1:5 bad thing\"", out)
	}
}

func TestCheckJSONFormat(t *testing.T) {
	root := repo(t, "app/x.py:1:5: error: bad thing")
	write(t, root, "app/x.py", "x = 2\n")
	code, out, _ := cli(t, "check", "--root", root, "--format", "json")
	var got struct {
		Status string `json:"status"`
		Files  int    `json:"files"`
		Diags  []struct {
			F string `json:"f"`
			L int    `json:"l"`
			M string `json:"m"`
		} `json:"diags"`
		Steps []struct {
			ID string `json:"id"`
			St string `json:"st"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if code != 1 || got.Status != "fail" || got.Files != 1 || len(got.Diags) != 1 || got.Diags[0].F != "app/x.py" || got.Diags[0].L != 1 || got.Diags[0].M != "bad thing" {
		t.Errorf("exit %d, parsed %+v", code, got)
	}
	if len(got.Steps) != 1 || got.Steps[0].ID != "fake.lint" || got.Steps[0].St != "fail" {
		t.Errorf("steps = %+v, want the fake linter marked fail", got.Steps)
	}
}

func TestCheckExplicitFilesBypassGit(t *testing.T) {
	root := repo(t, "app/x.py:1:5: error: bad thing")
	// the tree is clean; naming the file forces the check
	if code, out, _ := cli(t, "check", "--root", root, "app/x.py"); code != 1 || !strings.Contains(out, "bad thing") {
		t.Errorf("positional file: exit %d, stdout %q", code, out)
	}
	if code, out, _ := cli(t, "check", "--root", root, "--files", filepath.Join(root, "app", "x.py")); code != 1 || !strings.Contains(out, "app/x.py") {
		t.Errorf("--files with an absolute path: exit %d, stdout %q", code, out)
	}
	list := filepath.Join(t.TempDir(), "list.txt")
	os.WriteFile(list, []byte("app/x.py\n\n"), 0o644)
	if code, _, _ := cli(t, "check", "--root", root, "--files-from", list); code != 1 {
		t.Errorf("--files-from: exit %d, want 1", code)
	}
}

func TestCheckStateDirectoryStaysInvisibleToGit(t *testing.T) {
	root := repo(t, "")
	write(t, root, "app/x.py", "x = 2\n")
	if code, out, errOut := cli(t, "check", "--root", root); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".aiguard")); err != nil {
		t.Fatalf("no state directory was created: %v", err)
	}
	if st := git(t, root, "status", "--porcelain", "--untracked-files=all"); strings.Contains(st, ".aiguard") {
		t.Errorf("git sees the state directory:\n%s", st)
	}
}

func TestCheckUsageErrorsExitWith2(t *testing.T) {
	root := repo(t, "")
	if code, _, errOut := cli(t, "check", "--root", root, "--definitely-not-a-flag"); code != 2 || errOut == "" {
		t.Errorf("unknown flag: exit %d, stderr %q; want 2 and a message", code, errOut)
	}
	if code, _, errOut := cli(t, "check", "--root", root, "--format", "xml"); code != 2 || !strings.Contains(errOut, "format") {
		t.Errorf("bad --format: exit %d, stderr %q", code, errOut)
	}
	write(t, root, "aiguard.json", `{"tier1": {"mem_mbb": 1}}`)
	if code, _, errOut := cli(t, "check", "--root", root); code != 2 || !strings.Contains(errOut, "mem_mbb") {
		t.Errorf("broken config: exit %d, stderr %q; want 2 naming the bad key", code, errOut)
	}
}

func TestCheckOutsideARepositoryNeedsFiles(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := cli(t, "check", "--root", dir); code != 2 || !strings.Contains(errOut, "git") {
		t.Errorf("exit %d, stderr %q; want 2 explaining that git is needed to find changes", code, errOut)
	}
}

func TestInitWritesConfigOnceAndHidesStateDir(t *testing.T) {
	root := t.TempDir()
	if code, _, errOut := cli(t, "init", "--root", root); code != 0 {
		t.Fatalf("init: exit %d: %s", code, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(root, "aiguard.json")); err != nil || !json.Valid(b) {
		t.Errorf("aiguard.json missing or invalid: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".aiguard", ".gitignore")); strings.TrimSpace(string(b)) != "*" {
		t.Errorf(".aiguard/.gitignore = %q, want *", b)
	}
	write(t, root, "aiguard.json", `{"max_workers": 3}`)
	if code, _, _ := cli(t, "init", "--root", root); code == 0 {
		t.Error("init overwrote an existing aiguard.json")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "aiguard.json")); string(b) != `{"max_workers": 3}` {
		t.Errorf("existing config was modified: %s", b)
	}
}

func TestDoctorDescribesTheMachine(t *testing.T) {
	code, out, _ := cli(t, "doctor", "--root", t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"cpu:", "ram:", "limits:"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}

func TestSelftestPassesOnThisMachine(t *testing.T) {
	code, out, _ := cli(t, "selftest")
	if code != 0 || strings.Contains(out, "FAIL") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "PASS  memory watchdog") || !strings.Contains(out, "PASS  timeout kills the whole tree") {
		t.Errorf("selftest did not report its core checks:\n%s", out)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	if code, _, errOut := cli(t, "frobnicate"); code != 2 || !strings.Contains(errOut, "frobnicate") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestGoProjectEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "go.mod", "module e2e\n\ngo 1.21\n")
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	write(t, root, "calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Errorf(\"Add(1, 2) = %d, want 3\", Add(1, 2))\n\t}\n}\n")
	write(t, root, "app/app.go", "package app\n\nimport \"e2e/calc\"\n\nfunc Double(x int) int { return calc.Add(x, x) }\n")
	write(t, root, "aiguard.json", `{"tier1": {"mem_mb": 2048}}`)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")

	// 1. syntax error: tier 0 stops the run
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + }\n")
	code, out, _ := cli(t, "check", "--root", root)
	if code != 1 || !strings.HasPrefix(out, "FAIL t0 ") || strings.Contains(out, " t1 ") || !strings.Contains(out, "calc/calc.go\n  3:") {
		t.Errorf("syntax error: exit %d\n%s", code, out)
	}

	// 2. signature change: calc itself is fine, its importer breaks
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b, c int) int { return a + b + c }\n")
	code, out, _ = cli(t, "check", "--root", root)
	if code != 1 || !strings.Contains(out, " t1 ") || !strings.Contains(out, "app/app.go\n  5:") {
		t.Errorf("importer breakage must be found through reverse dependencies: exit %d\n%s", code, out)
	}

	// 3. behaviour change: type-checks, the unit test catches it
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a - b }\n")
	code, out, _ = cli(t, "check", "--root", root)
	if code != 1 || !strings.Contains(out, "calc/calc_test.go\n  7 TestAdd Add(1, 2) = -1, want 3") {
		t.Errorf("failing test: exit %d\n%s", code, out)
	}

	// 4. correct edit
	write(t, root, "calc/calc.go", "package calc\n\n// Add sums.\nfunc Add(a, b int) int { return a + b }\n")
	code, out, _ = cli(t, "check", "--root", root)
	if code != 0 || !strings.HasPrefix(out, "OK t0 ") || !strings.Contains(out, " t1 ") {
		t.Errorf("valid edit: exit %d\n%s", code, out)
	}
}

func TestTier2EndToEndThroughADetachedWorker(t *testing.T) {
	root := repo(t, "")
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	cfg := map[string]any{
		"python": map[string]string{"lint": "off", "types": "off", "tests": "off"},
		"steps":  []map[string]any{{"id": "fake.lint", "tier": 0, "langs": []string{"py"}, "cmd": []string{os.Args[0], "say", "0", "clean"}, "parser": "generic"}},
		"tier2":  map[string]any{"trigger": "on_pass", "command": []string{os.Args[0], "say", "0", "remote build ok for {sha}"}},
	}
	b, _ := json.Marshal(cfg)
	write(t, root, "aiguard.json", string(b))
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "tier2 config")
	write(t, root, "app/x.py", "x = 3\n")

	// A separate CLI process: the worker has to outlive it.
	cmd := exec.Command(os.Args[0], "check", "--root", root)
	cmd.Env = append(os.Environ(), "AIGUARD_AS_CLI=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}
	var id string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "> tier2 queued id=") {
			id = strings.Fields(strings.TrimPrefix(line, "> tier2 queued id="))[0]
		}
	}
	if id == "" {
		t.Fatalf("no tier-2 job was queued:\n%s", out)
	}

	code, wout, werr := cli(t, "tier2", "wait", "--root", root, "--timeout", "60", id)
	if code != 0 || !strings.Contains(wout, id+" done") || !strings.Contains(wout, "remote build ok for ") {
		t.Fatalf("wait: exit %d\nstdout:\n%s\nstderr:\n%s", code, wout, werr)
	}
	sha := git(t, bare, "rev-parse", "refs/aiguard/"+id)
	if got := git(t, bare, "show", strings.TrimSpace(sha)+":app/x.py"); got != "x = 3\n" {
		t.Errorf("the remote snapshot has app/x.py = %q, want the uncommitted edit", got)
	}
	if code, lout, _ := cli(t, "tier2", "list", "--root", root); code != 0 || !strings.Contains(lout, id+" done") {
		t.Errorf("list: exit %d\n%s", code, lout)
	}
	if st := git(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "M app/x.py" {
		t.Errorf("tier 2 disturbed the work tree:\n%s", st)
	}
}

func TestTier2WaitReportsARemoteFailure(t *testing.T) {
	root := repo(t, "")
	b, _ := json.Marshal(map[string]any{"tier2": map[string]any{"trigger": "manual", "sync": "none", "command": []string{os.Args[0], "say", "7", "E2E suite: 2 failed"}}})
	write(t, root, "aiguard.json", string(b))
	cmd := exec.Command(os.Args[0], "tier2", "run", "--root", root)
	cmd.Env = append(os.Environ(), "AIGUARD_AS_CLI=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tier2 run: %v\n%s", err, out)
	}
	code, out, _ := cli(t, "tier2", "wait", "--root", root, "--timeout", "60")
	if code != 1 || !strings.Contains(out, " failed exit=7") || !strings.Contains(out, "E2E suite: 2 failed") {
		t.Errorf("exit %d, want 1 with the remote exit code and its log tail:\n%s", code, out)
	}
}

func TestTier2WaitNoticesADeadWorker(t *testing.T) {
	root := repo(t, "")
	dir := filepath.Join(root, ".aiguard", "tier2")
	os.MkdirAll(dir, 0o755)
	gone := exec.Command(os.Args[0], "say", "0", "x")
	gone.Run() // a pid that has certainly exited
	job := `{"id":"20200101-000000000000-aaaaaa","state":"running","reason":"r","created":"2020-01-01T00:00:00Z","log":"l"}`
	os.WriteFile(filepath.Join(dir, "20200101-000000000000-aaaaaa.json"), []byte(job), 0o644)
	os.WriteFile(filepath.Join(dir, "20200101-000000000000-aaaaaa.pid"), []byte(strconv.Itoa(gone.Process.Pid)), 0o644)
	code, out, _ := cli(t, "tier2", "wait", "--root", root, "--timeout", "30")
	if code != 1 || !strings.Contains(out, "failed") || !strings.Contains(out, "worker") {
		t.Errorf("exit %d, want 1 explaining that the worker is gone:\n%s", code, out)
	}
}

func TestBenchInitRunRestore(t *testing.T) {
	root := repo(t, "")
	t.Setenv("AIGUARD_AS_CLI", "1") // children of this test binary behave as the CLI
	if code, _, errOut := cli(t, "bench", "init", "--root", root); code != 0 {
		t.Fatalf("bench init: exit %d: %s", code, errOut)
	}
	path := filepath.Join(root, "aiguard.bench.json")
	var cfg map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("aiguard.bench.json: %v", err)
	}
	patches := cfg["patches"].([]any)
	if len(patches) != 3 || patches[0].(map[string]any)["file"] != "app/x.py" {
		t.Fatalf("patches = %v, want three probes on app/x.py", patches)
	}
	if code, _, _ := cli(t, "bench", "init", "--root", root); code == 0 {
		t.Error("bench init overwrote an existing benchmark definition")
	}
	// hermetic pipelines: a fake legacy command, and this binary as ai-guard
	cfg["baseline"] = []map[string]any{{"name": "legacy", "cmd": []string{os.Args[0], "say", "0", "ok"}}}
	cfg["optimized"] = map[string]any{"name": "ai-guard", "cmd": []string{os.Args[0], "check", "--root", root, "--no-tier2", "--files", "{file}"}}
	data, _ = json.Marshal(cfg)
	os.WriteFile(path, data, 0o644)

	code, out, errOut := cli(t, "bench", "run", "--root", root, "--reps", "1", "--warmup", "0")
	if code != 0 {
		t.Fatalf("bench run: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "| py-valid | app/x.py |") || !strings.Contains(out, "| **all** (3 patches) |") {
		t.Errorf("table lacks the patch rows or the summary:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "app", "x.py")); string(b) != "x = 1\n" {
		t.Errorf("the benchmark left app/x.py as %q", b)
	}
	if report, err := os.ReadFile(filepath.Join(root, ".aiguard", "bench", "report.md")); err != nil || !strings.Contains(string(report), "| py-syntax |") {
		t.Errorf("report.md missing or incomplete: %v", err)
	}
	var raw struct {
		Rows []struct {
			Runs []struct {
				WallMs int64 `json:"wall_ms"`
			} `json:"aiguard_runs"`
		} `json:"rows"`
	}
	results, _ := os.ReadFile(filepath.Join(root, ".aiguard", "bench", "results.json"))
	if err := json.Unmarshal(results, &raw); err != nil || len(raw.Rows) != 3 || len(raw.Rows[0].Runs) != 1 {
		t.Errorf("results.json: %v, rows %+v", err, raw.Rows)
	}
	if st := strings.TrimSpace(git(t, root, "status", "--porcelain")); st != "?? aiguard.bench.json" {
		t.Errorf("git status after the benchmark:\n%s\nwant only the new benchmark definition", st)
	}
	if code, out, _ := cli(t, "bench", "restore", "--root", root); code != 0 || !strings.Contains(out, "nothing to restore") {
		t.Errorf("bench restore: exit %d, %q", code, out)
	}
	if code, out, _ := cli(t, "bench", "run", "--root", root, "--reps", "1", "--warmup", "0", "--only", "go"); code != 2 || out != "" {
		t.Errorf("--only with no matching patch: exit %d, stdout %q; want a usage error", code, out)
	}
}

func TestRunPassesOutputAndExitCodeThroughAndReportsResources(t *testing.T) {
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	code, out, errOut := cli(t, "run", "--", os.Args[0], "say", "3", "hello from the tool")
	if code != 3 {
		t.Errorf("exit %d, want the command's own 3", code)
	}
	if strings.TrimSpace(out) != "hello from the tool" {
		t.Errorf("stdout = %q, want the command's output untouched", out)
	}
	if !strings.Contains(errOut, "[ai-guard] exit=3 wall=") || !strings.Contains(errOut, " peak=") || !strings.Contains(errOut, " cpu=") {
		t.Errorf("stderr = %q, want a resource summary line", errOut)
	}
}

func TestRunKillsACommandOverItsMemoryBudget(t *testing.T) {
	code, _, errOut := cli(t, "run", "--mem", "64", "--", os.Args[0], "__hog", "300", "8000")
	if code != 137 || !strings.Contains(errOut, "killed=mem") {
		t.Errorf("exit %d, stderr %q; want 137 and killed=mem", code, errOut)
	}
}

func TestRunStopsACommandAtItsTimeout(t *testing.T) {
	code, _, errOut := cli(t, "run", "--timeout", "1", "--", os.Args[0], "__sleep", "60000")
	if code != 124 || !strings.Contains(errOut, "killed=timeout") {
		t.Errorf("exit %d, stderr %q; want 124 and killed=timeout", code, errOut)
	}
}

func TestRunAcceptsAShellLine(t *testing.T) {
	code, out, _ := cli(t, "run", "--shell", "echo one && echo two")
	if got := strings.Fields(out); code != 0 || len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

func TestRunWithoutACommandIsAUsageError(t *testing.T) {
	if code, _, errOut := cli(t, "run"); code != 2 || !strings.Contains(errOut, "run") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestCheckDisableAndEnableChooseStepsForOneRun(t *testing.T) {
	root := repo(t, "app/x.py:1:5: error: bad thing")
	write(t, root, "app/x.py", "x = 2\n")
	if code, out, _ := cli(t, "check", "--root", root, "--disable", "fake.lint"); code != 0 {
		t.Errorf("--disable fake.lint: exit %d, want 0 (the failing step must not run)\n%s", code, out)
	}

	// An opt-in step: the project's ESLint, here a stand-in that reports one error.
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	t.Setenv("AIGUARD_TOOLS", t.TempDir())
	write(t, root, "node_modules/eslint/bin/eslint.js", `const f = require("path").resolve(process.argv[process.argv.length - 1]);
console.log(JSON.stringify([{filePath: f, messages: [{ruleId: "no-thing", severity: 2, message: "do not", line: 3, column: 7}]}]));
process.exit(1);
`)
	write(t, root, "web/a.js", "export const a = 1;\n")
	if code, out, _ := cli(t, "check", "--root", root, "--files", "web/a.js"); code != 0 {
		t.Fatalf("without --enable: exit %d, want 0\n%s", code, out)
	}
	code, out, _ := cli(t, "check", "--root", root, "--files", "web/a.js", "--enable", "ts.eslint")
	if code != 1 || !strings.Contains(out, "web/a.js") || !strings.Contains(out, "3:7 no-thing do not") {
		t.Errorf("--enable ts.eslint: exit %d, want 1 with the ESLint finding\n%s", code, out)
	}
}

func TestToolsDirectoryResolution(t *testing.T) {
	exeDir := t.TempDir()
	if got := resolveToolsDir("/explicit", exeDir, "/cache"); got != "/explicit" {
		t.Errorf("with AIGUARD_TOOLS set: %q, want /explicit", got)
	}
	if got, want := resolveToolsDir("", exeDir, "/cache"), filepath.Join("/cache", "ai-guard", "tools"); got != want {
		t.Errorf("default: %q, want %q", got, want)
	}
	os.Mkdir(filepath.Join(exeDir, "tools"), 0o755) // a portable install: tools travel with the binary
	if got, want := resolveToolsDir("", exeDir, "/cache"), filepath.Join(exeDir, "tools"); got != want {
		t.Errorf("portable: %q, want %q", got, want)
	}
}

// fakeNpm puts an "npm" on PATH that records how it was called and installs
// a stand-in for the native TypeScript compiler package.
func fakeNpm(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "npm.log")
	t.Setenv("AIGUARD_TEST_LOG", log)
	arch := map[string]string{"amd64": "x64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	if runtime.GOOS == "windows" {
		pkg := `node_modules\@typescript\native-preview-win32-` + arch
		write(t, bin, "npm.cmd", "@echo off\r\necho %CD%^|%*> \"%AIGUARD_TEST_LOG%\"\r\nmkdir "+pkg+`\lib`+"\r\nmkdir node_modules\\@typescript\\native-preview\r\necho bin> "+pkg+`\lib\tsgo.exe`+
			"\r\necho {\"version\":\"7.0.0-test\"}> node_modules\\@typescript\\native-preview\\package.json\r\n")
	} else {
		pkg := "node_modules/@typescript/native-preview-" + runtime.GOOS + "-" + arch
		write(t, bin, "npm", "#!/bin/sh\necho \"$PWD|$*\" > \"$AIGUARD_TEST_LOG\"\nmkdir -p "+pkg+"/lib node_modules/@typescript/native-preview\necho bin > "+pkg+"/lib/tsgo\n"+
			"echo '{\"version\":\"7.0.0-test\"}' > node_modules/@typescript/native-preview/package.json\n")
		os.Chmod(filepath.Join(bin, "npm"), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestToolsInstallPutsTheNativeCompilerInItsOwnNpmPrefix(t *testing.T) {
	log := fakeNpm(t)
	dir := filepath.Join(t.TempDir(), "tools") // does not exist yet
	t.Setenv("AIGUARD_TOOLS", dir)

	code, out, _ := cli(t, "tools")
	if code != 0 || !strings.Contains(out, dir) || !strings.Contains(out, "not installed") {
		t.Errorf("tools before install: exit %d\n%s\nwant the directory and a compiler reported as not installed", code, out)
	}

	code, out, errOut := cli(t, "tools", "install")
	if code != 0 {
		t.Fatalf("tools install: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	call, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("npm was not run: %v", err)
	}
	cwd, args, _ := strings.Cut(strings.TrimSpace(string(call)), "|")
	if real, _ := filepath.EvalSymlinks(dir); !strings.EqualFold(cwd, dir) && !strings.EqualFold(cwd, real) {
		t.Errorf("npm ran in %q, want the tools directory %q", cwd, dir)
	}
	if !strings.HasPrefix(args, "install ") || !strings.Contains(args, "@typescript/native-preview@latest") {
		t.Errorf("npm args = %q, want an install of @typescript/native-preview@latest", args)
	}
	// Without its own manifest npm would install into whatever project encloses the directory.
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		t.Errorf("no package.json in the tools directory: %v", err)
	}
	if !strings.Contains(out, "7.0.0-test") || !strings.Contains(out, "tsgo") {
		t.Errorf("stdout:\n%s\nwant the installed compiler and its version", out)
	}

	t.Setenv("PATH", t.TempDir()) // no npm at all
	if code, _, errOut := cli(t, "tools", "install"); code != 3 || !strings.Contains(errOut, "npm") {
		t.Errorf("without npm: exit %d, stderr %q; want 3 and a message about npm", code, errOut)
	}
	if code, _, _ := cli(t, "tools", "upgrade"); code != 2 {
		t.Errorf("unknown subcommand: exit %d, want 2", code)
	}
}

func TestTier2WorkerFollowsTheConfigTheCheckWasGiven(t *testing.T) {
	root := repo(t, "")
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	// The config lives outside the repository and says: do not sync, just run this.
	cfg := map[string]any{
		"python": map[string]string{"lint": "off", "types": "off", "tests": "off"},
		"tier2":  map[string]any{"trigger": "manual", "sync": "none", "command": []string{os.Args[0], "say", "0", "local suite ok"}},
	}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(t.TempDir(), "elsewhere.json")
	os.WriteFile(cfgPath, b, 0o644)
	write(t, root, "app/x.py", "x = 3\n")

	cmd := exec.Command(os.Args[0], "check", "--root", root, "--config", cfgPath, "--tier2")
	cmd.Env = append(os.Environ(), "AIGUARD_AS_CLI=1")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "> tier2 queued id=") {
		t.Fatalf("check: %v\n%s", err, out)
	}
	code, out, _ := cli(t, "tier2", "wait", "--root", root, "--timeout", "60")
	if code != 0 || !strings.Contains(out, "local suite ok") {
		t.Errorf("wait: exit %d, want the configured command to have run:\n%s", code, out)
	}
	// The worker must not fall back to the defaults (push a snapshot to origin).
	if refs := git(t, bare, "for-each-ref", "refs/aiguard"); strings.TrimSpace(refs) != "" {
		t.Errorf("sync is \"none\", yet the worker pushed to origin:\n%s", refs)
	}

	// The same holds for a job queued by hand.
	cmd = exec.Command(os.Args[0], "tier2", "run", "--root", root, "--config", cfgPath)
	cmd.Env = append(os.Environ(), "AIGUARD_AS_CLI=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tier2 run --config: %v\n%s", err, out)
	}
	if code, out, _ := cli(t, "tier2", "wait", "--root", root, "--timeout", "60"); code != 0 || !strings.Contains(out, "local suite ok") {
		t.Errorf("wait after tier2 run --config: exit %d\n%s", code, out)
	}
	if refs := git(t, bare, "for-each-ref", "refs/aiguard"); strings.TrimSpace(refs) != "" {
		t.Errorf("tier2 run --config pushed to origin:\n%s", refs)
	}
}

func TestTier2IsNeverStartedOnDefaultsAlone(t *testing.T) {
	// --tier2 in a repository that never configured a remote tier must not
	// push a snapshot of the work tree to origin just because that is the
	// default sync mode.
	root := repo(t, "")
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, root, "init", "-q", "--bare", bare)
	git(t, root, "remote", "add", "origin", bare)
	write(t, root, "app/x.py", "x = 3\n")
	code, out, _ := cli(t, "check", "--root", root, "--tier2")
	if code != 0 || !strings.Contains(out, "not configured") || strings.Contains(out, "tier2 queued") {
		t.Errorf("exit %d, want the local pass plus a note that tier 2 is not configured:\n%s", code, out)
	}
	if code, _, errOut := cli(t, "tier2", "run", "--root", root); code != 2 || !strings.Contains(errOut, "not configured") {
		t.Errorf("tier2 run: exit %d, stderr %q; want 2 and the same explanation", code, errOut)
	}
	if code, out, _ := cli(t, "tier2", "list", "--root", root); code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("jobs exist although none should have been queued:\n%s", out)
	}
	if refs := git(t, bare, "for-each-ref", "refs/aiguard"); strings.TrimSpace(refs) != "" {
		t.Errorf("a snapshot was pushed to origin:\n%s", refs)
	}
}
