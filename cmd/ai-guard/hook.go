package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AgustinBouzonn/ai-guard/internal/gitx"
)

// hookStdin is where the hook event arrives; tests replace it.
var hookStdin io.Reader = os.Stdin

// hookInput is the part of a Claude Code hook event the hook needs.
type hookInput struct {
	Event string `json:"hook_event_name"`
	Cwd   string `json:"cwd"`
}

// readHookInput never fails: an empty or malformed event is the zero value.
func readHookInput(r io.Reader) hookInput {
	var in hookInput
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil || json.Unmarshal(b, &in) != nil {
		return hookInput{}
	}
	return in
}

const hookNext = "`ai-guard check` found errors in the changed files (above: the file, then `line:col rule message`). " +
	"Fix exactly those and run `ai-guard check` again until it reports OK. " +
	"Do not run the project's full lint, type-check or test commands for this.\n"

// cmdHook is a Claude Code Stop / SubagentStop hook: when a turn ends it runs
// the check on the git delta and, if the code has errors, hands them back to
// the agent. It answers in the two ways Claude Code understands: exit 2 with
// the report on stderr blocks the turn and goes to the model; exit 0 lets the
// turn end, with an optional {"systemMessage": ...} on stdout for the person.
//
// It never breaks a session. Outside a repository, on a clean tree, or when
// anything unexpected happens, it exits 0 and says nothing; a tool that could
// not run, or a check that was deferred, is reported to the person and does
// not block. A delta that already passed is not checked again.
func cmdHook(args []string, stdout, stderr io.Writer) int {
	if os.Getenv("AI_GUARD_HOOK") == "off" {
		return 0
	}
	in := readHookInput(hookStdin)
	dir := in.Cwd
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return 0
		}
	}
	root, err := gitx.Root(dir)
	if err != nil {
		return 0
	}
	// A subagent is checked only in a tree of its own (an isolated worktree).
	// If it shares the session's tree, that tree is checked by the session's
	// Stop hook, and blocking the subagent would set it to fixing work in
	// progress that is not its own.
	if in.Event == "SubagentStop" && !ownTree(root, os.Getenv("CLAUDE_PROJECT_DIR")) {
		return 0
	}
	fp, clean, err := gitx.Fingerprint(root)
	if err != nil || clean {
		return 0
	}
	passed := filepath.Join(root, ".aiguard", "hook-last-ok")
	if last, err := os.ReadFile(passed); err == nil && string(last) == fp {
		return 0
	}

	var out, errOut bytes.Buffer
	code := cmdCheck(append(append([]string{}, args...), "--root", root), &out, &errOut)
	switch code {
	case 0:
		// The check creates .aiguard (ignored by git) whenever it has files to
		// look at; if it is not there, the next turn simply checks again.
		os.WriteFile(passed, []byte(fp), 0o644)
		return 0
	case 1:
		fmt.Fprintf(stderr, "%s\n\n%s", strings.TrimRight(out.String(), "\n"), hookNext)
		return 2
	case 4:
		var deferred []string
		for _, l := range strings.Split(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(l, "> deferred "); ok {
				deferred = append(deferred, rest)
			}
		}
		hookSay(stdout, "ai-guard left checks unvalidated this turn (they did not fit in RAM or in time):\n"+strings.Join(deferred, "\n"))
		return 0
	}
	// 2 = bad usage or config, 3 = a tool could not run: not the change's fault.
	hookSay(stdout, fmt.Sprintf("ai-guard could not complete the check (exit %d); this turn was not validated.\n%s",
		code, firstLines(out.String()+errOut.String(), 5)))
	return 0
}

// ownTree reports whether root is a different working tree from the one the
// session started in. An unknown session directory counts as the same tree.
func ownTree(root, projectDir string) bool {
	if projectDir == "" {
		return false
	}
	main, err := gitx.Root(projectDir)
	if err != nil {
		return false
	}
	a, errA := os.Stat(root)
	b, errB := os.Stat(main)
	return errA == nil && errB == nil && !os.SameFile(a, b)
}

func hookSay(w io.Writer, msg string) {
	b, _ := json.Marshal(map[string]string{"systemMessage": strings.TrimSpace(msg)})
	w.Write(b)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
