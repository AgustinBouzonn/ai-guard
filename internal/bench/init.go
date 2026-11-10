package bench

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

// probes are the simulated edits per toolchain: one that is correct, one that
// does not parse, one that parses but does not type-check.
var probes = map[lang.Lang][3]string{
	lang.TS: {
		"\nconst __aiguardBench{n} = (a: number, b: number): number => a + b;\nvoid __aiguardBench{n};\n",
		"\nconst __aiguardBench{n} = (a: number => ;\n",
		"\nconst __aiguardBench{n}: number = \"not a number\";\nvoid __aiguardBench{n};\n",
	},
	lang.Py: {
		"\n\ndef _aiguard_bench_{n}(a: int, b: int) -> int:\n    return a + b\n",
		"\n\ndef _aiguard_bench_{n}(a:\n    return a +\n",
		"\n\n_aiguard_bench_{n}: int = \"not an int\"\n",
	},
	lang.Go: {
		"\n// AiguardBench{n} is a benchmark probe.\nfunc AiguardBench{n}(a, b int) int { return a + b }\n",
		"\nfunc AiguardBench{n}(a, b int) int { return a + }\n",
		"\nvar AiguardBench{n} int = \"not an int\"\n",
	},
	lang.Rust: {
		"\n#[allow(dead_code)]\npub fn aiguard_bench_{n}(a: i32, b: i32) -> i32 {\n    a + b\n}\n",
		"\npub fn aiguard_bench_{n}(a: i32 -> i32 {\n    a +\n}\n",
		"\n#[allow(dead_code)]\npub fn aiguard_bench_{n}() -> i32 {\n    \"not an i32\"\n}\n",
	},
}

var skipDirs = []string{"node_modules/", "vendor/", "dist/", "build/", "target/", ".venv/", "venv/", "migrations/", "generated/", "__pycache__/", "third_party/"}

// candidate reports whether a file is ordinary hand-written source: not a
// test, not generated, not configuration.
func candidate(f string, l lang.Lang) bool {
	low := strings.ToLower(f)
	for _, d := range skipDirs {
		if strings.HasPrefix(low, d) || strings.Contains(low, "/"+d) {
			return false
		}
	}
	base := path.Base(low)
	for _, d := range []string{"tests/", "test/", "__tests__/", "e2e/"} {
		if strings.HasPrefix(low, d) || strings.Contains(low, "/"+d) {
			return false
		}
	}
	switch l {
	case lang.TS:
		ext := path.Ext(base)
		return (ext == ".ts" || ext == ".tsx" || ext == ".mts" || ext == ".cts") && !strings.HasSuffix(base, ".d.ts") &&
			!strings.Contains(base, ".test.") && !strings.Contains(base, ".spec.") && !strings.Contains(base, ".config.")
	case lang.Py:
		return strings.HasSuffix(base, ".py") && !strings.HasPrefix(base, "test_") && !strings.HasSuffix(base, "_test.py") &&
			base != "__init__.py" && base != "conftest.py" && base != "setup.py"
	case lang.Go:
		return !strings.HasSuffix(base, "_test.go")
	case lang.Rust:
		return base != "build.rs"
	}
	return false
}

func findAny(root, dir string, names ...string) (string, bool) {
	d, _, ok := lang.FindUp(root, dir, names...)
	return d, ok
}

func dirField(d string) string {
	if d == "." {
		return ""
	}
	return d
}

// Init drafts a benchmark for the repository: three probes on a typical file
// of each toolchain, and the conventional legacy commands as the baseline.
// The draft is meant to be edited until the baseline is the team's real one.
func Init(root string, tracked []string, self string) Config {
	cfg := Config{Reps: 5, Warmup: 1, FailFast: true, TimeoutSec: 1800,
		Optimized: Command{Name: "ai-guard", Cmd: []string{self, "check", "--no-tier2", "--files", "{file}"}, DeferredExit: 4}}
	for _, l := range []lang.Lang{lang.TS, lang.Py, lang.Go, lang.Rust} {
		type sized struct {
			file string
			size int64
		}
		var cands []sized
		for _, f := range tracked {
			if lang.Of(f) == l && candidate(f, l) {
				if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err == nil && fi.Size() > 0 {
					cands = append(cands, sized{f, fi.Size()})
				}
			}
		}
		if len(cands) == 0 {
			continue
		}
		sort.Slice(cands, func(a, b int) bool {
			if cands[a].size != cands[b].size {
				return cands[a].size < cands[b].size
			}
			return cands[a].file < cands[b].file
		})
		file := cands[(len(cands)-1)/2].file // median size: neither a stub nor the biggest module
		for i, kind := range []string{"valid", "syntax", "type"} {
			cfg.Patches = append(cfg.Patches, Patch{Name: string(l) + "-" + kind, Lang: string(l), Kind: kind, File: file, Append: probes[l][i]})
		}
		dir, langs := path.Dir(file), []string{string(l)}
		switch l {
		case lang.TS:
			if d, ok := findAny(root, dir, "eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", ".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml"); ok {
				cfg.Baseline = append(cfg.Baseline, Command{Name: "eslint", Shell: "npx --no-install eslint .", Dir: dirField(d), Langs: langs})
			}
			if d, ok := findAny(root, dir, "tsconfig.json"); ok {
				cfg.Baseline = append(cfg.Baseline, Command{Name: "tsc", Shell: "npx --no-install tsc --noEmit", Dir: dirField(d), Langs: langs})
			}
		case lang.Py:
			d, ok := findAny(root, dir, "pyproject.toml", "setup.cfg", "setup.py", "requirements.txt", "tox.ini", ".flake8")
			if !ok {
				d = "."
			}
			bin := ""
			for _, v := range []string{".venv/bin/", ".venv/Scripts/", "venv/bin/", "venv/Scripts/"} {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d), filepath.FromSlash(v))); err == nil {
					bin = filepath.FromSlash(v)
					break
				}
			}
			cfg.Baseline = append(cfg.Baseline,
				Command{Name: "flake8", Shell: bin + "flake8 .", Dir: dirField(d), Langs: langs},
				Command{Name: "black", Shell: bin + "black --check .", Dir: dirField(d), Langs: langs},
				Command{Name: "pytest", Shell: bin + "pytest -q", Dir: dirField(d), Langs: langs})
		case lang.Go:
			if d, ok := findAny(root, dir, "go.mod"); ok {
				cfg.Baseline = append(cfg.Baseline, Command{Name: "go build", Shell: "go build ./...", Dir: dirField(d), Langs: langs})
			}
		case lang.Rust:
			if d, ok := findAny(root, dir, "Cargo.toml"); ok {
				for up := d; up != "."; { // prefer the enclosing workspace: that is what a legacy build compiles and links
					parent, found := findAny(root, path.Dir(up), "Cargo.toml")
					if !found {
						break
					}
					if data, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(parent), "Cargo.toml")); strings.Contains(string(data), "[workspace]") {
						d = parent
					}
					up = parent
				}
				cfg.Baseline = append(cfg.Baseline, Command{Name: "cargo build", Shell: "cargo build", Dir: dirField(d), Langs: langs})
			}
		}
	}
	return cfg
}
