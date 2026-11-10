package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
)

// tree creates files under a fresh root. Keys are slash paths; values are contents.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// env simulates a machine: target OS plus the tools reachable through PATH.
func env(root, goos string, onPath ...string) Env {
	return Env{
		GOOS: goos, GOARCH: "amd64", CacheDir: filepath.Join(root, ".aiguard", "cache"),
		LookPath: func(name string) (string, error) {
			for _, p := range onPath {
				if p == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
	}
}

func cfg(t *testing.T, mutate ...func(*config.Config)) *config.Config {
	t.Helper()
	c := config.Default()
	for _, m := range mutate {
		m(c)
	}
	return c
}

func step(t *testing.T, p Plan, id string) *Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	var ids []string
	for _, s := range p.Steps {
		ids = append(ids, s.ID)
	}
	t.Fatalf("no step %q in plan; steps = %v; notes = %v", id, ids, p.Notes)
	return nil
}

func noStep(t *testing.T, p Plan, prefix string) {
	t.Helper()
	for _, s := range p.Steps {
		if strings.HasPrefix(s.ID, prefix) {
			t.Errorf("unexpected step %q: %v", s.ID, s.Argv)
		}
	}
}

func hasNote(p Plan, sub string) bool {
	for _, n := range p.Notes {
		if strings.Contains(n.Text, sub) {
			return true
		}
	}
	return false
}

func abs(root string, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

func wantArgv(t *testing.T, s *Step, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(s.Argv, want) {
		t.Errorf("%s argv =\n  %q\nwant\n  %q", s.ID, s.Argv, want)
	}
}

func TestTSLintUsesBiomeNativeBinaryNotTheNodeShim(t *testing.T) {
	root := tree(t, map[string]string{
		"biome.json": "{}", "package.json": "{}", "src/a.ts": "", "src/b.tsx": "",
		"node_modules/.bin/biome":                   "shim",
		"node_modules/@biomejs/cli-linux-x64/biome": "bin",
	})
	p := Build(root, []string{"src/a.ts", "src/b.tsx"}, cfg(t), env(root, "linux"))
	s := step(t, p, "ts.lint")
	wantArgv(t, s, abs(root, "node_modules/@biomejs/cli-linux-x64/biome"), "lint", "--reporter=github", "--colors=off",
		"--no-errors-on-unmatched", "--files-ignore-unknown=true", "--max-diagnostics=200", "--diagnostic-level=error", "src/a.ts", "src/b.tsx")
	if s.Tier != 0 || s.Dir != root {
		t.Errorf("tier = %d, dir = %q; want tier 0 run from the repo root", s.Tier, s.Dir)
	}
}

func TestTSLintFindsBiomeInPnpmStoreAndOnWindows(t *testing.T) {
	root := tree(t, map[string]string{
		"biome.json": "{}", "src/a.ts": "",
		"node_modules/.pnpm/@biomejs+cli-win32-x64@2.6.6/node_modules/@biomejs/cli-win32-x64/biome.exe": "bin",
	})
	p := Build(root, []string{"src/a.ts"}, cfg(t), env(root, "windows"))
	s := step(t, p, "ts.lint")
	if want := abs(root, "node_modules/.pnpm/@biomejs+cli-win32-x64@2.6.6/node_modules/@biomejs/cli-win32-x64/biome.exe"); s.Argv[0] != want {
		t.Errorf("biome = %q, want the pnpm-store binary %q", s.Argv[0], want)
	}
}

func TestTSLintRunsPerNestedConfigWithPathsRelativeToIt(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json": "{}", "apps/web/biome.json": "{}", "apps/web/src/a.ts": "", "libs/core/.oxlintrc.json": "{}", "libs/core/src/b.ts": "",
		"node_modules/@biomejs/cli-linux-x64/biome": "bin", "node_modules/oxlint/bin/oxlint": "js",
	})
	p := Build(root, []string{"apps/web/src/a.ts", "libs/core/src/b.ts"}, cfg(t), env(root, "linux", "node"))
	web := step(t, p, "ts.lint@apps/web")
	if web.Dir != abs(root, "apps/web") || web.Argv[len(web.Argv)-1] != "src/a.ts" {
		t.Errorf("web: dir = %q, last arg = %q; want apps/web and src/a.ts", web.Dir, web.Argv[len(web.Argv)-1])
	}
	core := step(t, p, "ts.lint@libs/core")
	wantArgv(t, core, "/usr/bin/node", abs(root, "node_modules/oxlint/bin/oxlint"), "-f", "unix", "--quiet", "src/b.ts")
}

func TestTSLintMissingToolBecomesANoteNotAStep(t *testing.T) {
	root := tree(t, map[string]string{"package.json": "{}", "src/a.ts": ""})
	p := Build(root, []string{"src/a.ts"}, cfg(t), env(root, "linux"))
	noStep(t, p, "ts.lint")
	if !hasNote(p, "ts.lint skipped") {
		t.Errorf("notes = %v, want a ts.lint skipped note", p.Notes)
	}
}

func TestTSTypesPrefersNativeCompilerAndFastCheckConfig(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json": "{}", "tsconfig.json": "{}", "tsconfig.fast-check.json": "{}", "src/a.ts": "",
		"node_modules/typescript/bin/tsc":                       "js",
		"node_modules/@typescript/typescript-linux-x64/lib/tsc": "bin",
	})
	p := Build(root, []string{"src/a.ts"}, cfg(t), env(root, "linux", "node"))
	s := step(t, p, "ts.types")
	wantArgv(t, s, abs(root, "node_modules/@typescript/typescript-linux-x64/lib/tsc"), "-p", "tsconfig.fast-check.json", "--noEmit", "--pretty", "false")
	if s.Tier != 1 || s.Phase != 0 {
		t.Errorf("tier/phase = %d/%d, want 1/0", s.Tier, s.Phase)
	}
	if got := s.MemEnv(512); !reflect.DeepEqual(got, []string{"GOMEMLIMIT=460MiB"}) {
		t.Errorf("MemEnv(512) = %v, want the Go runtime soft limit for the native compiler", got)
	}
}

func TestTSTypesAddsIncrementalFlagsToAPlainTsconfigAndFallsBackToNode(t *testing.T) {
	root := tree(t, map[string]string{
		"apps/web/package.json": "{}", "apps/web/tsconfig.json": "{}", "apps/web/src/a.ts": "", "libs/core/tsconfig.json": "{}", "libs/core/b.ts": "",
		"node_modules/typescript/bin/tsc": "js",
	})
	e := env(root, "linux", "node")
	p := Build(root, []string{"apps/web/src/a.ts", "libs/core/b.ts"}, cfg(t), e)
	web := step(t, p, "ts.types@apps/web")
	wantArgv(t, web, "/usr/bin/node", abs(root, "node_modules/typescript/bin/tsc"), "-p", "tsconfig.json", "--noEmit", "--pretty", "false",
		"--incremental", "--tsBuildInfoFile", filepath.Join(e.CacheDir, "tsc-apps_web_tsconfig.json.tsbuildinfo"), "--skipLibCheck")
	if web.Dir != abs(root, "apps/web") {
		t.Errorf("dir = %q, want the tsconfig directory", web.Dir)
	}
	if got := web.MemEnv(512); !reflect.DeepEqual(got, []string{"NODE_OPTIONS=--max-old-space-size=384"}) {
		t.Errorf("MemEnv(512) = %v, want a V8 heap cap at 75%% of the budget", got)
	}
	step(t, p, "ts.types@libs/core") // one run per project
}

func TestTSTestsUseRelatedFileSelection(t *testing.T) {
	root := tree(t, map[string]string{"package.json": "{}", "src/a.ts": "", "node_modules/vitest/vitest.mjs": "js"})
	p := Build(root, []string{"src/a.ts"}, cfg(t), env(root, "linux", "node"))
	s := step(t, p, "ts.test")
	wantArgv(t, s, "/usr/bin/node", abs(root, "node_modules/vitest/vitest.mjs"), "related", "--run", "--reporter=github-actions", "--passWithNoTests", "--maxWorkers=2", "src/a.ts")
	if s.Phase != 1 {
		t.Errorf("phase = %d, want tests in phase 1", s.Phase)
	}

	root = tree(t, map[string]string{"package.json": "{}", "src/a.js": "", "node_modules/jest/bin/jest.js": "js"})
	p = Build(root, []string{"src/a.js"}, cfg(t), env(root, "linux", "node"))
	wantArgv(t, step(t, p, "ts.test"), "/usr/bin/node", abs(root, "node_modules/jest/bin/jest.js"), "--json", "--testLocationInResults", "--silent", "--passWithNoTests", "--maxWorkers=2", "--findRelatedTests", "src/a.js")
}

func TestPythonToolsComeFromTheProjectVenv(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml": "[tool.mypy]\nstrict = true\n", "app/x.py": "", "tests/test_x.py": "",
		".venv/Scripts/ruff.exe": "", ".venv/Scripts/dmypy.exe": "", ".venv/Scripts/pytest.exe": "", ".venv/Lib/site-packages/testmon/__init__.py": "",
	})
	e := env(root, "windows")
	p := Build(root, []string{"app/x.py"}, cfg(t), e)
	wantArgv(t, step(t, p, "py.lint"), abs(root, ".venv/Scripts/ruff.exe"), "check", "--output-format=concise", "--no-fix", "--force-exclude", "--quiet", "app/x.py")

	types := step(t, p, "py.types")
	wantArgv(t, types, abs(root, ".venv/Scripts/dmypy.exe"), "--status-file", filepath.Join(e.CacheDir, "dmypy-root.json"), "run", "--timeout", "900", "--",
		"--show-column-numbers", "--no-error-summary", "--hide-error-context", "--no-pretty", "--no-color-output", "--cache-dir", filepath.Join(e.CacheDir, "mypy-root"), ".")
	if !types.Detached || types.Daemon == nil || types.Daemon.StatusFile != filepath.Join(e.CacheDir, "dmypy-root.json") {
		t.Errorf("dmypy step must be detached and carry its status file; got detached=%v daemon=%+v", types.Detached, types.Daemon)
	}

	wantArgv(t, step(t, p, "py.test"), abs(root, ".venv/Scripts/pytest.exe"), "-q", "--no-header", "--color=no", "--tb=line", "-rfE", "--testmon")
}

func TestPythonTypeCheckerSelection(t *testing.T) {
	bins := map[string]string{".venv/bin/ruff": "", ".venv/bin/dmypy": "", ".venv/bin/mypy": "", ".venv/bin/ty": "", ".venv/bin/pyright": "", "app/x.py": ""}
	with := func(extra map[string]string) string {
		m := map[string]string{}
		for k, v := range bins {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return tree(t, m)
	}
	// [tool.ty] in pyproject selects ty and checks the whole project
	root := with(map[string]string{"pyproject.toml": "[tool.ty]\n"})
	wantArgv(t, step(t, Build(root, []string{"app/x.py"}, cfg(t), env(root, "linux")), "py.types"),
		abs(root, ".venv/bin/ty"), "check", "--output-format", "concise", "--color", "never", "--no-progress")

	// pyrightconfig.json selects pyright on the changed files
	root = with(map[string]string{"pyrightconfig.json": "{}"})
	wantArgv(t, step(t, Build(root, []string{"app/x.py"}, cfg(t), env(root, "linux")), "py.types"),
		abs(root, ".venv/bin/pyright"), "--outputjson", "app/x.py")

	// mypy with `files =` in its config gets no explicit targets
	root = with(map[string]string{"mypy.ini": "[mypy]\nfiles = app\n"})
	argv := step(t, Build(root, []string{"app/x.py"}, cfg(t), env(root, "linux")), "py.types").Argv
	if last := argv[len(argv)-1]; last == "." || !strings.HasPrefix(argv[len(argv)-2], "--cache-dir") {
		t.Errorf("dmypy argv ends with %q; a config that lists its own files must not get an extra target", last)
	}

	// an explicit choice beats detection
	root = with(map[string]string{"pyproject.toml": "[tool.ty]\n"})
	c := cfg(t, func(c *config.Config) { c.Python.Types = "mypy" })
	if got := step(t, Build(root, []string{"app/x.py"}, c, env(root, "linux")), "py.types").Argv[0]; got != abs(root, ".venv/bin/mypy") {
		t.Errorf("python.types=mypy ran %q", got)
	}
}

func TestPythonTestsWithoutTestmonRunOnlyMatchingTestFiles(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml": "", "app/calc.py": "", "app/text.py": "", "tests/test_calc.py": "", "tests/test_other.py": "", ".venv/bin/pytest": "", ".venv/bin/ruff": "",
	})
	p := Build(root, []string{"app/calc.py", "tests/test_other.py"}, cfg(t), env(root, "linux"))
	wantArgv(t, step(t, p, "py.test"), abs(root, ".venv/bin/pytest"), "-q", "--no-header", "--color=no", "--tb=line", "-rfE", "tests/test_calc.py", "tests/test_other.py")

	p = Build(root, []string{"app/text.py"}, cfg(t), env(root, "linux"))
	noStep(t, p, "py.test")
	if !hasNote(p, "pytest-testmon") {
		t.Errorf("notes = %v, want a hint to install pytest-testmon", p.Notes)
	}
}

func TestGoStepsTargetChangedPackagesOfEachModule(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module lab\n", "pkg/a/a.go": "", "pkg/a/a_test.go": "", "cmd/app/main.go": "", "svc/go.mod": "module svc\n", "svc/x.go": ""})
	p := Build(root, []string{"cmd/app/main.go", "pkg/a/a.go", "pkg/a/a_test.go", "svc/x.go"}, cfg(t), env(root, "linux", "go", "gofmt"))
	wantArgv(t, step(t, p, "go.syntax"), "/usr/bin/gofmt", "-l", "-e", "cmd/app/main.go", "pkg/a/a.go", "pkg/a/a_test.go", "svc/x.go")
	wantArgv(t, step(t, p, "go.vet"), "/usr/bin/go", "vet", "./cmd/app", "./pkg/a")
	wantArgv(t, step(t, p, "go.test"), "/usr/bin/go", "test", "-json", "-vet=off", "./cmd/app", "./pkg/a")
	svc := step(t, p, "go.vet@svc")
	wantArgv(t, svc, "/usr/bin/go", "vet", ".")
	if svc.Dir != abs(root, "svc") {
		t.Errorf("svc vet dir = %q, want the module directory", svc.Dir)
	}
}

func TestRustStepsUseEditionWorkspaceRootAndTargets(t *testing.T) {
	root := tree(t, map[string]string{
		"Cargo.toml":             "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/core/Cargo.toml": "[package]\nname = \"core-lib\"\nedition = \"2024\"\n",
		"crates/core/src/lib.rs": "", "crates/core/src/util.rs": "",
		"crates/cli/Cargo.toml":  "[package]\nname = \"cli\"\nedition = \"2021\"\n",
		"crates/cli/src/main.rs": "",
	})
	p := Build(root, []string{"crates/cli/src/main.rs", "crates/core/src/util.rs"}, cfg(t), env(root, "linux", "cargo", "rustfmt"))
	wantArgv(t, step(t, p, "rs.syntax#1"), "/usr/bin/rustfmt", "--check", "--edition", "2021", "--color", "never", "crates/cli/src/main.rs")
	wantArgv(t, step(t, p, "rs.syntax#2"), "/usr/bin/rustfmt", "--check", "--edition", "2024", "--color", "never", "crates/core/src/util.rs")
	check := step(t, p, "rs.check")
	wantArgv(t, check, "/usr/bin/cargo", "check", "--message-format=short", "--color", "never", "-q", "--workspace")
	if check.Dir != root {
		t.Errorf("cargo check dir = %q, want the workspace root", check.Dir)
	}
	wantArgv(t, step(t, p, "rs.test@crates/cli"), "/usr/bin/cargo", "test", "--message-format=short", "--color", "never", "-q", "-p", "cli", "--bins")
	wantArgv(t, step(t, p, "rs.test@crates/core"), "/usr/bin/cargo", "test", "--message-format=short", "--color", "never", "-q", "-p", "core-lib", "--bins", "--lib")
}

func TestCppSyntaxReusesCompileCommandsWithoutCodegen(t *testing.T) {
	root := tree(t, map[string]string{"src/a.cpp": "", "src/b.c": "", "include/a.hpp": ""})
	ccdb := `[
 {"directory": "` + filepath.ToSlash(filepath.Join(root, "build")) + `", "file": "` + filepath.ToSlash(filepath.Join(root, "src", "a.cpp")) + `",
  "command": "/usr/bin/c++ -DNDEBUG -I` + filepath.ToSlash(filepath.Join(root, "include")) + ` -std=gnu++20 -MD -MT CMakeFiles/x.dir/src/a.cpp.o -MF CMakeFiles/x.dir/src/a.cpp.o.d -o CMakeFiles/x.dir/src/a.cpp.o -c ` + filepath.ToSlash(filepath.Join(root, "src", "a.cpp")) + `"}
]`
	os.MkdirAll(filepath.Join(root, "build"), 0o755)
	os.WriteFile(filepath.Join(root, "build", "compile_commands.json"), []byte(ccdb), 0o644)

	p := Build(root, []string{"include/a.hpp", "src/a.cpp", "src/b.c"}, cfg(t), env(root, "linux", "clang", "clang++"))
	a := step(t, p, "cpp.syntax#1")
	wantArgv(t, a, "/usr/bin/c++", "-DNDEBUG", "-I"+filepath.ToSlash(filepath.Join(root, "include")), "-std=gnu++20", "-fsyntax-only", "-fdiagnostics-color=never", filepath.ToSlash(filepath.Join(root, "src", "a.cpp")))
	if a.Dir != filepath.Join(root, "build") {
		t.Errorf("dir = %q, want the compile_commands directory", a.Dir)
	}
	// no entry for b.c: default C compiler with default flags
	wantArgv(t, step(t, p, "cpp.syntax#2"), "/usr/bin/clang", "-fsyntax-only", "-fdiagnostics-color=never", "src/b.c")
	if len(p.Steps) != 2 {
		t.Errorf("%d steps, want 2: headers are not translation units", len(p.Steps))
	}
}

func TestSplitCommandHandlesQuotingLikeAShell(t *testing.T) {
	got := splitCommand(`cc -DNAME="a b" -I'/x y/inc' -DQ=\"q\" plain\ space "" end`)
	want := []string{"cc", "-DNAME=a b", "-I/x y/inc", `-DQ="q"`, "plain space", "", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCommand = %q, want %q", got, want)
	}
}

func TestCustomStepsExpandFilesAndRespectLanguageFilter(t *testing.T) {
	root := tree(t, map[string]string{"web/a.ts": "", "api/b.py": ""})
	c := cfg(t, func(c *config.Config) {
		c.Steps = []config.CustomStep{
			{ID: "ts.i18n", Tier: 0, Langs: []string{"ts"}, Cmd: []string{"node", "scripts/i18n.js", "{files}"}, Parser: "generic"},
			{ID: "rs.deny", Tier: 1, Langs: []string{"rs"}, Cmd: []string{"cargo", "deny", "check"}},
			{ID: "all.license", Tier: 1, Shell: "scripts/license.sh"},
		}
	})
	p := Build(root, []string{"api/b.py", "web/a.ts"}, c, env(root, "linux", "node"))
	wantArgv(t, step(t, p, "ts.i18n"), "node", "scripts/i18n.js", "web/a.ts")
	noStep(t, p, "rs.deny") // no Rust file changed
	lic := step(t, p, "all.license")
	if lic.Shell != "scripts/license.sh" || lic.Tier != 1 {
		t.Errorf("shell step = %+v", lic)
	}
}

func TestDisabledAndOffStepsAreNotPlanned(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module lab\n", "a.go": "", "x.py": "", ".venv/bin/ruff": ""})
	c := cfg(t, func(c *config.Config) {
		c.Disable = []string{"go.test"}
		c.Python.Lint = "off"
	})
	p := Build(root, []string{"a.go", "x.py"}, c, env(root, "linux", "go", "gofmt"))
	noStep(t, p, "go.test")
	noStep(t, p, "py.lint")
	step(t, p, "go.vet")
}

func TestLongFileListsAreSplitBelowTheCommandLineLimit(t *testing.T) {
	files := map[string]string{"biome.json": "{}", "node_modules/@biomejs/cli-win32-x64/biome.exe": ""}
	var changed []string
	for i := 0; i < 600; i++ {
		f := "src/components/some/deeply/nested/feature/module/component_number_" + strings.Repeat("x", 10) + "_" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + ".tsx"
		files[f] = ""
		changed = append(changed, f)
	}
	root := tree(t, files)
	p := Build(root, changed, cfg(t), env(root, "windows"))
	total := 0
	for _, s := range p.Steps {
		if !strings.HasPrefix(s.ID, "ts.lint") {
			continue
		}
		if n := len(strings.Join(s.Argv, " ")); n > 30000 {
			t.Errorf("%s command line is %d chars; Windows rejects more than 32767", s.ID, n)
		}
		for _, a := range s.Argv {
			if strings.HasSuffix(a, ".tsx") {
				total++
			}
		}
	}
	if total != 600 {
		t.Errorf("chunks cover %d files, want all 600", total)
	}
}

func hasEnv(s *Step, kv string) bool {
	for _, e := range s.Env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestToolCachesAreKeptInsideTheStateDirectory(t *testing.T) {
	root := tree(t, map[string]string{
		"pyproject.toml": "", "app/x.py": "", ".venv/bin/ruff": "", ".venv/bin/pytest": "", ".venv/lib/python3.13/site-packages/testmon/__init__.py": "",
	})
	e := env(root, "linux")
	p := Build(root, []string{"app/x.py"}, cfg(t), e)
	if s := step(t, p, "py.lint"); !hasEnv(s, "RUFF_CACHE_DIR="+filepath.Join(e.CacheDir, "ruff")) {
		t.Errorf("py.lint env = %v, want RUFF_CACHE_DIR under the cache dir (no .ruff_cache in the project)", s.Env)
	}
	if s := step(t, p, "py.test"); !hasEnv(s, "TESTMON_DATAFILE="+filepath.Join(e.CacheDir, "testmon-root")) {
		t.Errorf("py.test env = %v, want TESTMON_DATAFILE under the cache dir (no .testmondata in the project)", s.Env)
	}
}

func TestNativeTypeCheckerGetsFewerCheckersOnABusyMachine(t *testing.T) {
	files := map[string]string{"tsconfig.fast-check.json": "{}", "src/a.ts": "", "node_modules/@typescript/typescript-linux-x64/lib/tsc": "bin"}
	root := tree(t, files)
	e := env(root, "linux")
	e.Threads = 2
	wantArgv(t, step(t, Build(root, []string{"src/a.ts"}, cfg(t), e), "ts.types"),
		abs(root, "node_modules/@typescript/typescript-linux-x64/lib/tsc"), "-p", "tsconfig.fast-check.json", "--noEmit", "--pretty", "false", "--checkers", "2")
	e.Threads = 8 // plenty of idle cores: leave the compiler's default alone
	wantArgv(t, step(t, Build(root, []string{"src/a.ts"}, cfg(t), e), "ts.types"),
		abs(root, "node_modules/@typescript/typescript-linux-x64/lib/tsc"), "-p", "tsconfig.fast-check.json", "--noEmit", "--pretty", "false")
}

func TestSplitCommandWindowsKeepsBackslashes(t *testing.T) {
	got := splitCommandWindows(`C:\PROGRA~1\LLVM\bin\clang-cl.exe /nologo -IC:\proj\include "/DNAME=\"a b\"" "-IC:\dir with space\inc\\" /FoCMakeFiles\x.dir\a.cpp.obj -c C:\proj\src\a.cpp`)
	want := []string{`C:\PROGRA~1\LLVM\bin\clang-cl.exe`, "/nologo", `-IC:\proj\include`, `/DNAME="a b"`, `-IC:\dir with space\inc\`, `/FoCMakeFiles\x.dir\a.cpp.obj`, "-c", `C:\proj\src\a.cpp`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCommandWindows =\n  %q\nwant\n  %q", got, want)
	}
}

func TestCppSyntaxOnWindowsKeepsMSVCFlagsAndDropsOutputs(t *testing.T) {
	root := tree(t, map[string]string{"src/a.cpp": ""})
	src := filepath.ToSlash(filepath.Join(root, "src", "a.cpp"))
	entry := map[string]string{
		"directory": filepath.ToSlash(filepath.Join(root, "build")),
		"file":      src,
		"command":   `C:\PROGRA~1\MSVC\bin\cl.exe /nologo /TP -IC:\proj\include /DWIN32 /FoCMakeFiles\x.dir\a.cpp.obj /FdCMakeFiles\x.dir\ /FS -c ` + src,
	}
	data, _ := json.Marshal([]map[string]string{entry})
	os.MkdirAll(filepath.Join(root, "build"), 0o755)
	os.WriteFile(filepath.Join(root, "build", "compile_commands.json"), data, 0o644)

	p := Build(root, []string{"src/a.cpp"}, cfg(t), env(root, "windows"))
	wantArgv(t, step(t, p, "cpp.syntax"), `C:\PROGRA~1\MSVC\bin\cl.exe`, "/nologo", "/TP", `-IC:\proj\include`, "/DWIN32", "/FS", "/Zs", src)
}

// goList fakes `go list`: three packages import the changed one.
func goList(root string) RunFunc {
	return func(argv []string, dir string) ([]byte, error) {
		var b strings.Builder
		for _, p := range []string{"shared|", "a|lab/shared,", "b|lab/shared,", "c|lab/shared,", "d|fmt,"} {
			name, imports, _ := strings.Cut(p, "|")
			b.WriteString("lab/" + name + "|" + filepath.Join(root, name) + "|" + imports + ",\n")
		}
		return []byte(b.String()), nil
	}
}

func TestGoTestsAreDeferredWhenTheBlastRadiusIsTooWide(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module lab\n", "shared/s.go": "", "a/a.go": "", "b/b.go": "", "c/c.go": "", "d/d.go": ""})
	c := cfg(t, func(c *config.Config) { c.Go.MaxTestPackages = 3 })
	p := Build(root, []string{"shared/s.go"}, c, env(root, "linux", "go", "gofmt"))
	vet, test := step(t, p, "go.vet"), step(t, p, "go.test")
	vet.Prepare(goList(root))
	test.Prepare(goList(root))
	wantArgv(t, vet, "/usr/bin/go", "vet", "lab/a", "lab/b", "lab/c", "lab/shared") // type-checking the importers stays local
	if test.Defer != "4 affected packages > go.max_test_packages=3" {
		t.Errorf("go.test Defer = %q, want the four affected packages to exceed the cap of 3", test.Defer)
	}

	c = cfg(t, func(c *config.Config) { c.Go.MaxTestPackages = 4 })
	p = Build(root, []string{"shared/s.go"}, c, env(root, "linux", "go", "gofmt"))
	test = step(t, p, "go.test")
	test.Prepare(goList(root))
	if test.Defer != "" {
		t.Errorf("go.test deferred (%q) although 4 packages fit a cap of 4", test.Defer)
	}
	wantArgv(t, test, "/usr/bin/go", "test", "-json", "-vet=off", "lab/a", "lab/b", "lab/c", "lab/shared")
}

func TestCppSyntaxDropsCompilerCacheLaunchers(t *testing.T) {
	root := tree(t, map[string]string{"src/a.cpp": ""})
	src := filepath.ToSlash(filepath.Join(root, "src", "a.cpp"))
	entry := map[string]any{"directory": filepath.ToSlash(root), "file": src, "arguments": []string{"/usr/bin/sccache", "/usr/bin/c++", "-O2", "-o", "a.o", "-c", src}}
	data, _ := json.Marshal([]map[string]any{entry})
	os.WriteFile(filepath.Join(root, "compile_commands.json"), data, 0o644)
	p := Build(root, []string{"src/a.cpp"}, cfg(t), env(root, "linux"))
	// a syntax check produces nothing to cache: going through the cache server only adds a hop
	wantArgv(t, step(t, p, "cpp.syntax"), "/usr/bin/c++", "-O2", "-fsyntax-only", "-fdiagnostics-color=never", src)
}

func TestTSSyntaxFallsBackToEsbuildWhenNoLinterIsInstalled(t *testing.T) {
	root := tree(t, map[string]string{
		"package.json": "{}", "apps/api/src/a.ts": "", "apps/api/src/b.tsx": "",
		"node_modules/.pnpm/@esbuild+win32-x64@0.9.7/node_modules/@esbuild/win32-x64/esbuild.exe":  "old",
		"node_modules/.pnpm/@esbuild+win32-x64@0.28.2/node_modules/@esbuild/win32-x64/esbuild.exe": "new",
	})
	e := env(root, "windows")
	p := Build(root, []string{"apps/api/src/a.ts", "apps/api/src/b.tsx"}, cfg(t), e)
	s := step(t, p, "ts.syntax")
	// 0.28 is newer than 0.9: versions are not compared as text.
	wantArgv(t, s, abs(root, "node_modules/.pnpm/@esbuild+win32-x64@0.28.2/node_modules/@esbuild/win32-x64/esbuild.exe"),
		"--log-level=error", "--color=false", "--log-limit=50", "--loader:.js=jsx", "--outbase=.",
		"--outdir="+filepath.Join(e.CacheDir, "esbuild-out"), "apps/api/src/a.ts", "apps/api/src/b.tsx")
	if s.Tier != 0 || s.Dir != root {
		t.Errorf("tier/dir = %d/%q, want the syntax gate in tier 0 at the repository root", s.Tier, s.Dir)
	}
	if hasNote(p, "ts.lint skipped") {
		t.Errorf("notes = %v: a syntax gate is running, there is nothing to warn about", p.Notes)
	}

	// Linux layout of the same package
	root = tree(t, map[string]string{"src/a.ts": "", "node_modules/@esbuild/linux-x64/bin/esbuild": "bin"})
	if got := step(t, Build(root, []string{"src/a.ts"}, cfg(t), env(root, "linux")), "ts.syntax").Argv[0]; got != abs(root, "node_modules/@esbuild/linux-x64/bin/esbuild") {
		t.Errorf("esbuild = %q", got)
	}

	// A linter the project asked for is still reported missing; the gate can be switched off.
	want := cfg(t, func(c *config.Config) { c.TS.Lint = "biome" })
	p = Build(root, []string{"src/a.ts"}, want, env(root, "linux"))
	if step(t, p, "ts.syntax"); !hasNote(p, "ts.lint skipped") {
		t.Errorf("notes = %v, want the missing linter reported next to the fallback", p.Notes)
	}
	off := cfg(t, func(c *config.Config) { c.TS.Syntax = "off" })
	p = Build(root, []string{"src/a.ts"}, off, env(root, "linux"))
	if noStep(t, p, "ts.syntax"); !hasNote(p, "ts.lint skipped") {
		t.Errorf("notes = %v, want the skipped lint reported when nothing replaces it", p.Notes)
	}
}

func TestTSSyntaxKeepsInputsWithTheSameOutputNameApart(t *testing.T) {
	// esbuild writes src/a.js for both src/a.ts and src/a.tsx and refuses to.
	root := tree(t, map[string]string{"src/a.ts": "", "src/a.tsx": "", "src/b.ts": "", "node_modules/@esbuild/linux-x64/bin/esbuild": "bin"})
	e := env(root, "linux")
	p := Build(root, []string{"src/a.ts", "src/a.tsx", "src/b.ts"}, cfg(t), e)
	first, second := step(t, p, "ts.syntax#1"), step(t, p, "ts.syntax#2")
	if got := first.Argv[len(first.Argv)-3:]; !reflect.DeepEqual(got, []string{"--outdir=" + filepath.Join(e.CacheDir, "esbuild-out"), "src/a.ts", "src/b.ts"}) {
		t.Errorf("first = %q", got)
	}
	if got := second.Argv[len(second.Argv)-2:]; !reflect.DeepEqual(got, []string{"--outdir=" + filepath.Join(e.CacheDir, "esbuild-out-2"), "src/a.tsx"}) {
		t.Errorf("second = %q", got)
	}
}

func TestTSTypesUsesTheManagedNativeCompilerWhenTheProjectOnlyHasTheJavaScriptOne(t *testing.T) {
	root := tree(t, map[string]string{"tsconfig.json": "{}", "src/a.ts": "", "node_modules/typescript/bin/tsc": "js"})
	toolsDir := tree(t, map[string]string{"node_modules/@typescript/native-preview-linux-x64/lib/tsgo": "bin"})
	e := env(root, "linux", "node")
	e.ToolsDir = toolsDir
	managed := filepath.Join(toolsDir, "node_modules", "@typescript", "native-preview-linux-x64", "lib", "tsgo")

	s := step(t, Build(root, []string{"src/a.ts"}, cfg(t), e), "ts.types")
	if s.Argv[0] != managed {
		t.Errorf("compiler = %q, want the managed native one %q", s.Argv[0], managed)
	}
	if got := s.MemEnv(1000); !reflect.DeepEqual(got, []string{"GOMEMLIMIT=900MiB"}) {
		t.Errorf("MemEnv = %v, want the Go soft limit of a native compiler", got)
	}

	js := cfg(t, func(c *config.Config) { c.TS.Types = "tsc" })
	if argv := step(t, Build(root, []string{"src/a.ts"}, js, e), "ts.types").Argv; argv[0] != "/usr/bin/node" || argv[1] != abs(root, "node_modules/typescript/bin/tsc") {
		t.Errorf("ts.types=tsc ran %q, want the project's own compiler", argv[:2])
	}

	native := cfg(t, func(c *config.Config) { c.TS.Types = "native" })
	e.ToolsDir = ""
	p := Build(root, []string{"src/a.ts"}, native, e)
	noStep(t, p, "ts.types")
	if !hasNote(p, "ai-guard tools install") {
		t.Errorf("notes = %v, want a pointer to `ai-guard tools install`", p.Notes)
	}
}

func TestTSTestsHonourTheProjectsUnitTestScript(t *testing.T) {
	root := tree(t, map[string]string{
		"apps/api/package.json": `{"scripts": {"test": "vitest run --coverage", "test:unit": "vitest run --project unit --coverage", "test:int": "vitest run --project int"}}`,
		"apps/api/src/a.ts":     "", "node_modules/vitest/vitest.mjs": "js",
	})
	e := env(root, "linux", "node")
	// Only the unit project runs locally: the integration one needs a database.
	wantArgv(t, step(t, Build(root, []string{"apps/api/src/a.ts"}, cfg(t), e), "ts.test@apps/api"),
		"/usr/bin/node", abs(root, "node_modules/vitest/vitest.mjs"), "related", "--run", "--reporter=github-actions", "--passWithNoTests", "--maxWorkers=2", "--project", "unit", "src/a.ts")

	c := cfg(t, func(c *config.Config) { c.TS.TestArgs = []string{"--project", "fast", "--maxWorkers=1"} })
	wantArgv(t, step(t, Build(root, []string{"apps/api/src/a.ts"}, c, e), "ts.test@apps/api"),
		"/usr/bin/node", abs(root, "node_modules/vitest/vitest.mjs"), "related", "--run", "--reporter=github-actions", "--passWithNoTests", "--project", "fast", "--maxWorkers=1", "src/a.ts")
}

func TestESLintRunsOnTheDeltaOnlyWhenEnabled(t *testing.T) {
	root := tree(t, map[string]string{
		"eslint.config.js": "", "apps/api/package.json": `{"scripts": {"lint": "eslint . --max-warnings 0"}}`, "apps/api/src/a.ts": "",
		"legacy/package.json": "{}", "legacy/.eslintrc.json": "{}", "legacy/b.js": "", "node_modules/eslint/bin/eslint.js": "js",
	})
	e := env(root, "linux", "node")
	files := []string{"apps/api/src/a.ts", "legacy/b.js"}
	noStep(t, Build(root, files, cfg(t), e), "ts.eslint") // a type-aware config costs a whole program per run: opt-in

	on := cfg(t, func(c *config.Config) { c.Enable = []string{"ts.eslint"} })
	p := Build(root, files, on, e)
	s := step(t, p, "ts.eslint@apps/api")
	// The project's own strictness (warnings fail its lint script) carries over.
	wantArgv(t, s, "/usr/bin/node", abs(root, "node_modules/eslint/bin/eslint.js"), "--format", "json", "--no-warn-ignored", "--max-warnings", "0", "src/a.ts")
	if s.Tier != 1 || s.Phase != 0 || s.Dir != abs(root, "apps/api") || s.Stream != Stdout {
		t.Errorf("tier/phase/dir/stream = %d/%d/%q/%v; want a tier-1 static step run from the package, reading stdout", s.Tier, s.Phase, s.Dir, s.Stream)
	}
	// --no-warn-ignored only exists for flat configs; root eslint.config.js does not cover a package with its own .eslintrc.
	wantArgv(t, step(t, p, "ts.eslint@legacy"), "/usr/bin/node", abs(root, "node_modules/eslint/bin/eslint.js"), "--format", "json", "b.js")
}

func TestTestRunnerWorkersFollowTheCoresTheStepMayUse(t *testing.T) {
	root := tree(t, map[string]string{"package.json": "{}", "src/a.ts": "", "node_modules/vitest/vitest.mjs": "js"})
	// Each worker is a process with its own copy of the module graph: two when
	// nothing is known, one on a saturated machine, never more than four.
	for threads, want := range map[int]string{0: "--maxWorkers=2", 1: "--maxWorkers=1", 3: "--maxWorkers=3", 16: "--maxWorkers=4"} {
		e := env(root, "linux", "node")
		e.Threads = threads
		if got := step(t, Build(root, []string{"src/a.ts"}, cfg(t), e), "ts.test").Argv[6]; got != want {
			t.Errorf("%d threads: %s, want %s", threads, got, want)
		}
	}
}
