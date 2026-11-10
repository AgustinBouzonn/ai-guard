package diag

import (
	"regexp"
	"strings"
)

var (
	// "error: msg", "warning[E0308]: msg", "error[rule] msg" (ty), "note: ..."
	reSevTail = regexp.MustCompile(`^(fatal error|error|warning|note|help|info)(?:\[([^\]]+)\])?:?\s+(.*)$`)
	// trailing "[code]": mypy error codes, clang/gcc -W flags
	reTrailRule = regexp.MustCompile(`^(.*\S)\s+\[([^\[\]\s]+)\]$`)
	reRuffCode  = regexp.MustCompile(`^([A-Z]+[0-9]+)\s+(?:\[\*\]\s+)?(.*)$`)
	reRuffNamed = regexp.MustCompile(`^([a-z][a-z0-9-]*):\s+(.*)$`)
	reOxTail    = regexp.MustCompile(`^(.*\S)\s+\[(Error|Warning)(?:/([^\]]+))?\]$`)
	reCodeTail  = regexp.MustCompile(`^(fatal error|error|warning)\s+([A-Za-z]+\d+)\s*:\s*(.*)$`)
	reTSGlobal  = regexp.MustCompile(`^error\s+(TS\d+):\s*(.*)$`)
)

// sevTail interprets "<severity>[code]: message [trailing-code]".
// skip is true for notes and hints, which only elaborate a previous finding.
func sevTail(rest string) (sev Severity, rule, msg string, matched, skip bool) {
	m := reSevTail.FindStringSubmatch(rest)
	if m == nil {
		return Error, "", rest, false, false
	}
	switch m[1] {
	case "note", "help", "info":
		return Error, "", "", true, true
	case "warning":
		sev = Warning
	}
	rule, msg = m[2], strings.TrimSpace(m[3])
	if rule == "" {
		if t := reTrailRule.FindStringSubmatch(msg); t != nil {
			msg, rule = t[1], t[2]
		}
	}
	return sev, rule, msg, true, false
}

// GCC parses "path:line:col: severity: message", the shape shared by clang,
// gcc, cargo --message-format=short, mypy and ty --output-format concise.
func GCC(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		file, line, col, rest, ok := splitColon(l)
		if !ok {
			continue
		}
		sev, rule, msg, matched, skip := sevTail(rest)
		if !matched || skip || msg == "" {
			continue
		}
		ds = append(ds, D{File: c.Rel(file), Line: line, Col: col, Rule: rule, Msg: msg, Sev: sev})
	}
	return ds
}

// Ruff parses `ruff check --output-format=concise`.
func Ruff(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		file, line, col, rest, ok := splitColon(l)
		if !ok || rest == "" {
			continue
		}
		d := D{File: c.Rel(file), Line: line, Col: col, Msg: rest}
		if m := reRuffCode.FindStringSubmatch(rest); m != nil {
			d.Rule, d.Msg = m[1], m[2]
		} else if m := reRuffNamed.FindStringSubmatch(rest); m != nil {
			d.Rule, d.Msg = m[1], m[2]
		}
		ds = append(ds, d)
	}
	return ds
}

// Oxlint parses `oxlint -f unix`.
func Oxlint(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		file, line, col, rest, ok := splitColon(l)
		if !ok {
			continue
		}
		m := reOxTail.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		d := D{File: c.Rel(file), Line: line, Col: col, Msg: m[1], Rule: m[3]}
		if m[2] == "Warning" {
			d.Sev = Warning
		}
		if i := strings.IndexByte(d.Rule, '('); i >= 0 && strings.HasSuffix(d.Rule, ")") { // eslint(no-debugger)
			d.Rule = d.Rule[i+1 : len(d.Rule)-1]
		}
		ds = append(ds, d)
	}
	return ds
}

func codeTail(d *D, rest string) {
	d.Msg = rest
	if m := reCodeTail.FindStringSubmatch(rest); m != nil {
		d.Rule, d.Msg = m[2], m[3]
		if m[1] == "warning" {
			d.Sev = Warning
		}
	}
}

// TSC parses `tsc --pretty false` ("path(line,col): error TS1234: message").
// MSVC and C# compilers print the same shape.
func TSC(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		if file, line, col, rest, ok := splitParen(l); ok {
			d := D{File: c.Rel(file), Line: line, Col: col}
			codeTail(&d, rest)
			ds = append(ds, d)
		} else if m := reTSGlobal.FindStringSubmatch(l); m != nil { // config-level error, no location
			ds = append(ds, D{Rule: m[1], Msg: m[2]})
		}
	}
	return ds
}

// GoTool parses gofmt, go vet and go build ("path:line:col: message").
func GoTool(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		if strings.HasPrefix(l, "#") {
			continue
		}
		file, line, col, rest, ok := splitColon(strings.TrimPrefix(l, "vet: "))
		if !ok || rest == "" || !strings.HasSuffix(file, ".go") {
			continue
		}
		ds = append(ds, D{File: c.Rel(file), Line: line, Col: col, Msg: rest})
	}
	return ds
}

// Generic is the best-effort parser for custom steps: any line carrying a
// "path:line:col:" or "path(line,col):" location becomes a diagnostic.
func Generic(out []byte, c Ctx) []D {
	var ds []D
	for _, l := range lines(out) {
		if file, line, col, rest, ok := splitColon(l); ok {
			sev, rule, msg, _, skip := sevTail(rest)
			if skip || msg == "" {
				continue
			}
			ds = append(ds, D{File: c.Rel(file), Line: line, Col: col, Rule: rule, Msg: msg, Sev: sev})
		} else if file, line, col, rest, ok := splitParen(l); ok {
			d := D{File: c.Rel(file), Line: line, Col: col}
			codeTail(&d, rest)
			ds = append(ds, d)
		}
	}
	return ds
}

// Esbuild parses esbuild's log (--color=false): a "✘ [ERROR] message" line,
// optionally followed by an indented "path:line:col:" pointer (0-based column).
func Esbuild(out []byte, c Ctx) []D {
	var ds []D
	ls := lines(out)
	for i := 0; i < len(ls); i++ {
		_, msg, ok := strings.Cut(ls[i], "[ERROR] ")
		if !ok {
			continue
		}
		for j := i + 1; j < len(ls) && j <= i+2; j++ {
			loc := strings.TrimSpace(ls[j])
			if loc == "" {
				continue
			}
			// No location: a bad flag or an output clash, not an error in the code.
			if file, line, col, _, ok := splitColon(loc + " "); ok {
				ds = append(ds, D{File: c.Rel(file), Line: line, Col: col + 1, Rule: "syntax", Msg: strings.TrimSpace(msg)})
			}
			break
		}
	}
	return ds
}
