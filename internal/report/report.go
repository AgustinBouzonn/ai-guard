// Package report renders a check result for agents (compact text) and for
// machines (one-line JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
)

// Status is the overall verdict of a run.
type Status string

const (
	Pass     Status = "pass"
	Fail     Status = "fail"     // the code has errors
	Infra    Status = "error"    // a check could not complete (tool crash, timeout, kill)
	Deferred Status = "deferred" // nothing failed locally, but some checks did not run here
)

// Tier is the wall time spent in one level against its target.
type Tier struct {
	N        int
	Ms       int64
	BudgetMs int64
}

// Step is the outcome of one tool invocation.
type Step struct {
	ID     string
	Tier   int
	Ms     int64
	PeakMB int
	Status string // ok | fail | error | killed:mem | killed:timeout | cancelled | deferred
	Note   string
}

// Tier2 describes a queued remote job.
type Tier2 struct{ ID, State, Log string }

// Result is everything a run produced.
type Result struct {
	Status   Status
	Files    int
	PeakMB   int // largest tree any single step reached
	Tiers    []Tier
	Diags    []diag.D
	Steps    []Step
	Notes    []string // skipped tools and infrastructure problems
	Deferred []string // checks handed to tier 2 or skipped for lack of RAM
	Tier2    *Tier2
}

// Options bounds the size of the report.
type Options struct {
	MaxErrors  int // total diagnostics printed
	MaxPerFile int
	MaxMsg     int // characters per message
	Warnings   bool
}

// ExitCode maps the verdict to the process exit status.
func (r Result) ExitCode() int {
	switch r.Status {
	case Pass:
		return 0
	case Fail:
		return 1
	case Deferred:
		return 4
	}
	return 3
}

// clean flattens a message to one line and cuts it to max characters.
func clean(msg string, max int) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); max > 0 && len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return msg
}

// prepare de-duplicates and orders diagnostics: no-file first, then by path
// and position, so repeated runs print identical reports.
func prepare(ds []diag.D, warnings bool) (shown []diag.D, errs, warns int) {
	seen := map[diag.D]struct{}{}
	for _, d := range ds {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		if d.Sev == diag.Warning {
			warns++
			if !warnings {
				continue
			}
		} else {
			errs++
		}
		shown = append(shown, d)
	}
	sort.SliceStable(shown, func(i, j int) bool {
		a, b := shown[i], shown[j]
		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Col != b.Col:
			return a.Col < b.Col
		case a.Rule != b.Rule:
			return a.Rule < b.Rule
		}
		return a.Msg < b.Msg
	})
	return shown, errs, warns
}

func word(s Status) string {
	switch s {
	case Pass:
		return "OK"
	case Fail:
		return "FAIL"
	case Deferred:
		return "DEFERRED"
	}
	return "ERROR"
}

func entry(d diag.D, max int) string {
	var b strings.Builder
	switch {
	case d.Line > 0 && d.Col > 0:
		fmt.Fprintf(&b, "%d:%d ", d.Line, d.Col)
	case d.Line > 0:
		fmt.Fprintf(&b, "%d ", d.Line)
	default:
		b.WriteString("- ")
	}
	if d.Sev == diag.Warning {
		b.WriteString("warn ")
	}
	if d.Rule != "" {
		b.WriteString(d.Rule + " ")
	}
	b.WriteString(clean(d.Msg, max))
	return b.String()
}

// Agent writes the compact text form: one status line, then diagnostics
// grouped under their file so each path is paid for once.
func Agent(w io.Writer, r Result, o Options) {
	shown, errs, warns := prepare(r.Diags, o.Warnings)
	if r.Status == Pass && r.Files == 0 && len(r.Tiers) == 0 {
		fmt.Fprintln(w, "OK no changed source files")
		return
	}
	var body strings.Builder
	printed, perFile, hiddenInFile, hiddenFiles := 0, 0, 0, 0
	cur, started := "", false
	flush := func() {
		if hiddenInFile > 0 {
			fmt.Fprintf(&body, "  +%d more\n", hiddenInFile)
		}
		hiddenInFile = 0
	}
	skipping := false
	for _, d := range shown {
		if d.File == "" {
			if o.MaxErrors <= 0 || printed < o.MaxErrors {
				fmt.Fprintf(&body, "%s\n", entry(d, o.MaxMsg))
				printed++
			}
			continue
		}
		if !started || d.File != cur {
			flush()
			cur, started, perFile = d.File, true, 0
			skipping = o.MaxErrors > 0 && printed >= o.MaxErrors
			if skipping {
				hiddenFiles++
			} else {
				fmt.Fprintf(&body, "%s\n", d.File)
			}
		}
		if skipping {
			continue
		}
		if (o.MaxPerFile > 0 && perFile >= o.MaxPerFile) || (o.MaxErrors > 0 && printed >= o.MaxErrors) {
			hiddenInFile++
			continue
		}
		fmt.Fprintf(&body, "  %s\n", entry(d, o.MaxMsg))
		printed++
		perFile++
	}
	flush()
	if hiddenFiles == 1 {
		body.WriteString("+1 more file\n")
	} else if hiddenFiles > 1 {
		fmt.Fprintf(&body, "+%d more files\n", hiddenFiles)
	}

	var head strings.Builder
	head.WriteString(word(r.Status))
	for _, t := range r.Tiers {
		fmt.Fprintf(&head, " t%d %dms", t.N, t.Ms)
	}
	fmt.Fprintf(&head, " files=%d", r.Files)
	if r.PeakMB > 0 {
		fmt.Fprintf(&head, " peak=%dMB", r.PeakMB)
	}
	if errs > 0 {
		fmt.Fprintf(&head, " errors=%d", errs)
		if printed < len(shown) {
			fmt.Fprintf(&head, " (showing %d)", printed)
		}
	}
	if o.Warnings && warns > 0 {
		fmt.Fprintf(&head, " warnings=%d", warns)
	}
	fmt.Fprintln(w, head.String())
	io.WriteString(w, body.String())
	for _, d := range r.Deferred {
		fmt.Fprintf(w, "> deferred %s\n", d)
	}
	if r.Tier2 != nil {
		fmt.Fprintf(w, "> tier2 %s id=%s log=%s\n", r.Tier2.State, r.Tier2.ID, r.Tier2.Log)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "! %s\n", n)
	}
}

type jsonDiag struct {
	F string `json:"f,omitempty"`
	L int    `json:"l,omitempty"`
	C int    `json:"c,omitempty"`
	R string `json:"r,omitempty"`
	M string `json:"m"`
	W bool   `json:"w,omitempty"`
}

type jsonTier struct {
	Tier       int   `json:"tier"`
	Ms         int64 `json:"ms"`
	BudgetMs   int64 `json:"budget_ms"`
	OverBudget bool  `json:"over_budget"`
}

type jsonStep struct {
	ID     string `json:"id"`
	Tier   int    `json:"tier"`
	Ms     int64  `json:"ms"`
	MB     int    `json:"mb"`
	Status string `json:"st"`
	Note   string `json:"note,omitempty"`
}

type jsonTier2 struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Log   string `json:"log"`
}

type jsonResult struct {
	Status    Status     `json:"status"`
	OK        bool       `json:"ok"`
	Files     int        `json:"files"`
	PeakMB    int        `json:"peak_mb"`
	Tiers     []jsonTier `json:"tiers"`
	Errors    int        `json:"errors"`
	Warnings  int        `json:"warnings"`
	Diags     []jsonDiag `json:"diags"`
	Truncated int        `json:"truncated,omitempty"`
	Steps     []jsonStep `json:"steps"`
	Notes     []string   `json:"notes,omitempty"`
	Deferred  []string   `json:"deferred,omitempty"`
	Tier2     *jsonTier2 `json:"t2,omitempty"`
}

// JSON writes the result as a single line of JSON with short keys.
func JSON(w io.Writer, r Result, o Options) {
	shown, errs, warns := prepare(r.Diags, o.Warnings)
	out := jsonResult{Status: r.Status, OK: r.Status == Pass, Files: r.Files, PeakMB: r.PeakMB, Errors: errs, Warnings: warns,
		Tiers: []jsonTier{}, Diags: []jsonDiag{}, Steps: []jsonStep{}, Notes: r.Notes, Deferred: r.Deferred}
	for _, t := range r.Tiers {
		out.Tiers = append(out.Tiers, jsonTier{t.N, t.Ms, t.BudgetMs, t.BudgetMs > 0 && t.Ms > t.BudgetMs})
	}
	for i, d := range shown {
		if o.MaxErrors > 0 && i >= o.MaxErrors {
			out.Truncated = len(shown) - i
			break
		}
		out.Diags = append(out.Diags, jsonDiag{d.File, d.Line, d.Col, d.Rule, clean(d.Msg, o.MaxMsg), d.Sev == diag.Warning})
	}
	for _, s := range r.Steps {
		out.Steps = append(out.Steps, jsonStep{s.ID, s.Tier, s.Ms, s.PeakMB, s.Status, s.Note})
	}
	if r.Tier2 != nil {
		out.Tier2 = &jsonTier2{r.Tier2.ID, r.Tier2.State, r.Tier2.Log}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(out)
}
