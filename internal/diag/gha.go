package diag

import (
	"strconv"
	"strings"
)

type ghaCmd struct {
	level string
	props map[string]string
	msg   string
}

var (
	ghaProp = strings.NewReplacer("%2C", ",", "%3A", ":", "%0A", "\n", "%0D", "\r", "%25", "%")
	ghaData = strings.NewReplacer("%0A", "\n", "%0D", "\r", "%25", "%")
)

// parseGHA reads GitHub Actions workflow commands
// ("::error file=a.ts,line=1,col=2,title=x::message"), an ASCII, line-oriented
// format that several tools can emit and none of them localises.
func parseGHA(out []byte) []ghaCmd {
	var cmds []ghaCmd
	for _, l := range lines(out) {
		if !strings.HasPrefix(l, "::") {
			continue
		}
		body := l[2:]
		end := strings.Index(body, "::")
		if end < 0 {
			continue
		}
		head, msg := body[:end], body[end+2:]
		level, rawProps, _ := strings.Cut(head, " ")
		c := ghaCmd{level: level, props: map[string]string{}, msg: ghaData.Replace(msg)}
		for _, kv := range strings.Split(rawProps, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				c.props[strings.TrimSpace(k)] = ghaProp.Replace(v)
			}
		}
		cmds = append(cmds, c)
	}
	return cmds
}

func (g ghaCmd) diag(c Ctx) (D, bool) {
	d := D{File: c.Rel(g.props["file"]), Msg: firstLine(g.msg)}
	switch g.level {
	case "error":
	case "warning":
		d.Sev = Warning
	default:
		return d, false
	}
	d.Line, _ = strconv.Atoi(g.props["line"])
	col := g.props["col"]
	if col == "" {
		col = g.props["column"]
	}
	d.Col, _ = strconv.Atoi(col)
	return d, true
}

// Biome parses `biome lint --reporter=github`.
func Biome(out []byte, c Ctx) []D {
	var ds []D
	for _, g := range parseGHA(out) {
		d, ok := g.diag(c)
		if !ok {
			continue
		}
		d.Rule = g.props["title"] // lint/suspicious/noDebugger -> noDebugger
		if i := strings.LastIndexByte(d.Rule, '/'); i >= 0 {
			d.Rule = d.Rule[i+1:]
		}
		ds = append(ds, d)
	}
	return ds
}

// Vitest parses `vitest --reporter=github-actions`.
func Vitest(out []byte, c Ctx) []D {
	var ds []D
	for _, g := range parseGHA(out) {
		d, ok := g.diag(c)
		if !ok {
			continue
		}
		// title is "path/to/file.test.ts > suite > test": keep the test name
		if _, name, found := strings.Cut(g.props["title"], " > "); found {
			d.Rule = name
		} else {
			d.Rule = g.props["title"]
		}
		ds = append(ds, d)
	}
	return ds
}
