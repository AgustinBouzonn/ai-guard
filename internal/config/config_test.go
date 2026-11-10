package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadWithoutFileYieldsTheDefaultEnvelope(t *testing.T) {
	c, err := Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Tier0.BudgetMs != 300 || c.Tier0.MemMB != 150 {
		t.Errorf("tier0 = %+v, want a 300ms / 150MB target", c.Tier0)
	}
	// Tier 1 keeps the 3 s target, but its memory cap is sized from the machine
	// (0 = auto): no fixed number fits both a script and a monorepo type-check.
	if c.Tier1.BudgetMs != 3000 || c.Tier1.MemMB != 0 || c.Tier1.TimeoutMs != 60000 {
		t.Errorf("tier1 = %+v, want a 3000ms target, automatic memory and a 60s timeout", c.Tier1)
	}
	if c.Tier2.Trigger != "off" {
		t.Errorf("tier2.trigger = %q, want off: remote work must be opt-in", c.Tier2.Trigger)
	}
	if c.TS.Syntax != "auto" || len(c.Enable) != 0 {
		t.Errorf("ts.syntax = %q, enable = %v; want the syntax gate on and no opt-in step", c.TS.Syntax, c.Enable)
	}
}

func TestOptInStepsAreEnabledByID(t *testing.T) {
	c, err := Load(writeCfg(t, `{"enable": ["ts.eslint"], "ts": {"types": "native", "syntax": "off", "test_args": ["--project", "unit"]}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled("ts.eslint") || c.Enabled("ts.types") {
		t.Errorf("Enabled(ts.eslint) = %v, Enabled(ts.types) = %v; want true, false", c.Enabled("ts.eslint"), c.Enabled("ts.types"))
	}
	if c.TS.Types != "native" || c.TS.Syntax != "off" || len(c.TS.TestArgs) != 2 {
		t.Errorf("ts = %+v", c.TS)
	}
}

func TestLoadOverridesOnlyTheKeysPresentInTheFile(t *testing.T) {
	root := writeCfg(t, `{"tier1": {"mem_mb": 1024}, "python": {"types": "ty"}, "disable": ["rs.test"]}`)
	c, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Tier1.MemMB != 1024 {
		t.Errorf("tier1.mem_mb = %d, want 1024 from the file", c.Tier1.MemMB)
	}
	if c.Tier1.BudgetMs != 3000 || c.Tier1.TimeoutMs != 60000 {
		t.Errorf("tier1 = %+v: keys absent from the file must keep their defaults", c.Tier1)
	}
	if c.Python.Types != "ty" || c.Python.Lint != "auto" {
		t.Errorf("python = %+v, want types=ty and lint still auto", c.Python)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	root := writeCfg(t, `{"tier1": {"mem_mbb": 1024}}`)
	_, err := Load(root, "")
	if err == nil || !strings.Contains(err.Error(), "mem_mbb") {
		t.Errorf("err = %v, want a complaint naming the misspelt key mem_mbb", err)
	}
}

func TestLoadRejectsInvalidChoices(t *testing.T) {
	for _, body := range []string{
		`{"ts": {"lint": "eslint"}}`,
		`{"ts": {"syntax": "esbuild"}}`,
		`{"ts": {"types": "tsgo"}}`,
		`{"python": {"types": "pyre"}}`,
		`{"tier2": {"trigger": "sometimes"}}`,
		`{"tier2": {"sync": "ftp"}}`,
		`{"steps": [{"id": "x", "tier": 3, "cmd": ["true"]}]}`,
		`{"steps": [{"id": "x", "tier": 1}]}`,
		`{"steps": [{"id": "x", "tier": 1, "cmd": ["true"], "parser": "nope"}]}`,
		`{"steps": [{"id": "", "tier": 1, "cmd": ["true"]}]}`,
	} {
		if _, err := Load(writeCfg(t, body), ""); err == nil {
			t.Errorf("Load accepted %s", body)
		}
	}
}

func TestLoadExplicitPathMustExist(t *testing.T) {
	if _, err := Load(t.TempDir(), filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("Load ignored a --config path that does not exist")
	}
}

func TestBudgetAppliesOverridesToEveryInstanceOfAStep(t *testing.T) {
	root := writeCfg(t, `{"overrides": {"ts.types": {"mem_mb": 1536, "timeout_ms": 90000}, "go.test@svc/api": {"mem_mb": 2048}}}`)
	c, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id      string
		tier    int
		mem     int // 0 = sized automatically
		timeout time.Duration
	}{
		{"ts.lint", 0, 150, 10 * time.Second},
		{"py.types", 1, 0, 60 * time.Second},
		{"ts.types", 1, 1536, 90 * time.Second},
		{"ts.types@apps/web", 1, 1536, 90 * time.Second},   // per-project instance
		{"ts.types@apps/web#2", 1, 1536, 90 * time.Second}, // chunk of an instance
		{"go.test@svc/api", 1, 2048, 60 * time.Second},     // exact id wins, timeout untouched
		{"go.test@svc/other", 1, 0, 60 * time.Second},
	}
	for _, k := range cases {
		mem, timeout := c.Budget(k.id, k.tier)
		if mem != k.mem || timeout != k.timeout {
			t.Errorf("Budget(%q, %d) = (%d MB, %v), want (%d MB, %v)", k.id, k.tier, mem, timeout, k.mem, k.timeout)
		}
	}
}

func TestDisabledMatchesBaseIDs(t *testing.T) {
	c, err := Load(writeCfg(t, `{"disable": ["rs.test", "ts.lint@legacy"]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"rs.test": true, "rs.test@crates/a": true, "rs.check": false, "ts.lint@legacy": true, "ts.lint@apps/web": false, "ts.lint": false} {
		if got := c.Disabled(id); got != want {
			t.Errorf("Disabled(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestMinFreeScalesWithInstalledRAMUnlessSet(t *testing.T) {
	c, _ := Load(t.TempDir(), "")
	for total, want := range map[int]int{2048: 256, 8192: 512, 16384: 1024, 32768: 2048, 131072: 2048} {
		if got := c.MinFree(total); got != want {
			t.Errorf("MinFree(%d MB installed) = %d, want %d", total, got, want)
		}
	}
	c.MinFreeMB = 4000
	if got := c.MinFree(32768); got != 4000 {
		t.Errorf("MinFree with explicit min_free_mb=4000 returned %d", got)
	}
}

func TestDefaultJSONRoundTrips(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), DefaultJSON(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, ""); err != nil {
		t.Errorf("the config written by `ai-guard init` does not load: %v", err)
	}
}

func TestCustomStepCarriesItsOwnBudget(t *testing.T) {
	const step = `"steps": [{"id": "ts.typed-lint", "tier": 1, "cmd": ["x"], "mem_mb": 1024, "timeout_ms": 45000}]`
	c, err := Load(writeCfg(t, "{"+step+"}"), "")
	if err != nil {
		t.Fatal(err)
	}
	if mem, timeout := c.Budget("ts.typed-lint", 1); mem != 1024 || timeout != 45*time.Second {
		t.Errorf("Budget = (%d MB, %v), want the step's own 1024 MB and 45s", mem, timeout)
	}
	c, err = Load(writeCfg(t, `{`+step+`, "overrides": {"ts.typed-lint": {"mem_mb": 2048}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if mem, timeout := c.Budget("ts.typed-lint", 1); mem != 2048 || timeout != 45*time.Second {
		t.Errorf("Budget = (%d MB, %v), want the override's 2048 MB and the step's 45s", mem, timeout)
	}
}
