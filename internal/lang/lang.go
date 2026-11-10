// Package lang classifies files and locates project roots.
package lang

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Lang is a toolchain family, not a single language: TS covers everything the
// JS/TS tools accept, Cpp covers C and C++.
type Lang string

const (
	None Lang = ""
	TS   Lang = "ts"
	Py   Lang = "py"
	Go   Lang = "go"
	Rust Lang = "rs"
	Cpp  Lang = "cpp"
)

// All lists the supported families in a stable order.
var All = []Lang{TS, Py, Go, Rust, Cpp}

var byExt = map[string]Lang{
	".ts": TS, ".tsx": TS, ".mts": TS, ".cts": TS, ".js": TS, ".jsx": TS, ".mjs": TS, ".cjs": TS,
	".py": Py, ".pyi": Py,
	".go": Go,
	".rs": Rust,
	".c":  Cpp, ".cc": Cpp, ".cpp": Cpp, ".cxx": Cpp, ".h": Cpp, ".hh": Cpp, ".hpp": Cpp, ".hxx": Cpp,
}

// Of returns the toolchain family of a file, or None.
func Of(p string) Lang { return byExt[strings.ToLower(path.Ext(filepath.ToSlash(p)))] }

// IsHeader reports whether p is a C/C++ header (not a translation unit).
func IsHeader(p string) bool {
	switch strings.ToLower(path.Ext(filepath.ToSlash(p))) {
	case ".h", ".hh", ".hpp", ".hxx":
		return true
	}
	return false
}

// FindUp walks from startRel (a slash path relative to root) up to root and
// returns the first directory containing one of names; within a directory,
// earlier names win. dirRel is "." for the root itself.
func FindUp(root, startRel string, names ...string) (dirRel, name string, ok bool) {
	dir := path.Clean(filepath.ToSlash(startRel))
	if dir == "" || strings.HasPrefix(dir, "..") || path.IsAbs(dir) {
		dir = "."
	}
	for {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), n)); err == nil {
				return dir, n, true
			}
		}
		if dir == "." {
			return "", "", false
		}
		dir = path.Dir(dir)
	}
}
