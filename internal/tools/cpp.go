package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/diag"
	"github.com/AgustinBouzonn/ai-guard/internal/lang"
)

type ccEntry struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

// splitCommand splits a compile_commands "command" string like a POSIX shell:
// quotes group, backslash escapes outside single quotes.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// splitCommandWindows splits a command line by the rules of the Microsoft C
// runtime (CommandLineToArgvW): backslashes are literal unless they precede a
// double quote, where 2n backslashes yield n and an odd one escapes the quote.
func splitCommandWindows(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					quoted = !quoted
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
			}
			inWord = true
		case c == '"':
			quoted, inWord = !quoted, true
		case (c == ' ' || c == '\t') && !quoted:
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func (b *builder) pathKey(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if b.env.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func (b *builder) loadCompileDB(path string, cache map[string]map[string]ccEntry) map[string]ccEntry {
	if db, ok := cache[path]; ok {
		return db
	}
	db := map[string]ccEntry{}
	if data, err := os.ReadFile(path); err == nil {
		var entries []ccEntry
		if json.Unmarshal(data, &entries) == nil {
			for _, e := range entries {
				file := filepath.FromSlash(e.File)
				if !filepath.IsAbs(file) {
					file = filepath.Join(filepath.FromSlash(e.Directory), file)
				}
				db[b.pathKey(file)] = e
			}
		}
	}
	cache[path] = db
	return db
}

func isMSVC(compiler string) bool {
	base := strings.ToLower(compiler[strings.LastIndexAny(compiler, `/\`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	return base == "cl" || base == "clang-cl"
}

// syntaxOnly rewrites a real compile command into "parse and analyse, emit
// nothing": outputs and dependency files are dropped, the flags that shape
// the translation unit (includes, defines, standard) stay.
func (b *builder) syntaxOnly(e ccEntry) (argv []string, msvc bool) {
	args := e.Arguments
	if len(args) == 0 {
		if b.env.GOOS == "windows" {
			args = splitCommandWindows(e.Command)
		} else {
			args = splitCommand(e.Command)
		}
	}
	for len(args) > 1 { // compiler-cache launchers wrap the real compiler
		base := strings.ToLower(args[0][strings.LastIndexAny(args[0], `/\`)+1:])
		if base = strings.TrimSuffix(base, ".exe"); base != "sccache" && base != "ccache" && base != "buildcache" {
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, false
	}
	msvc = isMSVC(args[0])
	src := filepath.FromSlash(e.File)
	if !filepath.IsAbs(src) {
		src = filepath.Join(filepath.FromSlash(e.Directory), src)
	}
	srcKey := b.pathKey(src)
	argv = []string{args[0]}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "/c":
		case a == "-o" || a == "-MF" || a == "-MT" || a == "-MQ":
			i++ // flag and its value
		case a == "-MD" || a == "-MMD" || a == "-MP" || a == "-MG":
		case strings.HasPrefix(a, "-MF"), strings.HasPrefix(a, "-MT"), strings.HasPrefix(a, "-MQ"):
		case msvc && (strings.HasPrefix(a, "/Fo") || strings.HasPrefix(a, "/Fd") || strings.HasPrefix(a, "/Fe") || strings.HasPrefix(a, "-Fo")):
		default:
			cand := filepath.FromSlash(a)
			if !filepath.IsAbs(cand) {
				cand = filepath.Join(filepath.FromSlash(e.Directory), cand)
			}
			if b.pathKey(cand) == srcKey {
				continue // the source file is re-added last
			}
			argv = append(argv, a)
		}
	}
	if msvc {
		return append(argv, "/Zs", e.File), true // /Zs: syntax check only
	}
	return append(argv, "-fsyntax-only", "-fdiagnostics-color=never", e.File), false
}

func (b *builder) cpp(files []string) {
	if b.cfg.Cpp.Syntax == "off" {
		return
	}
	dbs := map[string]map[string]ccEntry{}
	var steps []*Step
	for _, f := range files {
		if lang.IsHeader(f) {
			continue // a header is validated through the translation units that include it
		}
		if dir, name, ok := lang.FindUp(b.root, dirOf(f), "compile_commands.json", "build/compile_commands.json"); ok {
			db := b.loadCompileDB(b.abs(pathJoin(dir, name)), dbs)
			if e, ok := db[b.pathKey(b.abs(f))]; ok {
				if argv, msvc := b.syntaxOnly(e); argv != nil {
					s := &Step{ID: "cpp.syntax", Tier: 0, Lang: lang.Cpp, Argv: argv, Dir: filepath.FromSlash(e.Directory), Parser: diag.GCC, Files: 1}
					if msvc {
						s.Parser = diag.TSC // cl prints path(line,col): error C1234: ...
					}
					steps = append(steps, s)
					continue
				}
			}
		}
		cc := b.cfg.Cpp.Compiler
		if cc == "" {
			names := []string{"clang++", "g++", "c++"}
			if strings.HasSuffix(strings.ToLower(f), ".c") {
				names = []string{"clang", "gcc", "cc"}
			}
			for _, n := range names {
				if p, ok := b.f.onPath(n); ok {
					cc = p
					break
				}
			}
		}
		if cc == "" {
			b.note(0, "cpp.syntax skipped: no compile_commands.json entry and no compiler on PATH for %s", f)
			continue
		}
		steps = append(steps, &Step{ID: "cpp.syntax", Tier: 0, Lang: lang.Cpp, Dir: b.root, Parser: diag.GCC, Files: 1,
			Argv: []string{cc, "-fsyntax-only", "-fdiagnostics-color=never", f}})
	}
	number(steps)
	for _, s := range steps {
		b.add(s)
	}
}
