package tools

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

var pyMarkers = []string{"pyproject.toml", "ruff.toml", ".ruff.toml", "mypy.ini", "setup.cfg", "setup.py", "pyrightconfig.json", "requirements.txt"}

func (b *builder) python(files []string) {
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, _, ok := lang.FindUp(b.root, dirOf(f), pyMarkers...)
		if !ok {
			dir = "."
		}
		return dir, true
	})
	for _, dir := range keys {
		fs := groups[dir]
		b.pyLint(dir, fs)
		b.pyTypes(dir, fs)
		b.pyTests(dir, fs)
	}
}

func (b *builder) pyLint(dir string, files []string) {
	if b.cfg.Python.Lint == "off" {
		return
	}
	ruff, _, ok := b.f.pyTool(dir, "ruff")
	if !ok {
		b.note(0, "py.lint skipped: ruff not found for %s (uv add --dev ruff)", dir)
		return
	}
	prefix := []string{ruff, "check", "--output-format=concise", "--no-fix", "--force-exclude", "--quiet"}
	env := []string{"RUFF_CACHE_DIR=" + filepath.Join(b.env.CacheDir, "ruff")}
	b.fileSteps(Step{ID: stepID("py.lint", dir), Tier: 0, Lang: lang.Py, Dir: b.abs(dir), Env: env, Parser: diag.Ruff}, prefix, relAll(dir, files))
}

// tomlSection returns the body of [name] in a TOML/INI file.
func tomlSection(text, name string) (string, bool) {
	i := strings.Index(text, "["+name+"]")
	if i < 0 {
		return "", false
	}
	body := text[i+len(name)+2:]
	if j := regexp.MustCompile(`(?m)^\[`).FindStringIndex(body); j != nil {
		body = body[:j[0]]
	}
	return body, true
}

var reMypyTargets = regexp.MustCompile(`(?m)^\s*(files|packages|modules)\s*=`)

func (b *builder) pyTypes(dir string, files []string) {
	want := b.cfg.Python.Types
	if want == "off" {
		return
	}
	pyproject := b.read(pathJoin(dir, "pyproject.toml"))
	mypyCfg, hasMypyCfg := tomlSection(pyproject, "tool.mypy")
	for _, f := range []string{"mypy.ini", ".mypy.ini"} {
		if text := b.read(pathJoin(dir, f)); text != "" {
			mypyCfg, hasMypyCfg = text, true
		}
	}
	if sec, ok := tomlSection(b.read(pathJoin(dir, "setup.cfg")), "mypy"); ok {
		mypyCfg, hasMypyCfg = sec, true
	}
	_, hasTyCfg := tomlSection(pyproject, "tool.ty")
	hasTyCfg = hasTyCfg || exists(b.abs(pathJoin(dir, "ty.toml")))
	_, hasPyrightCfg := tomlSection(pyproject, "tool.pyright")
	hasPyrightCfg = hasPyrightCfg || exists(b.abs(pathJoin(dir, "pyrightconfig.json")))

	has := func(name string) bool { _, _, ok := b.f.pyTool(dir, name); return ok }
	tool := want
	if tool == "auto" {
		switch {
		case hasMypyCfg && has("dmypy"):
			tool = "dmypy"
		case hasTyCfg && has("ty"):
			tool = "ty"
		case hasPyrightCfg && has("pyright"):
			tool = "pyright"
		case has("ty"):
			tool = "ty"
		case has("dmypy"):
			tool = "dmypy"
		case has("pyright"):
			tool = "pyright"
		default:
			b.note(1, "py.types skipped: no type checker found for %s (uv add --dev mypy | ty | pyright)", dir)
			return
		}
	}
	bin, _, ok := b.f.pyTool(dir, tool)
	if !ok {
		b.note(1, "py.types skipped: %s not found for %s", tool, dir)
		return
	}
	s := &Step{ID: stepID("py.types", dir), Tier: 1, Phase: 0, Lang: lang.Py, Dir: b.abs(dir), Files: len(files)}
	mypyFlags := []string{"--show-column-numbers", "--no-error-summary", "--hide-error-context", "--no-pretty", "--no-color-output",
		"--cache-dir", filepath.Join(b.env.CacheDir, "mypy-"+slug(dir))}
	targets := []string{"."}
	if reMypyTargets.MatchString(mypyCfg) { // the config already says what to check
		targets = nil
	}
	switch tool {
	case "dmypy":
		// The server keeps the whole program in memory and re-checks only what a
		// change invalidates; it exits by itself after the idle timeout.
		status := filepath.Join(b.env.CacheDir, "dmypy-"+slug(dir)+".json")
		s.Argv = append([]string{bin, "--status-file", status, "run", "--timeout", strconv.Itoa(b.cfg.Python.DmypyIdleSec), "--"}, append(mypyFlags, targets...)...)
		s.Parser, s.Detached = diag.GCC, true
		s.Daemon = &Daemon{StatusFile: status, MaxRSSMB: b.cfg.Python.DaemonMaxRSSMB, Stop: []string{bin, "--status-file", status, "stop"}}
	case "mypy":
		s.Argv = append(append([]string{bin}, mypyFlags...), targets...)
		s.Parser = diag.GCC
	case "ty":
		s.Argv = []string{bin, "check", "--output-format", "concise", "--color", "never", "--no-progress"}
		s.Parser = diag.GCC
		if n := b.env.Threads; n > 0 {
			s.Env = append(s.Env, "TY_MAX_PARALLELISM="+strconv.Itoa(n), "RAYON_NUM_THREADS="+strconv.Itoa(n))
		}
	case "pyright":
		s.Argv = []string{bin, "--outputjson"}
		if !b.cfg.Python.PyrightAllFiles {
			s.Argv = append(s.Argv, relAll(dir, files)...)
		}
		s.Parser, s.Stream, s.MemEnv = diag.Pyright, Stdout, nodeHeap
	}
	b.add(s)
}

func isPyTest(file string) bool {
	base := path.Base(file)
	return strings.HasSuffix(base, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py"))
}

func (b *builder) hasTestmon(venv string) bool {
	if venv == "" {
		return false
	}
	if exists(filepath.Join(venv, "Lib", "site-packages", "testmon")) { // Windows layout
		return true
	}
	m, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages", "testmon"))
	return len(m) > 0
}

func (b *builder) pyTests(dir string, files []string) {
	want := b.cfg.Python.Tests
	if want == "off" {
		return
	}
	pytest, venv, ok := b.f.pyTool(dir, "pytest")
	if !ok {
		return // a project without pytest has nothing to select
	}
	s := &Step{ID: stepID("py.test", dir), Tier: 1, Phase: 1, Lang: lang.Py, Dir: b.abs(dir), Parser: diag.Pytest, OKExit: []int{5}, Files: len(files)}
	base := []string{pytest, "-q", "--no-header", "--color=no", "--tb=line", "-rfE"}
	if want == "testmon" || b.hasTestmon(venv) {
		s.Argv = append(base, "--testmon") // coverage-based selection of affected tests
		s.Env = []string{"TESTMON_DATAFILE=" + filepath.Join(b.env.CacheDir, "testmon-"+slug(dir))}
		b.add(s)
		return
	}
	// No dependency data: fall back to naming conventions.
	seen := map[string]struct{}{}
	var tests []string
	addIf := func(rel string) {
		if _, dup := seen[rel]; !dup && exists(b.abs(pathJoin(dir, rel))) {
			seen[rel] = struct{}{}
			tests = append(tests, rel)
		}
	}
	for _, f := range relAll(dir, files) {
		if isPyTest(f) {
			addIf(f)
			continue
		}
		name := "test_" + path.Base(f)
		fdir := path.Dir(f)
		for _, cand := range []string{"tests/" + name, "test/" + name, path.Join(fdir, name), path.Join(fdir, "tests", name)} {
			addIf(cand)
		}
	}
	if len(tests) == 0 {
		b.note(1, "py.test skipped: pytest-testmon is not installed and no test file matches the changed modules in %s (uv add --dev pytest-testmon)", dir)
		return
	}
	sort.Strings(tests)
	s.Argv = append(base, tests...)
	b.add(s)
}
