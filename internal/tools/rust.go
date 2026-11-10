package tools

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

var (
	reEdition  = regexp.MustCompile(`(?m)^\s*edition\s*=\s*"(\d{4})"`)
	reCrate    = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)
	reLibTable = regexp.MustCompile(`(?m)^\[lib\]`)
)

// workspaceRoot returns the outermost Cargo.toml directory above pkgDir that
// declares [workspace]; a lone package is its own root.
func (b *builder) workspaceRoot(pkgDir string) (dir string, isWorkspace bool) {
	dir = pkgDir
	for cur := pkgDir; ; {
		if strings.Contains(b.read(pathJoin(cur, "Cargo.toml")), "[workspace]") {
			dir, isWorkspace = cur, true
		}
		if cur == "." {
			return dir, isWorkspace
		}
		next, _, ok := lang.FindUp(b.root, path.Dir(cur), "Cargo.toml")
		if !ok {
			return dir, isWorkspace
		}
		cur = next
	}
}

func (b *builder) rust(files []string) {
	type crate struct {
		dir, name, edition, ws string
		isWS                   bool
		files                  []string
	}
	crates := map[string]*crate{}
	var crateDirs []string
	for _, f := range files {
		dir, _, ok := lang.FindUp(b.root, dirOf(f), "Cargo.toml")
		if !ok {
			b.note(1, "rs.check skipped: no Cargo.toml above %s", f)
			continue
		}
		c := crates[dir]
		if c == nil {
			manifest := b.read(pathJoin(dir, "Cargo.toml"))
			c = &crate{dir: dir, edition: "2021"}
			c.ws, c.isWS = b.workspaceRoot(dir)
			if pkg, ok := tomlSection(manifest, "package"); ok {
				if m := reCrate.FindStringSubmatch(pkg); m != nil {
					c.name = m[1]
				}
				if m := reEdition.FindStringSubmatch(pkg); m != nil {
					c.edition = m[1]
				} else if wp, ok := tomlSection(b.read(pathJoin(c.ws, "Cargo.toml")), "workspace.package"); ok {
					if m := reEdition.FindStringSubmatch(wp); m != nil { // edition.workspace = true
						c.edition = m[1]
					}
				}
			}
			crates[dir] = c
			crateDirs = append(crateDirs, dir)
		}
		c.files = append(c.files, f)
	}
	sort.Strings(crateDirs)

	// Tier 0: rustfmt parses each file (and nothing else) and fails on syntax
	// errors. It must know the edition: 2024 syntax does not parse as 2021.
	if rustfmt, ok := b.f.onPath("rustfmt"); ok {
		byEdition := map[string][]string{}
		var editions []string
		for _, d := range crateDirs {
			c := crates[d]
			if _, seen := byEdition[c.edition]; !seen {
				editions = append(editions, c.edition)
			}
			byEdition[c.edition] = append(byEdition[c.edition], c.files...)
		}
		sort.Strings(editions)
		var steps []*Step
		for _, ed := range editions {
			prefix := []string{rustfmt, "--check", "--edition", ed, "--color", "never"}
			for _, chunk := range b.chunks(prefix, byEdition[ed]) {
				// Exit status 1 also means "formatting differs", which is not our concern:
				// only the parse errors on stderr count.
				steps = append(steps, &Step{ID: "rs.syntax", Tier: 0, Lang: lang.Rust, Dir: b.root, Parser: diag.RustFull, Stream: Stderr, OKExit: []int{1},
					Argv: append(append([]string{}, prefix...), chunk...), Files: len(chunk)})
			}
		}
		number(steps)
		for _, s := range steps {
			b.add(s)
		}
	} else if len(crateDirs) > 0 {
		b.note(0, "rs.syntax skipped: rustfmt not found (rustup component add rustfmt)")
	}

	cargo, ok := b.f.onPath("cargo")
	if !ok {
		if len(crateDirs) > 0 {
			b.note(1, "rs.check skipped: cargo not found")
		}
		return
	}
	env := []string{"CARGO_TERM_COLOR=never", "RUST_BACKTRACE=0"}
	if n := b.env.Threads; n > 0 {
		env = append(env, "CARGO_BUILD_JOBS="+strconv.Itoa(n))
	}
	if b.cfg.Rust.Check != "off" {
		// cargo's own fingerprints restrict the work to the changed crate and its dependents.
		checked := map[string]struct{}{}
		for _, d := range crateDirs {
			c := crates[d]
			if _, dup := checked[c.ws]; dup {
				continue
			}
			checked[c.ws] = struct{}{}
			argv := []string{cargo, "check", "--message-format=short", "--color", "never", "-q"}
			if c.isWS {
				argv = append(argv, "--workspace")
			}
			if b.cfg.Rust.AllTargets {
				argv = append(argv, "--all-targets")
			}
			b.add(&Step{ID: stepID("rs.check", c.ws), Tier: 1, Phase: 0, Lang: lang.Rust, Argv: argv, Dir: b.abs(c.ws), Env: env, Parser: diag.GCC, Files: len(c.files)})
		}
	}
	if b.cfg.Rust.Tests != "off" {
		for _, d := range crateDirs {
			c := crates[d]
			if c.name == "" {
				continue // a virtual manifest has no tests of its own
			}
			argv := []string{cargo, "test", "--message-format=short", "--color", "never", "-q", "-p", c.name, "--bins"}
			if exists(b.abs(pathJoin(d, "src/lib.rs"))) || reLibTable.MatchString(b.read(pathJoin(d, "Cargo.toml"))) {
				argv = append(argv, "--lib") // --lib is an error for a binary-only crate
			}
			b.add(&Step{ID: stepID("rs.test", d), Tier: 1, Phase: 1, Lang: lang.Rust, Argv: argv, Dir: b.abs(c.ws), Env: env, Parser: diag.CargoTest, Files: len(c.files)})
		}
	}
}
