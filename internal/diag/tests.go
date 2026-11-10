package diag

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	rePySection = regexp.MustCompile(`^=+ (.+?) =+$`)
	rePyCollect = regexp.MustCompile(`^_+ ERROR collecting (.+?) _+$`)
	rePyTbLine  = regexp.MustCompile(`^(\S.*\.py):(\d+): (.+)$`)
	rePyInFrame = regexp.MustCompile(`^(\S.*\.py):(\d+): in `)
	rePyFileRef = regexp.MustCompile(`File "(.+?)", line (\d+)`)
)

// Pytest parses `pytest -q --tb=line -rfE`: one "file:line: Error" line per
// failure plus the short summary, which names the test. Collection errors
// (import and syntax errors in test modules) have their own block shape.
func Pytest(out []byte, c Ctx) []D {
	type tbLine struct {
		file string
		line int
		msg  string
	}
	type summary struct {
		kind, path, test, msg string
	}
	type collect struct {
		line int
		msg  string
	}
	var (
		section    string
		tbs        []tbLine
		sums       []summary
		collecting string
		collects   = map[string]*collect{}
	)
	for _, l := range lines(out) {
		if m := rePySection.FindStringSubmatch(l); m != nil {
			section, collecting = m[1], ""
			continue
		}
		if kind, rest, ok := strings.Cut(l, " "); ok && (kind == "FAILED" || kind == "ERROR") && !strings.HasPrefix(rest, " ") {
			nodeid, msg, _ := strings.Cut(rest, " - ")
			path, test, _ := strings.Cut(nodeid, "::")
			sums = append(sums, summary{kind, path, test, strings.TrimSpace(msg)})
			continue
		}
		switch section {
		case "ERRORS":
			if m := rePyCollect.FindStringSubmatch(l); m != nil {
				collecting = c.Rel(m[1])
				collects[collecting] = &collect{}
				continue
			}
			cur := collects[collecting]
			if cur == nil {
				continue
			}
			if m := rePyInFrame.FindStringSubmatch(l); m != nil && c.Rel(m[1]) == collecting {
				cur.line, _ = strconv.Atoi(m[2])
			}
			if strings.HasPrefix(l, "E ") {
				text := strings.TrimSpace(l[1:])
				if m := rePyFileRef.FindStringSubmatch(text); m != nil {
					if c.Rel(m[1]) == collecting {
						cur.line, _ = strconv.Atoi(m[2])
					}
				} else if strings.HasPrefix(l, "E   ") && !strings.HasPrefix(l, "E    ") {
					cur.msg = text // the exception line is indented by exactly three spaces
				}
			}
		case "FAILURES":
			if m := rePyTbLine.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, "E ") {
				n, _ := strconv.Atoi(m[2])
				tbs = append(tbs, tbLine{m[1], n, m[3]})
			}
		}
	}
	nFailed := 0
	for _, s := range sums {
		if s.kind == "FAILED" {
			nFailed++
		}
	}
	var ds []D
	next := 0
	for _, s := range sums {
		file := c.Rel(s.path)
		switch {
		case s.kind == "FAILED" && nFailed == len(tbs):
			tb := tbs[next]
			next++
			d := D{File: c.Rel(tb.file), Line: tb.line, Rule: s.test, Msg: s.msg}
			if d.File != file { // raised in application code: say which test got there
				d.Rule = s.path + "::" + s.test
			}
			if d.Msg == "" {
				d.Msg = tb.msg
			}
			ds = append(ds, d)
		case s.kind == "FAILED" || s.test != "":
			msg := s.msg
			if msg == "" {
				msg = "test failed"
			}
			ds = append(ds, D{File: file, Rule: s.test, Msg: msg})
		default: // ERROR <path>: the module could not be collected
			d := D{File: file, Msg: s.msg}
			if col := collects[file]; col != nil {
				d.Line = col.line
				if col.msg != "" {
					d.Msg = col.msg
				}
			}
			if d.Msg == "" {
				d.Msg = "collection error"
			}
			ds = append(ds, d)
		}
	}
	return ds
}

var (
	reRsPanic    = regexp.MustCompile(`^thread '(.+?)'(?: \(\d+\))? panicked at (.+):(\d+):(\d+):$`)
	reRsPanicOld = regexp.MustCompile(`^thread '(.+?)' panicked at '(.*)', (.+):(\d+):(\d+)$`)
	reRsHeader   = regexp.MustCompile(`^(error|warning)(?:\[([A-Z]\d+)\])?: (.*)$`)
	reRsArrow    = regexp.MustCompile(`^\s*--> (.+):(\d+):(\d+)$`)
)

// CargoTest parses `cargo test --message-format=short`: compiler diagnostics
// in short form, then one panic block per failing test.
func CargoTest(out []byte, c Ctx) []D {
	ds := GCC(out, c)
	ls := lines(out)
	for i := 0; i < len(ls); i++ {
		if m := reRsPanicOld.FindStringSubmatch(ls[i]); m != nil { // Rust < 1.73
			line, _ := strconv.Atoi(m[4])
			col, _ := strconv.Atoi(m[5])
			ds = append(ds, D{File: c.Rel(m[3]), Line: line, Col: col, Rule: m[1], Msg: m[2]})
			continue
		}
		m := reRsPanic.FindStringSubmatch(ls[i])
		if m == nil {
			continue
		}
		var msg []string
		for i+1 < len(ls) {
			t := strings.TrimSpace(ls[i+1])
			if t == "" || strings.HasPrefix(t, "note:") || strings.HasPrefix(t, "stack backtrace:") || strings.HasPrefix(t, "----") {
				break
			}
			msg = append(msg, t)
			i++
		}
		line, _ := strconv.Atoi(m[3])
		col, _ := strconv.Atoi(m[4])
		ds = append(ds, D{File: c.Rel(m[2]), Line: line, Col: col, Rule: m[1], Msg: strings.Join(msg, " ")})
	}
	return ds
}

// RustFull parses the human rustc/rustfmt format: a header line followed by
// a " --> path:line:col" pointer.
func RustFull(out []byte, c Ctx) []D {
	var ds []D
	var pending *D
	for _, l := range lines(out) {
		if m := reRsHeader.FindStringSubmatch(l); m != nil {
			pending = &D{Rule: m[2], Msg: m[3]}
			if m[1] == "warning" {
				pending.Sev = Warning
			}
			continue
		}
		if m := reRsArrow.FindStringSubmatch(l); m != nil && pending != nil {
			pending.File = c.Rel(m[1])
			pending.Line, _ = strconv.Atoi(m[2])
			pending.Col, _ = strconv.Atoi(m[3])
			ds = append(ds, *pending)
			pending = nil
		}
	}
	return ds
}
