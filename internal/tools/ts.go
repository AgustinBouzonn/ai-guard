package tools

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

func (b *builder) ts(files []string) {
	b.tsLint(files)
	b.tsTypes(files)
	b.tsESLint(files)
	b.tsTests(files)
}

// scriptWords returns the words of an npm script of the package in dir.
func (b *builder) scriptWords(dir, script string) []string {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(b.read(pathJoin(dir, "package.json"))), &pkg) != nil {
		return nil
	}
	return strings.Fields(pkg.Scripts[script])
}

// scriptFlags picks the given value-taking flags ("--name value" or
// "--name=value") out of an npm script that runs tool.
func scriptFlags(words []string, tool string, names ...string) []string {
	runs := false
	for _, w := range words {
		runs = runs || w == tool
	}
	if !runs {
		return nil
	}
	var out []string
	for i := 0; i < len(words); i++ {
		for _, n := range names {
			switch {
			case words[i] == n && i+1 < len(words):
				out = append(out, n, words[i+1])
				i++
			case strings.HasPrefix(words[i], n+"="):
				out = append(out, words[i])
			}
		}
	}
	return out
}

func (b *builder) biome(dir string) ([]string, bool) {
	plat := b.f.npmPlatform()
	for _, pkg := range []string{"@biomejs/cli-" + plat, "@biomejs/cli-" + plat + "-musl"} {
		if p, ok := b.f.nodeNative(dir, pkg, b.f.exe("biome")); ok {
			return []string{p}, true
		}
	}
	if p, ok := b.f.onPath("biome"); ok {
		return []string{p}, true
	}
	return nil, false
}

func (b *builder) oxlint(dir string) ([]string, bool) {
	if entry, ok := b.f.nodeFile(dir, "oxlint/bin/oxlint"); ok { // NAPI addon: no standalone binary
		if node, ok := b.f.onPath("node"); ok {
			return []string{node, entry}, true
		}
	}
	if p, ok := b.f.onPath("oxlint"); ok {
		return []string{p}, true
	}
	return nil, false
}

func (b *builder) tsLint(files []string) {
	want := b.cfg.TS.Lint
	if want == "off" {
		return
	}
	type group struct{ dir, cfg string }
	found := map[string]group{}
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, name, ok := lang.FindUp(b.root, dirOf(f), "biome.json", "biome.jsonc", ".oxlintrc.json")
		if !ok {
			dir, name = ".", ""
		}
		found[dir] = group{dir, name}
		return dir, true
	})
	for _, dir := range keys {
		g := found[dir]
		tool := want
		if tool == "auto" {
			switch {
			case strings.HasPrefix(g.cfg, "biome"):
				tool = "biome"
			case g.cfg == ".oxlintrc.json":
				tool = "oxlint"
			}
		}
		var bin []string
		var ok bool
		switch tool {
		case "biome":
			bin, ok = b.biome(dir)
		case "oxlint":
			bin, ok = b.oxlint(dir)
		default: // no config and no preference: whichever is installed
			if bin, ok = b.biome(dir); ok {
				tool = "biome"
			} else if bin, ok = b.oxlint(dir); ok {
				tool = "oxlint"
			}
		}
		if !ok {
			// Without a linter the changed files still get parsed. A linter the
			// project is set up for is reported missing either way.
			gated := b.tsSyntax(dir, groups[dir])
			if !gated || want != "auto" || g.cfg != "" {
				hint := "install one: npm i -D @biomejs/biome"
				if tool == "oxlint" {
					hint = "npm i -D oxlint"
				} else if tool == "biome" {
					hint = "npm i -D @biomejs/biome"
				}
				b.note(0, "ts.lint skipped: no linter found for %s (%s)", dir, hint)
			}
			continue
		}
		tmpl := Step{ID: stepID("ts.lint", dir), Tier: 0, Lang: lang.TS, Dir: b.abs(dir)}
		var prefix []string
		if tool == "biome" {
			prefix = append(bin, "lint", "--reporter=github", "--colors=off", "--no-errors-on-unmatched", "--files-ignore-unknown=true", "--max-diagnostics=200")
			if !b.cfg.Report.Warnings {
				prefix = append(prefix, "--diagnostic-level=error")
			}
			tmpl.Parser = diag.Biome
		} else {
			prefix = append(bin, "-f", "unix")
			if !b.cfg.Report.Warnings {
				prefix = append(prefix, "--quiet")
			}
			tmpl.Parser = diag.Oxlint
		}
		b.fileSteps(tmpl, prefix, relAll(dir, groups[dir]))
	}
}

func (b *builder) esbuild(dir string) (string, bool) {
	bin := "bin/esbuild"
	if b.env.GOOS == "windows" {
		bin = "esbuild.exe"
	}
	if p, ok := b.f.nodeNative(dir, "@esbuild/"+b.f.npmPlatform(), bin); ok {
		return p, true
	}
	return b.f.onPath("esbuild")
}

// tsSyntax parses the changed files with esbuild: the tier-0 gate of a
// project that has no fast linter. Nearly every Vite/Vitest/tsx project
// already ships the binary. It reports whether the gate was planned.
func (b *builder) tsSyntax(dir string, files []string) bool {
	if b.cfg.TS.Syntax == "off" {
		return false
	}
	bin, ok := b.esbuild(dir)
	if !ok {
		return false
	}
	// esbuild cannot parse without writing, and writes one .js per input: inputs
	// that differ only in their extension go to separate runs.
	var buckets [][]string
	var taken []map[string]bool
	for _, f := range relAll(dir, files) {
		key := strings.TrimSuffix(f, path.Ext(f))
		i := 0
		for i < len(buckets) && taken[i][key] {
			i++
		}
		if i == len(buckets) {
			buckets, taken = append(buckets, nil), append(taken, map[string]bool{})
		}
		buckets[i], taken[i][key] = append(buckets[i], f), true
	}
	var steps []*Step
	for i, bucket := range buckets {
		out := "esbuild-out"
		if dir != "." && dir != "" {
			out += "-" + slug(dir)
		}
		if i > 0 {
			out += "-" + strconv.Itoa(i+1)
		}
		// .js may hold JSX; --outbase keeps one output path per input.
		prefix := []string{bin, "--log-level=error", "--color=false", "--log-limit=50", "--loader:.js=jsx", "--outbase=.",
			"--outdir=" + filepath.Join(b.env.CacheDir, out)}
		for _, c := range b.chunks(prefix, bucket) {
			steps = append(steps, &Step{ID: stepID("ts.syntax", dir), Tier: 0, Lang: lang.TS, Dir: b.abs(dir),
				Argv: append(append([]string{}, prefix...), c...), Parser: diag.Esbuild, Files: len(c)})
		}
	}
	number(steps)
	for _, s := range steps {
		b.add(s)
	}
	return true
}

// tsc resolves the type checker. A native compiler comes first: the
// project's TypeScript 7, its native preview (tsgo), then the one installed
// by `ai-guard tools install`. ts.types = "tsc" keeps to the project's own
// typescript package; "native" refuses the JavaScript compiler.
func (b *builder) tsc(dir string) (argv []string, native, ok bool) {
	plat := b.f.npmPlatform()
	if p, ok := b.f.nodeNative(dir, "@typescript/typescript-"+plat, "lib/"+b.f.exe("tsc")); ok {
		return []string{p}, true, true
	}
	if b.cfg.TS.Types != "tsc" {
		preview, tsgo := "@typescript/native-preview-"+plat, "lib/"+b.f.exe("tsgo")
		if p, ok := b.f.nodeNative(dir, preview, tsgo); ok {
			return []string{p}, true, true
		}
		if p, ok := b.f.managed(preview, tsgo); ok {
			return []string{p}, true, true
		}
	}
	if b.cfg.TS.Types == "native" {
		return nil, false, false
	}
	if entry, ok := b.f.nodeFile(dir, "typescript/bin/tsc"); ok {
		if node, ok := b.f.onPath("node"); ok {
			return []string{node, entry}, false, true
		}
	}
	if p, ok := b.f.onPath("tsc"); ok {
		return []string{p}, false, true
	}
	return nil, false, false
}

func (b *builder) tsTypes(files []string) {
	if b.cfg.TS.Types == "off" {
		return
	}
	names := []string{"tsconfig.fast-check.json", "tsconfig.json"}
	if n := b.cfg.TS.Tsconfig; n != "" {
		names = []string{n}
	}
	type proj struct{ dir, name string }
	projs := map[string]proj{}
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, name, ok := lang.FindUp(b.root, dirOf(f), names...)
		if !ok {
			if ext := strings.ToLower(filepath.Ext(f)); strings.HasSuffix(ext, "ts") || ext == ".tsx" {
				b.note(1, "ts.types skipped: no %s above %s", names[len(names)-1], f)
			}
			return "", false
		}
		projs[dir] = proj{dir, name}
		return dir, true
	})
	for _, dir := range keys {
		p := projs[dir]
		bin, native, ok := b.tsc(dir)
		if !ok {
			if b.cfg.TS.Types == "native" {
				b.note(1, "ts.types skipped: no native TypeScript compiler for %s (run: ai-guard tools install)", dir)
			} else {
				b.note(1, "ts.types skipped: no TypeScript compiler found for %s (npm i -D typescript)", dir)
			}
			continue
		}
		argv := append(bin, "-p", p.name, "--noEmit", "--pretty", "false")
		if p.name != "tsconfig.fast-check.json" {
			// The project's own config: make the run incremental without touching it.
			info := filepath.Join(b.env.CacheDir, "tsc-"+slug(pathJoin(dir, p.name))+".tsbuildinfo")
			argv = append(argv, "--incremental", "--tsBuildInfoFile", info, "--skipLibCheck")
		}
		s := &Step{ID: stepID("ts.types", dir), Tier: 1, Phase: 0, Lang: lang.TS, Argv: argv, Dir: b.abs(dir), Parser: diag.TSC, Files: len(groups[dir])}
		if native {
			s.MemEnv = goMemLimit // the native compiler is a Go program
			if n := b.env.Threads; n > 0 {
				s.Env = append(s.Env, "GOMAXPROCS="+strconv.Itoa(n))
				if n < 4 { // default is 4 checkers; each one holds its own type state
					s.Argv = append(s.Argv, "--checkers", strconv.Itoa(n))
				}
			}
		} else {
			s.MemEnv = nodeHeap
		}
		b.add(s)
	}
}

// tsESLint lints the changed files with the project's own ESLint. It is
// opt-in ("enable": ["ts.eslint"]): a type-aware config builds a whole
// TypeScript program on every run, however few files it is given.
func (b *builder) tsESLint(files []string) {
	if !b.cfg.Enabled("ts.eslint") {
		return
	}
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, _, ok := lang.FindUp(b.root, dirOf(f), "package.json")
		if !ok {
			dir = "."
		}
		return dir, true
	})
	node, haveNode := b.f.onPath("node")
	for _, dir := range keys {
		entry, ok := b.f.nodeFile(dir, "eslint/bin/eslint.js")
		if !ok || !haveNode {
			b.note(1, "ts.eslint skipped: eslint (or node) not found for %s", dir)
			continue
		}
		prefix := []string{node, entry, "--format", "json"}
		// The nearest config decides the mode; the flag only exists for flat ones.
		if _, name, ok := lang.FindUp(b.root, dir, "eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", "eslint.config.mts", "eslint.config.cts",
			".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml", ".eslintrc"); ok && strings.HasPrefix(name, "eslint.config.") {
			prefix = append(prefix, "--no-warn-ignored")
		}
		// If warnings fail the project's lint script, they fail here too.
		prefix = append(prefix, scriptFlags(b.scriptWords(dir, "lint"), "eslint", "--max-warnings")...)
		tmpl := Step{ID: stepID("ts.eslint", dir), Tier: 1, Phase: 0, Lang: lang.TS, Dir: b.abs(dir), Parser: diag.ESLint, Stream: Stdout, MemEnv: nodeHeap}
		b.fileSteps(tmpl, prefix, relAll(dir, groups[dir]))
	}
}

func (b *builder) tsTests(files []string) {
	want := b.cfg.TS.Tests
	if want == "off" {
		return
	}
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, _, ok := lang.FindUp(b.root, dirOf(f), "package.json")
		if !ok {
			dir = "."
		}
		return dir, true
	})
	node, haveNode := b.f.onPath("node")
	for _, dir := range keys {
		vitest, hasVitest := b.f.nodeFile(dir, "vitest/vitest.mjs")
		jest, hasJest := b.f.nodeFile(dir, "jest/bin/jest.js")
		rel := relAll(dir, groups[dir])
		s := &Step{ID: stepID("ts.test", dir), Tier: 1, Phase: 1, Lang: lang.TS, Dir: b.abs(dir), MemEnv: nodeHeap, Files: len(rel)}
		// Each worker is a process with its own copy of the module graph: as
		// many as the cores this step may use, four at most, unless the project
		// says otherwise.
		n := b.env.Threads
		switch {
		case n <= 0:
			n = 2
		case n > 4:
			n = 4
		}
		extra, workers := b.cfg.TS.TestArgs, []string{"--maxWorkers=" + strconv.Itoa(n)}
		for _, a := range extra {
			if strings.HasPrefix(a, "--maxWorkers") {
				workers = nil
			}
		}
		switch {
		case !haveNode:
			if want != "auto" {
				b.note(1, "ts.test skipped: node is not on PATH")
			}
			continue
		case hasVitest && want != "jest":
			if len(extra) == 0 {
				// A "test:unit" script tells unit tests from the ones that need
				// services: keep its project and config selection.
				extra = scriptFlags(b.scriptWords(dir, "test:unit"), "vitest", "--project", "--config")
			}
			s.Argv = append([]string{node, vitest, "related", "--run", "--reporter=github-actions", "--passWithNoTests"}, workers...)
			s.Argv = append(append(s.Argv, extra...), rel...)
			s.Parser = diag.Vitest
		case hasJest && want != "vitest":
			s.Argv = append([]string{node, jest, "--json", "--testLocationInResults", "--silent", "--passWithNoTests"}, workers...)
			s.Argv = append(append(append(s.Argv, extra...), "--findRelatedTests"), rel...)
			s.Parser, s.Stream = diag.Jest, Stdout
		default:
			if want != "auto" {
				b.note(1, "ts.test skipped: %s is not installed in %s", want, dir)
			}
			continue
		}
		b.add(s)
	}
}

func pathJoin(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}
