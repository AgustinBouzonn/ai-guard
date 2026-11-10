package diag

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// jsonFrom decodes the first JSON value in out, tolerating banner lines before it.
func jsonFrom(out []byte, v any) bool {
	i := bytes.IndexByte(out, '{')
	if i < 0 {
		return false
	}
	return json.NewDecoder(bytes.NewReader(out[i:])).Decode(v) == nil
}

// Pyright parses `pyright --outputjson` (positions are 0-based there).
func Pyright(out []byte, c Ctx) []D {
	var doc struct {
		GeneralDiagnostics []struct {
			File     string `json:"file"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
			Rule     string `json:"rule"`
			Range    struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"generalDiagnostics"`
	}
	if !jsonFrom(out, &doc) {
		return nil
	}
	var ds []D
	for _, g := range doc.GeneralDiagnostics {
		d := D{File: c.Rel(g.File), Line: g.Range.Start.Line + 1, Col: g.Range.Start.Character + 1, Rule: g.Rule, Msg: firstLine(g.Message)}
		switch g.Severity {
		case "error":
		case "warning":
			d.Sev = Warning
		default:
			continue
		}
		ds = append(ds, d)
	}
	return ds
}

var reJSFrame = regexp.MustCompile(`\(?((?:[A-Za-z]:)?[^():\s]+):(\d+):(\d+)\)?\s*$`)

// Jest parses `jest --json --testLocationInResults`.
func Jest(out []byte, c Ctx) []D {
	var doc struct {
		TestResults []struct {
			Name             string `json:"name"`
			Status           string `json:"status"`
			Message          string `json:"message"`
			AssertionResults []struct {
				Status   string `json:"status"`
				FullName string `json:"fullName"`
				Location *struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"location"`
				FailureMessages []string `json:"failureMessages"`
			} `json:"assertionResults"`
		} `json:"testResults"`
	}
	if !jsonFrom(out, &doc) {
		return nil
	}
	var ds []D
	for _, tr := range doc.TestResults {
		failed := 0
		for _, a := range tr.AssertionResults {
			if a.Status != "failed" {
				continue
			}
			failed++
			d := D{File: c.Rel(tr.Name), Rule: a.FullName, Msg: "test failed"}
			if a.Location != nil {
				d.Line, d.Col = a.Location.Line, a.Location.Column
			}
			if len(a.FailureMessages) > 0 {
				var msg []string
				located := false
				for _, l := range strings.Split(a.FailureMessages[0], "\n") {
					t := strings.TrimSpace(l)
					if !strings.HasPrefix(t, "at ") {
						if !located && t != "" {
							msg = append(msg, t)
						}
						continue
					}
					// first stack frame in project code is where it failed
					if m := reJSFrame.FindStringSubmatch(t); m != nil && !located && !strings.Contains(m[1], "node_modules") {
						d.File = c.Rel(m[1])
						d.Line, _ = strconv.Atoi(m[2])
						d.Col, _ = strconv.Atoi(m[3])
					}
					located = true
				}
				if len(msg) > 0 {
					d.Msg = strings.Join(msg, " ")
				}
			}
			ds = append(ds, d)
		}
		if failed == 0 && tr.Status == "failed" { // the file itself did not load
			var msg []string
			for _, l := range strings.Split(tr.Message, "\n") {
				t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "●"))
				if strings.HasPrefix(t, "at ") || len(msg) == 3 {
					break
				}
				if t != "" {
					msg = append(msg, t)
				}
			}
			ds = append(ds, D{File: c.Rel(tr.Name), Msg: strings.Join(msg, " ")})
		}
	}
	return ds
}

var reGoTestLoc = regexp.MustCompile(`^\s+([^\s:]+\.go):(\d+): (.*)$`)

// GoTest parses `go test -json`. Since Go 1.24 compiler errors arrive as
// build-output events; older toolchains print them as plain lines, which are
// handled too.
func GoTest(out []byte, c Ctx) []D {
	type event struct {
		Action, Package, ImportPath, Test, Output, FailedBuild string
	}
	type key struct{ pkg, test string }
	var (
		ds        []D
		order     []key // failed tests and packages, in event order ("" test = package)
		locs      = map[key][]D{}
		panics    = map[key]string{}
		pkgTail   = map[string]string{}
		failed    = map[key]bool{}
		buildFail = map[string]bool{}
	)
	pkgDir := func(pkg string) string {
		if dir, ok := c.PkgDir[pkg]; ok {
			return c.Rel(dir)
		}
		return pkg
	}
	for _, l := range lines(out) {
		if !strings.HasPrefix(l, "{") {
			ds = append(ds, GoTool([]byte(l), c)...)
			continue
		}
		var e event
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		switch e.Action {
		case "build-output":
			ds = append(ds, GoTool([]byte(e.Output), c)...)
		case "output":
			text := strings.TrimSpace(e.Output)
			k := key{e.Package, e.Test}
			if strings.HasPrefix(text, "panic:") && panics[k] == "" {
				panics[k] = text
			}
			if e.Test == "" {
				if text != "" && text != "FAIL" && text != "PASS" && !strings.HasPrefix(text, "FAIL\t") &&
					!strings.HasPrefix(text, "ok ") && !strings.HasPrefix(text, "exit status") && !strings.HasPrefix(text, "coverage:") {
					pkgTail[e.Package] = text
				}
				continue
			}
			if m := reGoTestLoc.FindStringSubmatch(strings.TrimRight(e.Output, "\r\n")); m != nil {
				file := m[1]
				if dir, ok := c.PkgDir[e.Package]; ok {
					file = filepath.Join(dir, file)
				}
				line, _ := strconv.Atoi(m[2])
				locs[k] = append(locs[k], D{File: c.Rel(file), Line: line, Rule: e.Test, Msg: m[3]})
			}
		case "fail":
			k := key{e.Package, e.Test}
			failed[k] = true
			order = append(order, k)
			if e.FailedBuild != "" {
				buildFail[e.Package] = true
			}
		}
	}
	testFailedIn := map[string]bool{}
	for k := range failed {
		if k.test != "" {
			testFailedIn[k.pkg] = true
		}
	}
	for _, k := range order {
		switch {
		case k.test == "":
			if buildFail[k.pkg] || testFailedIn[k.pkg] {
				continue // already explained by a compiler error or a failing test
			}
			why := panics[k]
			if why == "" {
				why = pkgTail[k.pkg]
			}
			ds = append(ds, D{File: pkgDir(k.pkg), Msg: strings.TrimSuffix("package tests failed: "+why, ": ")})
		case len(locs[k]) > 0:
			ds = append(ds, locs[k]...)
		default:
			hasFailedSubtest := false
			for other := range failed {
				if other.pkg == k.pkg && strings.HasPrefix(other.test, k.test+"/") {
					hasFailedSubtest = true
				}
			}
			if hasFailedSubtest {
				continue // the subtest carries the message
			}
			msg := panics[k]
			if msg == "" {
				msg = "test failed"
			}
			ds = append(ds, D{File: pkgDir(k.pkg), Rule: k.test, Msg: msg})
		}
	}
	return ds
}

// ESLint parses `eslint --format json` (an array of per-file results).
func ESLint(out []byte, c Ctx) []D {
	i := bytes.IndexByte(out, '[')
	if i < 0 {
		return nil
	}
	var files []struct {
		FilePath string `json:"filePath"`
		Messages []struct {
			RuleID   *string `json:"ruleId"`
			Severity int     `json:"severity"`
			Message  string  `json:"message"`
			Line     int     `json:"line"`
			Column   int     `json:"column"`
		} `json:"messages"`
	}
	if json.NewDecoder(bytes.NewReader(out[i:])).Decode(&files) != nil {
		return nil
	}
	var ds []D
	for _, f := range files {
		for _, m := range f.Messages {
			d := D{File: c.Rel(f.FilePath), Line: m.Line, Col: m.Column, Msg: firstLine(m.Message)}
			if m.RuleID != nil {
				d.Rule = *m.RuleID
			}
			if m.Severity < 2 {
				d.Sev = Warning
			}
			ds = append(ds, d)
		}
	}
	return ds
}
