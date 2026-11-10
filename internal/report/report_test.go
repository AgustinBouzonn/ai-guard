package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
)

func agent(r Result, o Options) string {
	var b bytes.Buffer
	Agent(&b, r, o)
	return b.String()
}

var defaults = Options{MaxErrors: 30, MaxPerFile: 6, MaxMsg: 200}

func TestAgentPassIsOneLine(t *testing.T) {
	r := Result{Status: Pass, Files: 3, PeakMB: 212, Tiers: []Tier{{N: 0, Ms: 41}, {N: 1, Ms: 1203}}}
	if got, want := agent(r, defaults), "OK t0 41ms t1 1203ms files=3 peak=212MB\n"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestAgentNothingToCheck(t *testing.T) {
	if got, want := agent(Result{Status: Pass}, defaults), "OK no changed source files\n"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestAgentGroupsDiagnosticsByFileSortedByPosition(t *testing.T) {
	r := Result{Status: Fail, Files: 2, Tiers: []Tier{{N: 0, Ms: 87}}, Diags: []diag.D{
		{File: "src/a.ts", Line: 40, Col: 1, Rule: "TS2322", Msg: "Type 'string' is not assignable to type 'number'."},
		{File: "app/x.py", Line: 3, Col: 1, Rule: "F401", Msg: "`os` imported but unused"},
		{File: "src/a.ts", Line: 12, Col: 5, Rule: "noDoubleEquals", Msg: "Using == may be unsafe."},
		{File: "src/a.ts", Line: 12, Col: 5, Rule: "noDoubleEquals", Msg: "Using == may be unsafe."}, // duplicate
		{File: "src/a.ts", Line: 7, Msg: "no column, no rule"},
	}}
	want := "FAIL t0 87ms files=2 errors=4\n" +
		"app/x.py\n" +
		"  3:1 F401 `os` imported but unused\n" +
		"src/a.ts\n" +
		"  7 no column, no rule\n" +
		"  12:5 noDoubleEquals Using == may be unsafe.\n" +
		"  40:1 TS2322 Type 'string' is not assignable to type 'number'.\n"
	if got := agent(r, defaults); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentCapsPerFileAndInTotal(t *testing.T) {
	var ds []diag.D
	for i := 1; i <= 5; i++ {
		ds = append(ds, diag.D{File: "a.py", Line: i, Rule: "E1", Msg: "m"})
	}
	for i := 1; i <= 3; i++ {
		ds = append(ds, diag.D{File: "b.py", Line: i, Rule: "E2", Msg: "m"})
	}
	ds = append(ds, diag.D{File: "c.py", Line: 1, Rule: "E3", Msg: "m"})
	r := Result{Status: Fail, Files: 3, Tiers: []Tier{{N: 0, Ms: 5}}, Diags: ds}
	want := "FAIL t0 5ms files=3 errors=9 (showing 4)\n" +
		"a.py\n" +
		"  1 E1 m\n" +
		"  2 E1 m\n" +
		"  +3 more\n" +
		"b.py\n" +
		"  1 E2 m\n" +
		"  2 E2 m\n" +
		"  +1 more\n" +
		"+1 more file\n"
	if got := agent(r, Options{MaxErrors: 4, MaxPerFile: 2, MaxMsg: 200}); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentTruncatesLongMessagesAndFlattensNewlines(t *testing.T) {
	r := Result{Status: Fail, Files: 1, Tiers: []Tier{{N: 1, Ms: 9}}, Diags: []diag.D{
		{File: "a.go", Line: 1, Msg: "first line\n   second\tline " + strings.Repeat("x", 50)},
	}}
	want := "FAIL t1 9ms files=1 errors=1\na.go\n  1 first line second line xxxxxx…\n"
	if got := agent(r, Options{MaxErrors: 30, MaxPerFile: 6, MaxMsg: 30}); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestAgentHidesWarningsUnlessAsked(t *testing.T) {
	r := Result{Status: Fail, Files: 1, Tiers: []Tier{{N: 0, Ms: 3}}, Diags: []diag.D{
		{File: "a.ts", Line: 2, Rule: "noDebugger", Msg: "debugger"},
		{File: "a.ts", Line: 1, Rule: "noUnusedVariables", Msg: "unused", Sev: diag.Warning},
	}}
	if got, want := agent(r, defaults), "FAIL t0 3ms files=1 errors=1\na.ts\n  2 noDebugger debugger\n"; got != want {
		t.Errorf("without warnings got:\n%s\nwant:\n%s", got, want)
	}
	o := defaults
	o.Warnings = true
	if got, want := agent(r, o), "FAIL t0 3ms files=1 errors=1 warnings=1\na.ts\n  1 warn noUnusedVariables unused\n  2 noDebugger debugger\n"; got != want {
		t.Errorf("with warnings got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentShowsFilelessDiagnosticsNotesDeferredAndTier2(t *testing.T) {
	r := Result{
		Status: Deferred, Files: 2, PeakMB: 540,
		Tiers:    []Tier{{N: 0, Ms: 30}, {N: 1, Ms: 2100}},
		Diags:    []diag.D{{Rule: "TS5083", Msg: "Cannot read file 'tsconfig.json'."}},
		Notes:    []string{"py.types skipped: no type checker installed"},
		Deferred: []string{"ts.types: rss 540MB > 512MB budget"},
		Tier2:    &Tier2{ID: "ab12cd", State: "queued", Log: ".aiguard/tier2/ab12cd.log"},
	}
	want := "DEFERRED t0 30ms t1 2100ms files=2 peak=540MB errors=1\n" +
		"- TS5083 Cannot read file 'tsconfig.json'.\n" +
		"> deferred ts.types: rss 540MB > 512MB budget\n" +
		"> tier2 queued id=ab12cd log=.aiguard/tier2/ab12cd.log\n" +
		"! py.types skipped: no type checker installed\n"
	if got := agent(r, defaults); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentInfraFailureStatus(t *testing.T) {
	r := Result{Status: Infra, Files: 1, Tiers: []Tier{{N: 1, Ms: 120000}}, Notes: []string{"go.test killed: timeout after 120s"}}
	if got, want := agent(r, defaults), "ERROR t1 120000ms files=1\n! go.test killed: timeout after 120s\n"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestJSONIsCompactAndComplete(t *testing.T) {
	r := Result{
		Status: Fail, Files: 2, PeakMB: 35,
		Tiers: []Tier{{N: 0, Ms: 87, BudgetMs: 300}},
		Diags: []diag.D{
			{File: "src/a.ts", Line: 12, Col: 5, Rule: "noDoubleEquals", Msg: "Using == may be unsafe."},
			{File: "src/a.ts", Line: 1, Rule: "w", Msg: "warn", Sev: diag.Warning},
		},
		Steps: []Step{{ID: "ts.lint", Tier: 0, Ms: 41, PeakMB: 35, Status: "fail"}},
		Notes: []string{"n1"},
	}
	var b bytes.Buffer
	JSON(&b, r, defaults)
	out := b.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("JSON output must be a single line, got %q", out)
	}
	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	want := `{"status":"fail","ok":false,"files":2,"peak_mb":35,"tiers":[{"tier":0,"ms":87,"budget_ms":300,"over_budget":false}],"errors":1,"warnings":1,"diags":[{"f":"src/a.ts","l":12,"c":5,"r":"noDoubleEquals","m":"Using == may be unsafe."}],"steps":[{"id":"ts.lint","tier":0,"ms":41,"mb":35,"st":"fail"}],"notes":["n1"]}` + "\n"
	if out != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

func TestJSONFlagsTierOverBudget(t *testing.T) {
	var b bytes.Buffer
	JSON(&b, Result{Status: Pass, Files: 1, Tiers: []Tier{{N: 0, Ms: 450, BudgetMs: 300}}}, defaults)
	if !strings.Contains(b.String(), `"over_budget":true`) {
		t.Errorf("a 450ms tier with a 300ms budget is not flagged: %s", b.String())
	}
}

func TestExitCodes(t *testing.T) {
	for st, want := range map[Status]int{Pass: 0, Fail: 1, Infra: 3, Deferred: 4} {
		if got := (Result{Status: st}).ExitCode(); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", st, got, want)
		}
	}
}
