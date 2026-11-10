// Package diag turns tool output into uniform, repository-relative diagnostics.
package diag

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Severity of a diagnostic. Errors fail a tier; warnings are reported only on request.
type Severity uint8

const (
	Error Severity = iota
	Warning
)

// D is one finding, addressed the way an editor (or an agent) can jump to it.
type D struct {
	File string // repository-relative slash path; "" when the tool gave none
	Line int    // 1-based; 0 = unknown
	Col  int    // 1-based; 0 = unknown
	Rule string // lint rule, error code or failing test
	Msg  string
	Sev  Severity
}

// Ctx tells a parser how to interpret the paths a tool printed.
type Ctx struct {
	Root   string            // absolute repository root
	Cwd    string            // absolute directory the tool ran in
	PkgDir map[string]string // Go import path -> absolute directory (go test only)
}

// Parser converts the captured output of one tool run.
type Parser func(out []byte, c Ctx) []D

// Rel maps a path printed by a tool (absolute, or relative to its cwd) to a
// repository-relative slash path. Paths outside the repository stay absolute.
func (c Ctx) Rel(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	n := filepath.FromSlash(p)
	if !filepath.IsAbs(n) {
		n = filepath.Join(c.Cwd, n)
	}
	n = filepath.Clean(n)
	if rel, err := filepath.Rel(c.Root, n); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(n)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// splitColon parses "path:line[:col]: rest". A Windows drive prefix ("C:\")
// is part of the path, and a bare number ("12:30:45: ...") is not a path.
func splitColon(s string) (file string, line, col int, rest string, ok bool) {
	start := 0
	if len(s) > 2 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') &&
		((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		start = 2
	}
	for i := start; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		j := i + 1
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		if j == i+1 || j >= len(s) || s[j] != ':' {
			continue
		}
		file = s[:i]
		if file == "" || strings.Trim(file, "0123456789") == "" {
			return "", 0, 0, "", false
		}
		line, _ = strconv.Atoi(s[i+1 : j])
		k := j + 1
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k > j+1 && k < len(s) && s[k] == ':' {
			col, _ = strconv.Atoi(s[j+1 : k])
			j = k
		}
		return file, line, col, strings.TrimLeft(s[j+1:], " \t"), true
	}
	return "", 0, 0, "", false
}

var reParenLoc = regexp.MustCompile(`^(.+?)\((\d+)(?:,(\d+))?\)\s*:\s*(.*)$`)

// splitParen parses "path(line[,col]): rest" (tsc, MSVC, C#).
func splitParen(s string) (file string, line, col int, rest string, ok bool) {
	m := reParenLoc.FindStringSubmatch(s)
	if m == nil {
		return "", 0, 0, "", false
	}
	line, _ = strconv.Atoi(m[2])
	col, _ = strconv.Atoi(m[3])
	return m[1], line, col, m[4], true
}

func lines(out []byte) []string {
	ls := strings.Split(string(out), "\n")
	for i, l := range ls {
		ls[i] = strings.TrimRight(l, "\r")
	}
	return ls
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

var parsers = map[string]Parser{
	"biome": Biome, "vitest": Vitest, "ruff": Ruff, "oxlint": Oxlint, "tsc": TSC, "gcc": GCC,
	"go": GoTool, "gotest": GoTest, "pyright": Pyright, "pytest": Pytest, "cargotest": CargoTest,
	"rust": RustFull, "jest": Jest, "generic": Generic, "esbuild": Esbuild, "eslint": ESLint,
}

// ByName resolves the parser names accepted in aiguard.json custom steps.
func ByName(name string) (Parser, bool) {
	p, ok := parsers[name]
	return p, ok
}
