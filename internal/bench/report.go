package bench

import (
	"fmt"
	"io"
	"math"
)

func verdict(pass bool) string {
	if pass {
		return "pass"
	}
	return "FAIL"
}

func round(x float64) int { return int(math.Round(x)) }

// Markdown renders the comparison table.
func Markdown(w io.Writer, r Results) {
	fmt.Fprintf(w, "# AI-Guard benchmark: %s\n\n", r.Repo)
	fmt.Fprintf(w, "%s · %s · %d measured run(s) per patch after %d warm-up · medians\n\n", r.When.Format("2006-01-02 15:04 UTC"), r.Machine, r.Reps, r.Warmup)
	fmt.Fprintln(w, "| Patch | File | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB | RAM saved | Baseline CPU | AI-Guard CPU | Verdict (baseline / AI-Guard) |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|:---:|")

	type group struct {
		n                 int
		baseMs, optMs     float64
		basePeak, optPeak float64
		logSpeedup        float64
	}
	groups := map[string]*group{}
	var order []string
	all := &group{}
	for _, row := range r.Rows {
		bw, ow, bp, op := walls(row.BaseRuns), walls(row.OptRuns), peaks(row.BaseRuns), peaks(row.OptRuns)
		v := verdict(row.BasePass) + " / "
		switch {
		case row.OptPass && row.OptDeferred:
			v += "defer"
		case row.BasePass != row.OptPass:
			v += verdict(row.OptPass) + " ⚠"
		default:
			v += verdict(row.OptPass)
		}
		fmt.Fprintf(w, "| %s | %s | %d | %d | %.1fx | %d | %d | %.1f%% | %d%% | %d%% | %s |\n",
			row.Patch.Name, row.Patch.File, round(bw), round(ow), row.Speedup(), round(bp), round(op), row.RAMSaved(),
			round(cpuPct(row.BaseRuns, r.Cores)), round(cpuPct(row.OptRuns, r.Cores)), v)
		g := groups[row.Patch.Lang]
		if g == nil {
			g = &group{}
			groups[row.Patch.Lang] = g
			order = append(order, row.Patch.Lang)
		}
		for _, x := range []*group{g, all} {
			x.n++
			x.baseMs += bw
			x.optMs += ow
			x.basePeak, x.optPeak = math.Max(x.basePeak, bp), math.Max(x.optPeak, op)
			x.logSpeedup += math.Log(row.Speedup())
		}
	}
	line := func(name string, g *group) {
		if g.n == 0 {
			return
		}
		noun := "patches"
		if g.n == 1 {
			noun = "patch"
		}
		fmt.Fprintf(w, "| **%s** (%d %s) | | %d | %d | **%.1fx** | %d | %d | **%.1f%%** | | | |\n",
			name, g.n, noun, round(g.baseMs), round(g.optMs), math.Exp(g.logSpeedup/float64(g.n)), round(g.basePeak), round(g.optPeak), saved(g.basePeak, g.optPeak))
	}
	for _, lang := range order {
		line(lang, groups[lang])
	}
	line("all", all)

	fmt.Fprint(w, "\nFirst run of each patch (cold caches, not included above):\n\n")
	fmt.Fprintln(w, "| Patch | Baseline ms | AI-Guard ms | Speedup | Baseline RSS MB | AI-Guard RSS MB |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|")
	for _, row := range r.Rows {
		bw, ow := math.Max(float64(row.BaseFirst.WallMs), 1), math.Max(float64(row.OptFirst.WallMs), 1)
		fmt.Fprintf(w, "| %s | %d | %d | %.1fx | %d | %d |\n", row.Patch.Name, round(bw), round(ow), bw/ow, row.BaseFirst.PeakMB, row.OptFirst.PeakMB)
	}
	fmt.Fprintf(w, `
- ms: wall-clock until the pipeline returns its verdict. Group rows add the patch medians; their speedup is the geometric mean of the patch speedups.
- RSS: peak resident memory of the whole process tree (for AI-Guard, plus any persistent type-check daemon). Group rows show the highest peak.
- CPU: CPU-seconds divided by wall-clock and by the %d logical cores (100%% = every core busy for the whole run).
- Verdict: whether each pipeline accepted the patch; ⚠ marks patches on which they disagree; "defer" means AI-Guard found no error but handed part of the work to the remote tier instead of running it locally.
`, r.Cores)
}
