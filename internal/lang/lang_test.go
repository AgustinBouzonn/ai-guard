package lang

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOfClassifiesByExtension(t *testing.T) {
	cases := map[string]Lang{
		"src/a.ts":            TS,
		"src/a.d.ts":          TS,
		"web/App.tsx":         TS,
		"scripts/build.mjs":   TS,
		"legacy/x.cjs":        TS,
		"x.jsx":               TS,
		"x.mts":               TS,
		"pkg/mod.py":          Py,
		"stubs/mod.pyi":       Py,
		"cmd/main.go":         Go,
		"crates/a/src/lib.rs": Rust,
		"native/a.cpp":        Cpp,
		"native/a.cc":         Cpp,
		"native/a.cxx":        Cpp,
		"native/a.c":          Cpp,
		"native/a.hpp":        Cpp,
		"native/a.h":          Cpp,
		"README.md":           None,
		"Makefile":            None,
		"data.json":           None,
		"UPPER.PY":            Py, // Windows checkouts are case-insensitive
	}
	for path, want := range cases {
		if got := Of(path); got != want {
			t.Errorf("Of(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestIsHeader(t *testing.T) {
	for path, want := range map[string]bool{"a.h": true, "a.hpp": true, "a.hh": true, "a.hxx": true, "a.cpp": false, "a.c": false} {
		if got := IsHeader(path); got != want {
			t.Errorf("IsHeader(%q) = %v, want %v", path, got, want)
		}
	}
}

func touch(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindUpReturnsNearestDirectoryHoldingAMarker(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "package.json", "apps/web/package.json", "apps/web/src/ui/button.tsx", "libs/core/src/x.ts")

	dir, name, ok := FindUp(root, "apps/web/src/ui", "package.json")
	if !ok || dir != "apps/web" || name != "package.json" {
		t.Errorf("FindUp from apps/web/src/ui = (%q, %q, %v), want (apps/web, package.json, true)", dir, name, ok)
	}
	dir, _, ok = FindUp(root, "libs/core/src", "package.json")
	if !ok || dir != "." {
		t.Errorf("FindUp from libs/core/src = (%q, %v), want the repo root \".\"", dir, ok)
	}
}

func TestFindUpPrefersEarlierNamesInTheSameDirectory(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "svc/tsconfig.json", "svc/tsconfig.fast-check.json", "svc/src/a.ts")
	_, name, ok := FindUp(root, "svc/src", "tsconfig.fast-check.json", "tsconfig.json")
	if !ok || name != "tsconfig.fast-check.json" {
		t.Errorf("FindUp picked %q, want tsconfig.fast-check.json", name)
	}
}

func TestFindUpStopsAtTheRepositoryRoot(t *testing.T) {
	outer := t.TempDir()
	touch(t, outer, "pyproject.toml", "repo/pkg/a.py")
	if dir, name, ok := FindUp(filepath.Join(outer, "repo"), "pkg", "pyproject.toml"); ok {
		t.Errorf("FindUp escaped the root and found (%q, %q)", dir, name)
	}
}
