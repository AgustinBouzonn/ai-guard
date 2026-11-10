// Package config holds the orchestrator's tunables. Everything has a default:
// aiguard.json is optional and only needs the keys that differ.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
)

// FileName is looked up at the repository root.
const FileName = "aiguard.json"

// Tier is the resource envelope of one level.
type Tier struct {
	BudgetMs  int `json:"budget_ms"`  // target latency, reported when exceeded
	TimeoutMs int `json:"timeout_ms"` // per step; past it a tier-0 step is an error, a tier-1 step is deferred
	// MemMB is the RSS budget per step (whole process tree). In tier 1, 0 sizes
	// it from the machine: a cap of 1/8 of installed RAM (1024..4096 MB) and a
	// reservation learned from the step's previous runs.
	MemMB int `json:"mem_mb"`
}

// Report bounds what is sent back to the agent.
type Report struct {
	MaxErrors  int  `json:"max_errors"`
	MaxPerFile int  `json:"max_per_file"`
	MaxMsg     int  `json:"max_msg_chars"`
	Warnings   bool `json:"warnings"`
}

// TS selects the JS/TS tools ("auto" picks from config files and node_modules).
type TS struct {
	Lint string `json:"lint"` // auto | biome | oxlint | off
	// Syntax is the tier-0 fallback when no linter is installed: esbuild parses
	// the changed files. auto | off
	Syntax string `json:"syntax"`
	// Types: auto prefers a native compiler (the project's TypeScript 7 or
	// native preview, then the one from `ai-guard tools install`) over the
	// JavaScript one; native insists on it; tsc only uses the project's own.
	Types    string `json:"types"`    // auto | native | tsc | off
	Tests    string `json:"tests"`    // auto | vitest | jest | off
	Tsconfig string `json:"tsconfig"` // preferred tsconfig file name
	// TestArgs are extra runner flags, e.g. ["--project", "unit"]. Empty: the
	// --project/--config flags of the package's "test:unit" script, if any.
	TestArgs []string `json:"test_args"`
}

// Python selects the Python tools.
type Python struct {
	Lint            string `json:"lint"`  // auto | ruff | off
	Types           string `json:"types"` // auto | dmypy | mypy | ty | pyright | off
	Tests           string `json:"tests"` // auto | testmon | off
	DmypyIdleSec    int    `json:"dmypy_idle_sec"`
	DaemonMaxRSSMB  int    `json:"daemon_max_rss_mb"`
	PyrightAllFiles bool   `json:"pyright_whole_project"`
}

// Go selects the Go checks.
type Go struct {
	Vet   string `json:"vet"`   // auto | off
	Tests string `json:"tests"` // auto | off
	Rdeps bool   `json:"rdeps"` // also check packages that import the changed ones
	// MaxTestPackages caps how many packages' tests run locally; a wider blast
	// radius (an edit to a package everything imports) is deferred. 0 = no cap.
	MaxTestPackages int `json:"max_test_packages"`
}

// Rust selects the Rust checks.
type Rust struct {
	Check      string `json:"check"` // auto | off
	Tests      string `json:"tests"` // auto | off
	AllTargets bool   `json:"all_targets"`
}

// Cpp selects the C/C++ checks.
type Cpp struct {
	Syntax   string `json:"syntax"`   // auto | off
	Compiler string `json:"compiler"` // used when compile_commands.json has no entry
}

// CustomStep adds a project-specific command to a tier.
type CustomStep struct {
	ID        string   `json:"id"`
	Tier      int      `json:"tier"`
	Phase     *int     `json:"phase"` // tier 1 only: 0 = with static analysis, 1 = with tests (default)
	Langs     []string `json:"langs"` // run when files of these families changed; empty = always
	Cmd       []string `json:"cmd"`   // argv; "{files}" expands to the changed files
	Shell     string   `json:"shell"` // alternative to cmd: a shell command line
	Dir       string   `json:"dir"`   // working directory relative to the repo root
	Parser    string   `json:"parser"`
	MemMB     int      `json:"mem_mb"`
	TimeoutMs int      `json:"timeout_ms"`
}

// Override changes the envelope of one step.
type Override struct {
	MemMB     int `json:"mem_mb"`
	TimeoutMs int `json:"timeout_ms"`
}

// Tier2 configures remote offloading.
type Tier2 struct {
	Trigger   string   `json:"trigger"`    // off | manual | on_defer | on_pass | always
	Sync      string   `json:"sync"`       // git-ref | rsync | none
	Remote    string   `json:"remote"`     // git remote that receives the snapshot ref
	RefPrefix string   `json:"ref_prefix"` // refs/aiguard
	RsyncDest string   `json:"rsync_dest"` // user@host:/path
	RsyncArgs []string `json:"rsync_args"`
	Command   []string `json:"command"` // remote trigger; {id} {sha} {ref} {branch} {root}
	Shell     string   `json:"shell"`
	Status    []string `json:"status_command"`
}

// Config is the whole file.
type Config struct {
	Tier0         Tier                `json:"tier0"`
	Tier1         Tier                `json:"tier1"`
	MaxWorkers    int                 `json:"max_workers"`     // 0 = one per idle core, capped
	MinFreeMB     int                 `json:"min_free_mb"`     // RAM to leave untouched; 0 = scale with installed RAM
	QueueWaitMs   int                 `json:"queue_wait_ms"`   // how long a step may wait for RAM before it is deferred
	HardCapFactor float64             `json:"hard_cap_factor"` // OS cap = mem_mb x factor; 0 disables the OS cap
	LowPriority   bool                `json:"low_priority"`    // run tier 1 below normal priority
	RlimitASMB    int                 `json:"rlimit_as_mb"`    // Linux: RLIMIT_AS for tier 1 steps; 0 = off
	Wrap          []string            `json:"wrap"`            // prefix for tier 1 commands, e.g. systemd-run; {mem_mb} expands
	Report        Report              `json:"report"`
	TS            TS                  `json:"ts"`
	Python        Python              `json:"python"`
	Go            Go                  `json:"go"`
	Rust          Rust                `json:"rust"`
	Cpp           Cpp                 `json:"cpp"`
	Steps         []CustomStep        `json:"steps"`
	Enable        []string            `json:"enable"` // opt-in steps by id: ts.eslint
	Disable       []string            `json:"disable"`
	Overrides     map[string]Override `json:"overrides"`
	Tier2         Tier2               `json:"tier2"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Tier0:         Tier{BudgetMs: 300, TimeoutMs: 10000, MemMB: 150},
		Tier1:         Tier{BudgetMs: 3000, TimeoutMs: 60000, MemMB: 0},
		QueueWaitMs:   15000,
		HardCapFactor: 1.5,
		LowPriority:   true,
		Report:        Report{MaxErrors: 30, MaxPerFile: 6, MaxMsg: 240},
		TS:            TS{Lint: "auto", Syntax: "auto", Types: "auto", Tests: "auto", TestArgs: []string{}},
		Python:        Python{Lint: "auto", Types: "auto", Tests: "auto", DmypyIdleSec: 900, DaemonMaxRSSMB: 1024},
		Go:            Go{Vet: "auto", Tests: "auto", Rdeps: true, MaxTestPackages: 16},
		Rust:          Rust{Check: "auto", Tests: "auto"},
		Cpp:           Cpp{Syntax: "auto"},
		Steps:         []CustomStep{},
		Enable:        []string{},
		Disable:       []string{},
		Overrides:     map[string]Override{},
		Tier2:         Tier2{Trigger: "off", Sync: "git-ref", Remote: "origin", RefPrefix: "refs/aiguard"},
	}
}

// DefaultJSON is what `ai-guard init` writes.
func DefaultJSON() []byte {
	b, _ := json.MarshalIndent(Default(), "", "  ")
	return append(b, '\n')
}

// Load reads <root>/aiguard.json over the defaults. A missing default file is
// fine; a missing explicit path is an error.
func Load(root, explicit string) (*Config, error) {
	c := Default()
	path := explicit
	if path == "" {
		path = filepath.Join(root, FileName)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if explicit == "" && errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a typo must not silently become a default
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return c, nil
}

func oneOf(key, val string, allowed ...string) error {
	for _, a := range allowed {
		if val == a {
			return nil
		}
	}
	return fmt.Errorf("%s = %q, want one of %s", key, val, strings.Join(allowed, " | "))
}

func (c *Config) validate() error {
	checks := []error{
		oneOf("ts.lint", c.TS.Lint, "auto", "biome", "oxlint", "off"),
		oneOf("ts.syntax", c.TS.Syntax, "auto", "off"),
		oneOf("ts.types", c.TS.Types, "auto", "native", "tsc", "off"),
		oneOf("ts.tests", c.TS.Tests, "auto", "vitest", "jest", "off"),
		oneOf("python.lint", c.Python.Lint, "auto", "ruff", "off"),
		oneOf("python.types", c.Python.Types, "auto", "dmypy", "mypy", "ty", "pyright", "off"),
		oneOf("python.tests", c.Python.Tests, "auto", "testmon", "off"),
		oneOf("go.vet", c.Go.Vet, "auto", "off"),
		oneOf("go.tests", c.Go.Tests, "auto", "off"),
		oneOf("rust.check", c.Rust.Check, "auto", "off"),
		oneOf("rust.tests", c.Rust.Tests, "auto", "off"),
		oneOf("cpp.syntax", c.Cpp.Syntax, "auto", "off"),
		oneOf("tier2.trigger", c.Tier2.Trigger, "off", "manual", "on_defer", "on_pass", "always"),
		oneOf("tier2.sync", c.Tier2.Sync, "git-ref", "rsync", "none"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	for i, s := range c.Steps {
		switch {
		case s.ID == "":
			return fmt.Errorf("steps[%d]: id is required", i)
		case s.Tier != 0 && s.Tier != 1:
			return fmt.Errorf("steps[%d] %q: tier = %d, want 0 or 1", i, s.ID, s.Tier)
		case len(s.Cmd) == 0 && s.Shell == "":
			return fmt.Errorf("steps[%d] %q: cmd or shell is required", i, s.ID)
		case s.Phase != nil && *s.Phase != 0 && *s.Phase != 1:
			return fmt.Errorf("steps[%d] %q: phase = %d, want 0 or 1", i, s.ID, *s.Phase)
		}
		if s.Parser != "" {
			if _, ok := diag.ByName(s.Parser); !ok {
				return fmt.Errorf("steps[%d] %q: unknown parser %q", i, s.ID, s.Parser)
			}
		}
	}
	return nil
}

// idForms returns the id itself, the id without its "#chunk" suffix and the
// base id without "@project", most specific first.
func idForms(id string) []string {
	forms := []string{id}
	if i := strings.LastIndexByte(id, '#'); i >= 0 {
		id = id[:i]
		forms = append(forms, id)
	}
	if i := strings.IndexByte(id, '@'); i >= 0 {
		forms = append(forms, id[:i])
	}
	return forms
}

// Budget returns the memory budget and timeout of a step. memMB is 0 when
// nothing sets it: the caller then sizes it from the machine.
func (c *Config) Budget(id string, tier int) (memMB int, timeout time.Duration) {
	t := c.Tier0
	if tier >= 1 {
		t = c.Tier1
	}
	memMB, ms := t.MemMB, t.TimeoutMs
	for _, cs := range c.Steps { // a custom step states its own envelope
		if cs.ID == id {
			if cs.MemMB > 0 {
				memMB = cs.MemMB
			}
			if cs.TimeoutMs > 0 {
				ms = cs.TimeoutMs
			}
		}
	}
	forms := idForms(id)
	for i := len(forms) - 1; i >= 0; i-- { // general first, specific last
		if o, ok := c.Overrides[forms[i]]; ok {
			if o.MemMB > 0 {
				memMB = o.MemMB
			}
			if o.TimeoutMs > 0 {
				ms = o.TimeoutMs
			}
		}
	}
	return memMB, time.Duration(ms) * time.Millisecond
}

// Enabled reports whether an opt-in step is switched on.
func (c *Config) Enabled(id string) bool {
	for _, e := range c.Enable {
		if e == id {
			return true
		}
	}
	return false
}

// Disabled reports whether a step is switched off in the config.
func (c *Config) Disabled(id string) bool {
	for _, f := range idForms(id) {
		for _, d := range c.Disable {
			if d == f {
				return true
			}
		}
	}
	return false
}

// MinFree is the RAM (MiB) the scheduler never hands out: an explicit
// min_free_mb, or 1/16 of installed memory clamped to 256..2048.
func (c *Config) MinFree(totalMB int) int {
	if c.MinFreeMB > 0 {
		return c.MinFreeMB
	}
	switch v := totalMB / 16; {
	case v < 256:
		return 256
	case v > 2048:
		return 2048
	default:
		return v
	}
}
