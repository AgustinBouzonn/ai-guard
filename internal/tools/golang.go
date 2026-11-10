package tools

import (
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

func (b *builder) golang(files []string) {
	gobin, haveGo := b.f.onPath("go")
	gofmt, haveFmt := b.f.onPath("gofmt")
	if !haveFmt && haveGo { // gofmt ships next to go even when only go is on PATH
		if p := filepath.Join(filepath.Dir(gobin), b.f.exe("gofmt")); exists(p) {
			gofmt, haveFmt = p, true
		}
	}
	if haveFmt {
		// gofmt -e reports every syntax error; -l keeps it from printing sources.
		b.fileSteps(Step{ID: "go.syntax", Tier: 0, Lang: lang.Go, Dir: b.root, Parser: diag.GoTool, Stream: Stderr}, []string{gofmt, "-l", "-e"}, files)
	} else {
		b.note(0, "go.syntax skipped: gofmt not found")
	}
	if !haveGo {
		b.note(1, "go.vet skipped: go not found")
		return
	}
	keys, groups := groupBy(files, func(f string) (string, bool) {
		dir, _, ok := lang.FindUp(b.root, dirOf(f), "go.mod")
		if !ok {
			b.note(1, "go.vet skipped: no go.mod above %s", f)
		}
		return dir, ok
	})
	for _, mod := range keys {
		seen := map[string]struct{}{}
		var pkgs []string
		for _, f := range relAll(mod, groups[mod]) {
			pkg := "."
			if d := path.Dir(f); d != "." {
				pkg = "./" + d
			}
			if _, dup := seen[pkg]; !dup {
				seen[pkg] = struct{}{}
				pkgs = append(pkgs, pkg)
			}
		}
		sort.Strings(pkgs)
		var env []string
		if n := b.env.Threads; n > 0 {
			env = []string{"GOFLAGS=-p=" + strconv.Itoa(n)} // bound parallel compiles and links
		}
		m := &goModule{dir: b.abs(mod), pkgs: pkgs, rdeps: b.cfg.Go.Rdeps}
		if b.cfg.Go.Vet != "off" {
			// vet type-checks the packages against export data: no code generation, no link.
			s := &Step{ID: stepID("go.vet", mod), Tier: 1, Phase: 0, Lang: lang.Go, Dir: m.dir, Env: env, Parser: diag.GoTool, Files: len(groups[mod])}
			s.Argv = append([]string{gobin, "vet"}, pkgs...)
			s.Prepare = func(run RunFunc) { m.apply(s, []string{gobin, "vet"}, gobin, run) }
			b.add(s)
		}
		if b.cfg.Go.Tests != "off" {
			// The test cache makes this "affected tests only": untouched packages report (cached).
			s := &Step{ID: stepID("go.test", mod), Tier: 1, Phase: 1, Lang: lang.Go, Dir: m.dir, Env: env, Parser: diag.GoTest, Files: len(groups[mod])}
			s.Argv = append([]string{gobin, "test", "-json", "-vet=off"}, pkgs...)
			limit := b.cfg.Go.MaxTestPackages
			s.Prepare = func(run RunFunc) {
				m.apply(s, []string{gobin, "test", "-json", "-vet=off"}, gobin, run)
				if n := len(m.targets); limit > 0 && n > limit { // every one of them links a test binary
					s.Defer = strconv.Itoa(n) + " affected packages > go.max_test_packages=" + strconv.Itoa(limit)
				}
			}
			b.add(s)
		}
	}
}

// goModule widens the package list of a module's steps to the packages that
// import a changed one (a signature change breaks its importers, not itself)
// and resolves import paths to directories for the test parser.
type goModule struct {
	dir   string
	pkgs  []string // changed package directories, "./x/y"
	rdeps bool

	once    sync.Once
	targets []string
	pkgDir  map[string]string
}

func (m *goModule) apply(s *Step, prefix []string, gobin string, run RunFunc) {
	m.once.Do(func() { m.load(gobin, run) })
	if m.pkgDir != nil {
		s.PkgDir = m.pkgDir
	}
	if len(m.targets) > 0 {
		s.Argv = append(append([]string{}, prefix...), m.targets...)
	}
}

func (m *goModule) load(gobin string, run RunFunc) {
	const format = `{{.ImportPath}}|{{.Dir}}|{{join .Imports ","}},{{join .TestImports ","}},{{join .XTestImports ","}}`
	out, err := run([]string{gobin, "list", "-e", "-f", format, "./..."}, m.dir)
	if err != nil && len(out) == 0 {
		return // keep the plain directory list
	}
	changedDir := map[string]struct{}{}
	for _, p := range m.pkgs {
		changedDir[filepath.Clean(filepath.Join(m.dir, filepath.FromSlash(p)))] = struct{}{}
	}
	type pkg struct {
		path    string
		imports []string
	}
	var all []pkg
	m.pkgDir = map[string]string{}
	changed := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(f) != 3 {
			continue
		}
		m.pkgDir[f[0]] = f[1]
		all = append(all, pkg{f[0], strings.Split(f[2], ",")})
		if _, ok := changedDir[filepath.Clean(f[1])]; ok {
			changed[f[0]] = struct{}{}
		}
	}
	if len(changed) == 0 {
		return
	}
	targets := map[string]struct{}{}
	for p := range changed {
		targets[p] = struct{}{}
	}
	if m.rdeps {
		for _, p := range all {
			for _, imp := range p.imports {
				if _, hit := changed[imp]; hit {
					targets[p.path] = struct{}{}
					break
				}
			}
		}
	}
	for p := range targets {
		m.targets = append(m.targets, p)
	}
	sort.Strings(m.targets)
}
