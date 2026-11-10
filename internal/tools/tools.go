// Package tools plans which commands validate a set of changed files: it
// groups files by toolchain and project, resolves the fastest available
// binary for each check, and emits steps for tier 0 (syntax/lint on the
// changed files) and tier 1 (incremental type-checking, affected tests).
package tools

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

// Env describes the machine the plan is for. Tests substitute it.
type Env struct {
	GOOS, GOARCH string
	LookPath     func(name string) (string, error)
	CacheDir     string // absolute path of <repo>/.aiguard/cache
	VirtualEnv   string // $VIRTUAL_ENV, if a venv is active
	Threads      int    // threads each tier-1 tool may use; 0 = tool default
	// ToolsDir holds tools installed by `ai-guard tools install` (an npm
	// prefix); they stand in for what the project does not ship itself.
	ToolsDir string
}

// Stream selects which output of a tool carries its diagnostics.
type Stream int

const (
	Both Stream = iota
	Stdout
	Stderr
)

// Daemon describes the persistent server behind a client step.
type Daemon struct {
	StatusFile string   // JSON file with the server's "pid"
	MaxRSSMB   int      // restart the server once it grows past this
	Stop       []string // command that shuts it down
}

// RunFunc lets a step run a helper command during Prepare.
type RunFunc func(argv []string, dir string) (stdout []byte, err error)

// Step is one tool invocation.
type Step struct {
	ID    string
	Tier  int
	Phase int // within tier 1: 0 = static analysis, 1 = tests
	Lang  lang.Lang

	Argv  []string
	Shell string   // alternative to Argv for custom steps
	Dir   string   // absolute working directory
	Env   []string // extra KEY=VALUE pairs

	Parser diag.Parser
	Stream Stream
	OKExit []int // non-zero exit codes that are not failures

	Detached bool    // leaves a daemon behind: must not be contained
	Daemon   *Daemon // set for daemon clients
	// MemEnv returns environment that makes the tool itself respect the budget.
	MemEnv func(memMB int) []string
	// Prepare may refine Argv just before the step runs (needs to spawn). It can
	// also set Defer: the step is then not run locally at all.
	Prepare func(run RunFunc)
	Defer   string
	PkgDir  map[string]string // Go: import path -> directory, for the test parser

	Files int // changed files this step covers
}

// Note explains a check that could not be planned.
type Note struct {
	Tier int
	Text string
}

// Plan is the outcome of Build.
type Plan struct {
	Steps []*Step
	Notes []Note
}

type builder struct {
	root  string
	cfg   *config.Config
	env   Env
	f     *finder
	steps []*Step
	notes map[string]struct{}
	order []Note
}

// note records, once, why a check of the given tier is not in the plan.
func (b *builder) note(tier int, format string, args ...string) {
	s := format
	for _, a := range args {
		s = strings.Replace(s, "%s", a, 1)
	}
	if _, dup := b.notes[s]; !dup {
		b.notes[s] = struct{}{}
		b.order = append(b.order, Note{tier, s})
	}
}

func (b *builder) add(s *Step) { b.steps = append(b.steps, s) }

// Build plans every tier for the given changed files (repo-relative slash paths).
func Build(root string, files []string, cfg *config.Config, env Env) Plan {
	b := &builder{root: root, cfg: cfg, env: env, f: newFinder(root, env), notes: map[string]struct{}{}}
	byLang := map[lang.Lang][]string{}
	for _, f := range files {
		if l := lang.Of(f); l != lang.None {
			byLang[l] = append(byLang[l], f)
		}
	}
	for _, fs := range byLang {
		sort.Strings(fs)
	}
	if fs := byLang[lang.TS]; len(fs) > 0 {
		b.ts(fs)
	}
	if fs := byLang[lang.Py]; len(fs) > 0 {
		b.python(fs)
	}
	if fs := byLang[lang.Go]; len(fs) > 0 {
		b.golang(fs)
	}
	if fs := byLang[lang.Rust]; len(fs) > 0 {
		b.rust(fs)
	}
	if fs := byLang[lang.Cpp]; len(fs) > 0 {
		b.cpp(fs)
	}
	b.custom(files, byLang)

	var steps []*Step
	for _, s := range b.steps {
		if !cfg.Disabled(s.ID) {
			steps = append(steps, s)
		}
	}
	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].Tier != steps[j].Tier {
			return steps[i].Tier < steps[j].Tier
		}
		return steps[i].Phase < steps[j].Phase
	})
	return Plan{Steps: steps, Notes: b.order}
}

// ---- helpers ---------------------------------------------------------------

func stepID(base, dir string) string {
	if dir == "." || dir == "" {
		return base
	}
	return base + "@" + dir
}

// number gives sibling steps "#1..#n" suffixes; a single step keeps its id.
func number(steps []*Step) {
	if len(steps) < 2 {
		return
	}
	for i, s := range steps {
		s.ID += "#" + strconv.Itoa(i+1)
	}
}

func relTo(dir, file string) string {
	if dir == "." || dir == "" {
		return file
	}
	return strings.TrimPrefix(file, dir+"/")
}

func relAll(dir string, files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = relTo(dir, f)
	}
	return out
}

var reSlug = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string) string {
	if s == "." || s == "" {
		return "root"
	}
	return reSlug.ReplaceAllString(s, "_")
}

func (b *builder) abs(rel string) string {
	if rel == "." || rel == "" {
		return b.root
	}
	return filepath.Join(b.root, filepath.FromSlash(rel))
}

func (b *builder) read(rel string) string {
	data, _ := os.ReadFile(b.abs(rel))
	return string(data)
}

// argLimit is a conservative command-line budget: CreateProcess rejects more
// than 32767 characters, Linux allows far more.
func (b *builder) argLimit() int {
	if b.env.GOOS == "windows" {
		return 24000
	}
	return 120000
}

// chunks splits files so that prefix+files stays under the argument limit.
func (b *builder) chunks(prefix, files []string) [][]string {
	used := 0
	for _, p := range prefix {
		used += len(p) + 3
	}
	limit := b.argLimit()
	var out [][]string
	var cur []string
	n := used
	for _, f := range files {
		if len(cur) > 0 && n+len(f)+3 > limit {
			out = append(out, cur)
			cur, n = nil, used
		}
		cur = append(cur, f)
		n += len(f) + 3
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// fileSteps emits one step per chunk of files, sharing a template.
func (b *builder) fileSteps(tmpl Step, prefix, files []string) []*Step {
	var steps []*Step
	for _, c := range b.chunks(prefix, files) {
		s := tmpl
		s.Argv = append(append([]string{}, prefix...), c...)
		s.Files = len(c)
		steps = append(steps, &s)
	}
	number(steps)
	for _, s := range steps {
		b.add(s)
	}
	return steps
}

// groupBy buckets files by key, returning keys in sorted order.
func groupBy(files []string, key func(string) (string, bool)) (keys []string, groups map[string][]string) {
	groups = map[string][]string{}
	for _, f := range files {
		k, ok := key(f)
		if !ok {
			continue
		}
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], f)
	}
	sort.Strings(keys)
	return keys, groups
}

func nodeHeap(memMB int) []string {
	return []string{"NODE_OPTIONS=--max-old-space-size=" + strconv.Itoa(memMB*3/4)}
}

func goMemLimit(memMB int) []string {
	return []string{"GOMEMLIMIT=" + strconv.Itoa(memMB*9/10) + "MiB"}
}

func dirOf(file string) string { return path.Dir(file) }

// ---- tool resolution -------------------------------------------------------

type finder struct {
	root string
	env  Env
	memo map[string]string
}

func newFinder(root string, env Env) *finder {
	return &finder{root: root, env: env, memo: map[string]string{}}
}

// onPath is a memoised PATH lookup; misses are expensive on Windows.
func (f *finder) onPath(name string) (string, bool) {
	if p, ok := f.memo[name]; ok {
		return p, p != ""
	}
	p, err := f.env.LookPath(name)
	if err != nil {
		p = ""
	}
	f.memo[name] = p
	return p, p != ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (f *finder) exe(name string) string {
	if f.env.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// up calls probe for startRel and each parent up to the repository root.
func (f *finder) up(startRel string, probe func(dirAbs string) (string, bool)) (string, bool) {
	dir := path.Clean(startRel)
	for {
		abs := f.root
		if dir != "." {
			abs = filepath.Join(f.root, filepath.FromSlash(dir))
		}
		if p, ok := probe(abs); ok {
			return p, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// nodeFile finds node_modules/<rel> at or above startRel.
func (f *finder) nodeFile(startRel, rel string) (string, bool) {
	return f.up(startRel, func(dir string) (string, bool) {
		p := filepath.Join(dir, "node_modules", filepath.FromSlash(rel))
		return p, exists(p)
	})
}

// nodeNative finds the native executable shipped in a platform package,
// bypassing the JS launcher in node_modules/.bin (tens of ms and tens of MB
// per call). pnpm keeps platform packages only in its virtual store.
func (f *finder) nodeNative(startRel, pkg, bin string) (string, bool) {
	return f.up(startRel, func(dir string) (string, bool) {
		nm := filepath.Join(dir, "node_modules")
		p := filepath.Join(nm, filepath.FromSlash(pkg), filepath.FromSlash(bin))
		if exists(p) {
			return p, true
		}
		pat := filepath.Join(nm, ".pnpm", strings.ReplaceAll(pkg, "/", "+")+"@*", "node_modules", filepath.FromSlash(pkg), filepath.FromSlash(bin))
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			sort.Slice(m, func(i, j int) bool { return versionLess(m[i], m[j]) })
			return m[len(m)-1], true
		}
		return "", false
	})
}

// managed finds a native executable in the ai-guard tools directory.
func (f *finder) managed(pkg, bin string) (string, bool) {
	if f.env.ToolsDir == "" {
		return "", false
	}
	p := filepath.Join(f.env.ToolsDir, "node_modules", filepath.FromSlash(pkg), filepath.FromSlash(bin))
	return p, exists(p)
}

// ManagedTSC returns the native TypeScript compiler of the tools directory.
func ManagedTSC(env Env) (string, bool) {
	f := newFinder("", env)
	return f.managed("@typescript/native-preview-"+f.npmPlatform(), "lib/"+f.exe("tsgo"))
}

// versionLess orders paths that differ in an embedded version: runs of digits
// compare as numbers, so 0.9.7 sorts before 0.28.2.
func versionLess(a, b string) bool {
	for a != "" && b != "" {
		na, nb := digits(a), digits(b)
		if na > 0 && nb > 0 {
			x, y := strings.TrimLeft(a[:na], "0"), strings.TrimLeft(b[:nb], "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
			if x != y {
				return x < y
			}
			a, b = a[na:], b[nb:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

func (f *finder) npmPlatform() string {
	os, arch := f.env.GOOS, f.env.GOARCH
	if os == "windows" {
		os = "win32"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	return os + "-" + arch
}

// venvBin finds an executable in a virtualenv at or above startRel.
func (f *finder) venvBin(startRel, name string) (bin, venv string, ok bool) {
	sub := "bin"
	if f.env.GOOS == "windows" {
		sub = "Scripts"
	}
	if v := f.env.VirtualEnv; v != "" {
		if p := filepath.Join(v, sub, f.exe(name)); exists(p) {
			return p, v, true
		}
	}
	bin, ok = f.up(startRel, func(dir string) (string, bool) {
		for _, v := range []string{".venv", "venv"} {
			if p := filepath.Join(dir, v, sub, f.exe(name)); exists(p) {
				venv = filepath.Join(dir, v)
				return p, true
			}
		}
		return "", false
	})
	return bin, venv, ok
}

// pyTool prefers the project's virtualenv, then PATH.
func (f *finder) pyTool(startRel, name string) (bin, venv string, ok bool) {
	if bin, venv, ok = f.venvBin(startRel, name); ok {
		return bin, venv, true
	}
	bin, ok = f.onPath(name)
	return bin, "", ok
}
