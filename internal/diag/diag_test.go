package diag

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ctx builds a context whose root and cwd are OS-native absolute paths, and
// returns the slash form of the root for embedding into fixtures.
func ctx(t *testing.T, cwdRel string) (Ctx, string) {
	t.Helper()
	root := t.TempDir()
	return Ctx{Root: root, Cwd: filepath.Join(root, filepath.FromSlash(cwdRel))}, filepath.ToSlash(root)
}

func check(t *testing.T, got, want []D) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		var b strings.Builder
		b.WriteString("got:\n")
		for _, d := range got {
			b.WriteString("  " + show(d) + "\n")
		}
		b.WriteString("want:\n")
		for _, d := range want {
			b.WriteString("  " + show(d) + "\n")
		}
		t.Error(b.String())
	}
}

func show(d D) string {
	return strings.Join([]string{d.File, itoa(d.Line), itoa(d.Col), "[" + d.Rule + "]", d.Msg, map[Severity]string{Error: "E", Warning: "W"}[d.Sev]}, " | ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func TestRelMakesPathsRepositoryRelative(t *testing.T) {
	c, _ := ctx(t, "apps/web")
	abs := filepath.Join(c.Root, "apps", "web", "src", "a.ts")
	outside := filepath.Join(filepath.Dir(c.Root), "elsewhere", "x.h")
	cases := map[string]string{
		"src/a.ts":     "apps/web/src/a.ts", // relative to the tool's cwd
		"./src/a.ts":   "apps/web/src/a.ts",
		"../../go.mod": "go.mod",
		abs:            "apps/web/src/a.ts",
		"  src/a.ts\r": "apps/web/src/a.ts",
		outside:        filepath.ToSlash(outside), // outside the repo: keep it absolute
		"":             "",
	}
	for in, want := range cases {
		if got := c.Rel(in); got != want {
			t.Errorf("Rel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitColonLocation(t *testing.T) {
	cases := []struct {
		in        string
		file      string
		line, col int
		rest      string
		ok        bool
	}{
		{"src/a.ts:12:5: msg here", "src/a.ts", 12, 5, "msg here", true},
		{"pkg/a_test.go:7: no column", "pkg/a_test.go", 7, 0, "no column", true},
		{`C:\proj\src\a.ts:12:5: error: bad`, `C:\proj\src\a.ts`, 12, 5, "error: bad", true},
		{`C:/proj/src/a.py:3:1: F401 unused`, `C:/proj/src/a.py`, 3, 1, "F401 unused", true},
		{"new dir/ñ file.rs:1:2: error: x", "new dir/ñ file.rs", 1, 2, "error: x", true},
		{"a.c:3:23: error: has: colons: inside", "a.c", 3, 23, "error: has: colons: inside", true},
		{"Found 8 errors.", "", 0, 0, "", false},
		{"12:30:45: a timestamp, not a file", "", 0, 0, "", false},
		{"error: could not compile `lab`", "", 0, 0, "", false},
	}
	for _, c := range cases {
		file, line, col, rest, ok := splitColon(c.in)
		if ok != c.ok || file != c.file || line != c.line || col != c.col || rest != c.rest {
			t.Errorf("splitColon(%q) = (%q, %d, %d, %q, %v), want (%q, %d, %d, %q, %v)", c.in, file, line, col, rest, ok, c.file, c.line, c.col, c.rest, c.ok)
		}
	}
}

func TestBiomeGithubReporter(t *testing.T) {
	c, root := ctx(t, ".")
	out := "::warning title=lint/correctness/noUnusedVariables,file=" + root + "/src/bad_lint.ts,line=3,endLine=3,col=7,endColumn=13::This variable unused is unused.\n" +
		"::error title=lint/suspicious/noDebugger,file=" + root + "/src/bad_lint.ts,line=2,endLine=2,col=3,endColumn=12::This is an unexpected use of the debugger statement.\n" +
		"::error title=parse,file=" + root + "/src/bad_syntax.ts,line=1,endLine=1,col=34,endColumn=35::expected `,` but instead found `{`\n" +
		"lint ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n  × Some errors were emitted while running checks.\n"
	check(t, Biome([]byte(out), c), []D{
		{File: "src/bad_lint.ts", Line: 3, Col: 7, Rule: "noUnusedVariables", Msg: "This variable unused is unused.", Sev: Warning},
		{File: "src/bad_lint.ts", Line: 2, Col: 3, Rule: "noDebugger", Msg: "This is an unexpected use of the debugger statement."},
		{File: "src/bad_syntax.ts", Line: 1, Col: 34, Rule: "parse", Msg: "expected `,` but instead found `{`"},
	})
}

func TestBiomeRelativeFileAndEscapes(t *testing.T) {
	c, _ := ctx(t, "apps/web")
	out := "::error title=lint/a11y/x,file=src/a%2Cb.tsx,line=1,col=2::50%25 done%0Asecond line\r\n"
	check(t, Biome([]byte(out), c), []D{{File: "apps/web/src/a,b.tsx", Line: 1, Col: 2, Rule: "x", Msg: "50% done"}})
}

func TestVitestGithubActionsReporter(t *testing.T) {
	c, root := ctx(t, ".")
	out := "\n::error file=" + root + "/test/calc.test.ts,title=test/calc.test.ts > math > add,line=5,column=21::AssertionError: expected 3 to be 4 // Object.is equality%0A%0A- Expected%0A+ Received%0A%0A- 4%0A+ 3%0A%0A ❯ test/calc.test.ts:5:21%0A%0A\n"
	check(t, Vitest([]byte(out), c), []D{
		{File: "test/calc.test.ts", Line: 5, Col: 21, Rule: "math > add", Msg: "AssertionError: expected 3 to be 4 // Object.is equality"},
	})
}

func TestRuffConcise(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "app/bad_lint.py:1:8: F401 [*] `os` imported but unused\n" +
		"app/bad_lint.py:6:12: F821 Undefined name `undefined_name`\n" +
		"app/bad_syntax.py:1:14: invalid-syntax: Expected `)`, found newline\n" +
		"Found 8 errors.\n[*] 3 fixable with `--fix` (1 unsafe fix available with `--fix --unsafe-fixes`).\n"
	check(t, Ruff([]byte(out), c), []D{
		{File: "app/bad_lint.py", Line: 1, Col: 8, Rule: "F401", Msg: "`os` imported but unused"},
		{File: "app/bad_lint.py", Line: 6, Col: 12, Rule: "F821", Msg: "Undefined name `undefined_name`"},
		{File: "app/bad_syntax.py", Line: 1, Col: 14, Rule: "invalid-syntax", Msg: "Expected `)`, found newline"},
	})
}

func TestOxlintUnix(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "src/bad_syntax.ts:1:34: Expected `,` or `)` but found `{` [Error]\n" +
		"src/bad_lint.ts:3:7: Variable 'unused' is declared but never used. Unused variables should start with a '_'. [Warning/eslint(no-unused-vars)]\n" +
		"src/bad_lint.ts:2:3: `debugger` statement is not allowed [Error/eslint(no-debugger)]\n\n3 problems\n"
	check(t, Oxlint([]byte(out), c), []D{
		{File: "src/bad_syntax.ts", Line: 1, Col: 34, Msg: "Expected `,` or `)` but found `{`"},
		{File: "src/bad_lint.ts", Line: 3, Col: 7, Rule: "no-unused-vars", Msg: "Variable 'unused' is declared but never used. Unused variables should start with a '_'.", Sev: Warning},
		{File: "src/bad_lint.ts", Line: 2, Col: 3, Rule: "no-debugger", Msg: "`debugger` statement is not allowed"},
	})
}

func TestTSCPlainOutput(t *testing.T) {
	c, _ := ctx(t, "apps/web")
	out := "src/bad_syntax.ts(1,34): error TS1005: ',' expected.\n" +
		"src/bad_type.ts(2,48): error TS2322: Type 'string' is not assignable to type 'number'.\n" +
		"error TS5083: Cannot read file '/x/tsconfig.json'.\n" +
		"\nFound 2 errors in 2 files.\n"
	check(t, TSC([]byte(out), c), []D{
		{File: "apps/web/src/bad_syntax.ts", Line: 1, Col: 34, Rule: "TS1005", Msg: "',' expected."},
		{File: "apps/web/src/bad_type.ts", Line: 2, Col: 48, Rule: "TS2322", Msg: "Type 'string' is not assignable to type 'number'."},
		{Rule: "TS5083", Msg: "Cannot read file '/x/tsconfig.json'."},
	})
}

func TestTSCParserAlsoReadsMSVC(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "src\\a.cpp(12,5): error C2065: 'x': undeclared identifier\nsrc\\b.cpp(7): warning C4101: 'y': unreferenced local variable\n"
	got := TSC([]byte(out), c)
	if len(got) != 2 || got[0].Line != 12 || got[0].Col != 5 || got[0].Rule != "C2065" || got[0].Msg != "'x': undeclared identifier" ||
		got[1].Line != 7 || got[1].Col != 0 || got[1].Rule != "C4101" || got[1].Sev != Warning {
		t.Errorf("MSVC lines parsed as %+v", got)
	}
}

func TestGCCStyleCoversClangCargoMypyAndTy(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "bad.cpp:3:23: error: cannot initialize a variable of type 'int' with an lvalue of type 'const char[2]'\n" +
		"/usr/include/c++/13/bits/basic_string.h:547:7: note: candidate constructor not viable\n" +
		"bad.cpp: In function 'std::string f()':\n" +
		"bad.cpp:3:27: error: invalid conversion from 'const char*' to 'int' [-fpermissive]\n" +
		"bad.cpp:9:7: warning: unused variable 'x' [-Wunused-variable]\n" +
		"src/bad.rs:2:18: error[E0308]: mismatched types: expected `i32`, found `&str`\n" +
		"src/bad.rs:3:9: warning: unused variable: `unused`: help: if this is intentional, prefix it with an underscore: `_unused`\n" +
		"error: could not compile `lab` (lib) due to 2 previous errors\n" +
		"app/bad_type.py:4:14: error: Argument 1 to \"add\" has incompatible type \"str\"; expected \"int\"  [arg-type]\n" +
		"app/bad_type.py:4: note: See https://mypy.readthedocs.io/x\n" +
		"app/bad_type.py:5:10: error[invalid-assignment] Object of type `int` is not assignable to `str`\n" +
		"Found 2 diagnostics\n"
	check(t, GCC([]byte(out), c), []D{
		{File: "bad.cpp", Line: 3, Col: 23, Msg: "cannot initialize a variable of type 'int' with an lvalue of type 'const char[2]'"},
		{File: "bad.cpp", Line: 3, Col: 27, Rule: "-fpermissive", Msg: "invalid conversion from 'const char*' to 'int'"},
		{File: "bad.cpp", Line: 9, Col: 7, Rule: "-Wunused-variable", Msg: "unused variable 'x'", Sev: Warning},
		{File: "src/bad.rs", Line: 2, Col: 18, Rule: "E0308", Msg: "mismatched types: expected `i32`, found `&str`"},
		{File: "src/bad.rs", Line: 3, Col: 9, Msg: "unused variable: `unused`: help: if this is intentional, prefix it with an underscore: `_unused`", Sev: Warning},
		{File: "app/bad_type.py", Line: 4, Col: 14, Rule: "arg-type", Msg: "Argument 1 to \"add\" has incompatible type \"str\"; expected \"int\""},
		{File: "app/bad_type.py", Line: 5, Col: 10, Rule: "invalid-assignment", Msg: "Object of type `int` is not assignable to `str`"},
	})
}

func TestGoToolOutput(t *testing.T) {
	c, _ := ctx(t, "services/api")
	out := "# lab/pkg/b\n" +
		"vet: pkg/b/b.go:9:17: cannot use a.Add(1, 2) (value of type int) as string value in variable declaration\n" +
		"./main.go:10:2: fmt.Printf format %d has arg \"x\" of wrong type string\n" +
		"pkg/b/bad_syntax.go:3:19: missing ',' in parameter list\n"
	check(t, GoTool([]byte(out), c), []D{
		{File: "services/api/pkg/b/b.go", Line: 9, Col: 17, Msg: "cannot use a.Add(1, 2) (value of type int) as string value in variable declaration"},
		{File: "services/api/main.go", Line: 10, Col: 2, Msg: "fmt.Printf format %d has arg \"x\" of wrong type string"},
		{File: "services/api/pkg/b/bad_syntax.go", Line: 3, Col: 19, Msg: "missing ',' in parameter list"},
	})
}

func TestGoTestJSON(t *testing.T) {
	c, _ := ctx(t, ".")
	c.PkgDir = map[string]string{"lab/pkg/a": filepath.Join(c.Root, "pkg", "a"), "lab/pkg/b": filepath.Join(c.Root, "pkg", "b"), "lab/pkg/c": filepath.Join(c.Root, "pkg", "c")}
	out := `{"ImportPath":"lab/pkg/b","Action":"build-output","Output":"# lab/pkg/b\n"}
{"ImportPath":"lab/pkg/b","Action":"build-output","Output":"pkg/b/b.go:6:17: cannot use a.Add(1, 2) (value of type int) as string value in variable declaration\n"}
{"ImportPath":"lab/pkg/b","Action":"build-fail"}
{"Time":"2026-11-10T04:30:55.753042411Z","Action":"start","Package":"lab/pkg/a"}
{"Time":"2026-11-10T04:30:55.753236168Z","Action":"output","Package":"lab/pkg/b","Output":"FAIL\tlab/pkg/b [build failed]\n"}
{"Time":"2026-11-10T04:30:55.753281751Z","Action":"fail","Package":"lab/pkg/b","Elapsed":0,"FailedBuild":"lab/pkg/b"}
{"Time":"2026-11-10T04:30:55.755552652Z","Action":"run","Package":"lab/pkg/a","Test":"TestAdd"}
{"Time":"2026-11-10T04:30:55.755609232Z","Action":"output","Package":"lab/pkg/a","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Time":"2026-11-10T04:30:55.755644236Z","Action":"output","Package":"lab/pkg/a","Test":"TestAdd","Output":"    a_test.go:7: Add(1,2) = 3, want 4\n"}
{"Time":"2026-11-10T04:30:55.755656002Z","Action":"output","Package":"lab/pkg/a","Test":"TestAdd","Output":"--- FAIL: TestAdd (0.00s)\n"}
{"Time":"2026-11-10T04:30:55.755663418Z","Action":"fail","Package":"lab/pkg/a","Test":"TestAdd","Elapsed":0}
{"Time":"2026-11-10T04:30:55.755692312Z","Action":"output","Package":"lab/pkg/a","Output":"FAIL\n"}
{"Time":"2026-11-10T04:30:55.755867671Z","Action":"fail","Package":"lab/pkg/a","Elapsed":0.003}
{"Action":"run","Package":"lab/pkg/c","Test":"TestTable"}
{"Action":"run","Package":"lab/pkg/c","Test":"TestTable/empty"}
{"Action":"output","Package":"lab/pkg/c","Test":"TestTable/empty","Output":"    c_test.go:21: got 1, want 0\n"}
{"Action":"fail","Package":"lab/pkg/c","Test":"TestTable/empty","Elapsed":0}
{"Action":"fail","Package":"lab/pkg/c","Test":"TestTable","Elapsed":0}
{"Action":"run","Package":"lab/pkg/c","Test":"TestBoom"}
{"Action":"output","Package":"lab/pkg/c","Test":"TestBoom","Output":"panic: runtime error: index out of range [5] with length 3 [recovered]\n"}
{"Action":"output","Package":"lab/pkg/c","Test":"TestBoom","Output":"goroutine 7 [running]:\n"}
{"Action":"fail","Package":"lab/pkg/c","Test":"TestBoom","Elapsed":0}
{"Action":"fail","Package":"lab/pkg/c","Elapsed":0.01}
{"Action":"pass","Package":"lab/pkg/d","Elapsed":0.01}
`
	check(t, GoTest([]byte(out), c), []D{
		{File: "pkg/b/b.go", Line: 6, Col: 17, Msg: "cannot use a.Add(1, 2) (value of type int) as string value in variable declaration"},
		{File: "pkg/a/a_test.go", Line: 7, Rule: "TestAdd", Msg: "Add(1,2) = 3, want 4"},
		{File: "pkg/c/c_test.go", Line: 21, Rule: "TestTable/empty", Msg: "got 1, want 0"},
		{File: "pkg/c", Rule: "TestBoom", Msg: "panic: runtime error: index out of range [5] with length 3 [recovered]"},
	})
}

func TestGoTestPackageFailureWithoutAFailingTest(t *testing.T) {
	c, _ := ctx(t, ".")
	c.PkgDir = map[string]string{"lab/pkg/e": filepath.Join(c.Root, "pkg", "e")}
	out := `{"Action":"start","Package":"lab/pkg/e"}
{"Action":"output","Package":"lab/pkg/e","Output":"setup: cannot open fixtures\n"}
{"Action":"output","Package":"lab/pkg/e","Output":"FAIL\tlab/pkg/e\t0.002s\n"}
{"Action":"fail","Package":"lab/pkg/e","Elapsed":0.002}
`
	check(t, GoTest([]byte(out), c), []D{{File: "pkg/e", Msg: "package tests failed: setup: cannot open fixtures"}})
}

func TestPyrightJSON(t *testing.T) {
	c, root := ctx(t, ".")
	out := `{
    "version": "1.1.414",
    "generalDiagnostics": [
        {"file": "` + root + `/app/bad_type.py", "severity": "error",
         "message": "Argument of type \"Literal['1']\" cannot be assigned to parameter \"a\" of type \"int\" in function \"add\"\n  \"Literal['1']\" is not assignable to \"int\"",
         "range": {"start": {"line": 3, "character": 13}, "end": {"line": 3, "character": 16}}, "rule": "reportArgumentType"},
        {"file": "` + root + `/app/x.py", "severity": "warning", "message": "Import \"y\" is not accessed",
         "range": {"start": {"line": 0, "character": 7}, "end": {"line": 0, "character": 8}}, "rule": "reportUnusedImport"},
        {"file": "` + root + `/app/x.py", "severity": "information", "message": "noise", "range": {"start": {"line": 1, "character": 0}, "end": {"line": 1, "character": 1}}}
    ],
    "summary": {"filesAnalyzed": 1, "errorCount": 1, "warningCount": 1, "informationCount": 1, "timeInSec": 0.4}
}`
	check(t, Pyright([]byte("some banner\n"+out), c), []D{
		{File: "app/bad_type.py", Line: 4, Col: 14, Rule: "reportArgumentType", Msg: "Argument of type \"Literal['1']\" cannot be assigned to parameter \"a\" of type \"int\" in function \"add\""},
		{File: "app/x.py", Line: 1, Col: 8, Rule: "reportUnusedImport", Msg: "Import \"y\" is not accessed", Sev: Warning},
	})
}

func TestPytestLineTracebacksJoinedWithSummary(t *testing.T) {
	c, root := ctx(t, ".")
	out := ".FEF                                                                     [100%]\n" +
		"==================================== ERRORS ====================================\n" +
		"_____________________ ERROR at setup of test_uses_fixture ______________________\n" +
		"E   RuntimeError: fixture exploded\n" +
		"=================================== FAILURES ===================================\n" +
		"E   ValueError: boom in app code\n" +
		root + "/app/calc.py:6: ValueError: boom in app code\n" +
		"E   AssertionError: assert 'A?' == 'A!'\n      \n      - A!\n      + A?\n" +
		root + "/tests/test_text.py:5: AssertionError: assert 'A?' == 'A!'\n" +
		"=========================== short test summary info ============================\n" +
		"FAILED tests/test_calc.py::test_mul - ValueError: boom in app code\n" +
		"FAILED tests/test_text.py::TestShout::test_shout - AssertionError: assert 'A?' == 'A!'\n" +
		"ERROR tests/test_fixture.py::test_uses_fixture - RuntimeError: fixture exploded\n" +
		"2 failed, 1 passed, 1 error in 0.02s\n"
	check(t, Pytest([]byte(out), c), []D{
		{File: "app/calc.py", Line: 6, Rule: "tests/test_calc.py::test_mul", Msg: "ValueError: boom in app code"},
		{File: "tests/test_text.py", Line: 5, Rule: "TestShout::test_shout", Msg: "AssertionError: assert 'A?' == 'A!'"},
		{File: "tests/test_fixture.py", Rule: "test_uses_fixture", Msg: "RuntimeError: fixture exploded"},
	})
}

func TestPytestCollectionErrors(t *testing.T) {
	c, root := ctx(t, ".")
	out := "\n==================================== ERRORS ====================================\n" +
		"_________________ ERROR collecting tests/test_broken_import.py _________________\n" +
		"ImportError while importing test module '" + root + "/tests/test_broken_import.py'.\n" +
		"Traceback:\n" +
		"/usr/local/lib/python3.13/importlib/__init__.py:88: in import_module\n" +
		"    return _bootstrap._gcd_import(name[level:], package, level)\n" +
		"tests/test_broken_import.py:1: in <module>\n" +
		"    from app.nope import missing\n" +
		"E   ModuleNotFoundError: No module named 'app.nope'\n" +
		"____________________ ERROR collecting tests/test_syntax.py _____________________\n" +
		".venv/lib/python3.13/site-packages/_pytest/python.py:528: in importtestmodule\n" +
		"    mod = import_path(\n" +
		"E     File \"" + root + "/tests/test_syntax.py\", line 3\n" +
		"E       def test_y(:\n" +
		"E                  ^\n" +
		"E   SyntaxError: invalid syntax\n" +
		"=========================== short test summary info ============================\n" +
		"ERROR tests/test_broken_import.py\n" +
		"ERROR tests/test_syntax.py\n" +
		"!!!!!!!!!!!!!!!!!!!! Interrupted: 2 errors during collection !!!!!!!!!!!!!!!!!!!!\n" +
		"2 errors in 0.08s\n"
	check(t, Pytest([]byte(out), c), []D{
		{File: "tests/test_broken_import.py", Line: 1, Msg: "ModuleNotFoundError: No module named 'app.nope'"},
		{File: "tests/test_syntax.py", Line: 3, Msg: "SyntaxError: invalid syntax"},
	})
}

func TestPytestSummaryAloneIsEnough(t *testing.T) {
	// --tb=no, or a count mismatch between tracebacks and summary lines.
	c, _ := ctx(t, "services/api")
	out := "FAILED tests/test_a.py::test_one - assert 1 == 2\nFAILED tests/test_b.py::test_two\n2 failed in 0.1s\n"
	check(t, Pytest([]byte(out), c), []D{
		{File: "services/api/tests/test_a.py", Rule: "test_one", Msg: "assert 1 == 2"},
		{File: "services/api/tests/test_b.py", Rule: "test_two", Msg: "test failed"},
	})
}

func TestCargoTestPanics(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "src/bad.rs:3:9: warning: unused variable: `unused`\n\nrunning 2 tests\ntests::adds --- FAILED\n\nfailures:\n\n" +
		"---- tests::adds stdout ----\n\n" +
		"thread 'tests::adds' (12246) panicked at src/lib.rs:8:17:\nassertion `left == right` failed\n  left: 3\n right: 4\n" +
		"note: run with `RUST_BACKTRACE=1` environment variable to display a backtrace\n\n" +
		"---- tests::old stdout ----\nthread 'tests::old' panicked at src/old.rs:4:5:\nexplicit panic\n\n" +
		"failures:\n    tests::adds\n    tests::old\n\ntest result: FAILED. 0 passed; 2 failed; 0 ignored\n\nerror: test failed, to rerun pass `--lib`\n"
	check(t, CargoTest([]byte(out), c), []D{
		{File: "src/bad.rs", Line: 3, Col: 9, Msg: "unused variable: `unused`", Sev: Warning},
		{File: "src/lib.rs", Line: 8, Col: 17, Rule: "tests::adds", Msg: "assertion `left == right` failed left: 3 right: 4"},
		{File: "src/old.rs", Line: 4, Col: 5, Rule: "tests::old", Msg: "explicit panic"},
	})
}

func TestRustFullFormat(t *testing.T) {
	c, root := ctx(t, ".")
	out := "error: this file contains an unclosed delimiter\n --> " + root + "/src/bad_syntax.rs:3:3\n  |\n1 | pub fn broken(a: i32 {\n  |              - unclosed delimiter\n\n" +
		"error[E0308]: mismatched types\n  --> src/bad.rs:2:18\n   |\n2 |     let x: i32 = \"s\";\n" +
		"warning: unused variable: `u`\n --> src/bad.rs:3:9\n" +
		"error: aborting due to 2 previous errors\n"
	check(t, RustFull([]byte(out), c), []D{
		{File: "src/bad_syntax.rs", Line: 3, Col: 3, Msg: "this file contains an unclosed delimiter"},
		{File: "src/bad.rs", Line: 2, Col: 18, Rule: "E0308", Msg: "mismatched types"},
		{File: "src/bad.rs", Line: 3, Col: 9, Msg: "unused variable: `u`", Sev: Warning},
	})
}

func TestJestJSON(t *testing.T) {
	c, root := ctx(t, ".")
	out := `{"numFailedTests":1,"success":false,"testResults":[{"name":"` + root + `/test/calc.test.js","status":"failed","message":"  ● calc › adds\n\n    expect(received).toBe(expected)","assertionResults":[{"status":"failed","fullName":"calc adds","location":{"column":3,"line":4},"failureMessages":["Error: expect(received).toBe(expected) // Object.is equality\n\nExpected: 4\nReceived: 3\n    at Object.toBe (` + root + `/test/calc.test.js:5:23)\n    at Promise.finally.completed (` + root + `/node_modules/jest-circus/build/jestAdapterInit.js:1557:28)"]},{"status":"passed","fullName":"calc ok","location":{"column":3,"line":7},"failureMessages":[]}]},{"name":"` + root + `/test/broken.test.js","status":"failed","message":"  ● Test suite failed to run\n\n    SyntaxError: Unexpected token (3:1)\n\n      at x","assertionResults":[]}]}`
	check(t, Jest([]byte(out), c), []D{
		{File: "test/calc.test.js", Line: 5, Col: 23, Rule: "calc adds", Msg: "Error: expect(received).toBe(expected) // Object.is equality Expected: 4 Received: 3"},
		{File: "test/broken.test.js", Msg: "Test suite failed to run SyntaxError: Unexpected token (3:1)"},
	})
}

func TestGenericFindsColonAndParenLocations(t *testing.T) {
	c, _ := ctx(t, ".")
	out := "building...\nlib/a.kt:10:3: error: unresolved reference\nlib/b.cs(4,9): error CS0103: The name 'x' does not exist\nlib/c.swift:2:1: warning: unused\ndone\n"
	check(t, Generic([]byte(out), c), []D{
		{File: "lib/a.kt", Line: 10, Col: 3, Msg: "unresolved reference"},
		{File: "lib/b.cs", Line: 4, Col: 9, Rule: "CS0103", Msg: "The name 'x' does not exist"},
		{File: "lib/c.swift", Line: 2, Col: 1, Msg: "unused", Sev: Warning},
	})
}

func TestByNameKnowsEveryParserUsedInConfigs(t *testing.T) {
	for _, n := range []string{"biome", "vitest", "ruff", "oxlint", "tsc", "gcc", "go", "gotest", "pyright", "pytest", "cargotest", "rust", "jest", "generic", "esbuild", "eslint"} {
		if _, ok := ByName(n); !ok {
			t.Errorf("ByName(%q) is unknown", n)
		}
	}
	if _, ok := ByName("nope"); ok {
		t.Error("ByName accepted an unknown parser name")
	}
}

func TestEsbuildSyntaxErrors(t *testing.T) {
	c, _ := ctx(t, "apps/api")
	out := "\u2718 [ERROR] Unexpected \"=\"\n\n    src/bad2.tsx:2:9:\n      2 \u2502 const x: = 2;\n        \u2575          ^\n\n" +
		"\u2718 [ERROR] Expected \")\" but found \"{\"\n\n    src/deep/bad1.ts:1:33:\n      1 \u2502 export function broken(a: number {\n        \u2502                                  ^\n        \u2575                                  )\n\n" +
		"\u2718 [ERROR] The symbol \"a\" has already been declared\n\n    src/two.ts:2:6:\n      2 \u2502 const a = 2\n        \u2575       ^\n\n" +
		"  The symbol \"a\" was originally declared here:\n\n    src/two.ts:1:6:\n      1 \u2502 const a = 1\n        \u2575       ^\n\n" +
		"\u2718 [ERROR] Two output files share the same path but have different contents: out/a.js\n\n2 errors\n"
	check(t, Esbuild([]byte(out), c), []D{
		{File: "apps/api/src/bad2.tsx", Line: 2, Col: 10, Rule: "syntax", Msg: `Unexpected "="`}, // esbuild columns are 0-based
		{File: "apps/api/src/deep/bad1.ts", Line: 1, Col: 34, Rule: "syntax", Msg: `Expected ")" but found "{"`},
		{File: "apps/api/src/two.ts", Line: 2, Col: 7, Rule: "syntax", Msg: `The symbol "a" has already been declared`},
		// An error with no source location is esbuild's own trouble, not the code's:
		// it is left for the caller to report as a failed check.
	})
}

func TestESLintJSON(t *testing.T) {
	c, root := ctx(t, "apps/api")
	out := `[{"filePath":"` + root + `/apps/api/src/a.ts","messages":[` +
		`{"ruleId":"@typescript-eslint/no-floating-promises","severity":2,"message":"Promises must be awaited.","line":12,"column":3,"endLine":12,"endColumn":20},` +
		`{"ruleId":"no-console","severity":1,"message":"Unexpected console statement.","line":4,"column":1},` +
		`{"ruleId":null,"fatal":true,"severity":2,"message":"Parsing error: ';' expected.","line":7,"column":9}],"errorCount":2,"warningCount":1},` +
		`{"filePath":"` + root + `/apps/api/src/clean.ts","messages":[],"errorCount":0,"warningCount":0}]`
	check(t, ESLint([]byte(out), c), []D{
		{File: "apps/api/src/a.ts", Line: 12, Col: 3, Rule: "@typescript-eslint/no-floating-promises", Msg: "Promises must be awaited."},
		{File: "apps/api/src/a.ts", Line: 4, Col: 1, Rule: "no-console", Msg: "Unexpected console statement.", Sev: Warning},
		{File: "apps/api/src/a.ts", Line: 7, Col: 9, Msg: "Parsing error: ';' expected."},
	})
}
