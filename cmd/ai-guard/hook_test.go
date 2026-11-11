package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hook runs `ai-guard hook` the way Claude Code does: the event as JSON on stdin.
func hook(t *testing.T, event, cwd string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"hook_event_name": event, "cwd": cwd})
	hookStdin = bytes.NewReader(in)
	t.Cleanup(func() { hookStdin = os.Stdin })
	return cli(t, append([]string{"hook"}, args...)...)
}

// hookEnv keeps the session this test may itself be running in out of the picture.
func hookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("AI_GUARD_HOOK", "")
}

// hookRepo is a committed repository with one Python file and the given
// aiguard.json. The config is hidden from git, so a test can swap it without
// changing what the working tree looks like.
func hookRepo(t *testing.T, cfg map[string]any) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	hookEnv(t)
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, ".git/info/exclude", "aiguard.json\n")
	setHookConfig(t, root, cfg)
	write(t, root, "app/x.py", "x = 1\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	return root
}

func setHookConfig(t *testing.T, root string, cfg map[string]any) {
	t.Helper()
	cfg["python"] = map[string]string{"lint": "off", "types": "off", "tests": "off"}
	b, _ := json.Marshal(cfg)
	write(t, root, "aiguard.json", string(b))
}

// lintThatSays is a tier-0 step that passes, or fails with the given output.
func lintThatSays(failWith string) map[string]any {
	code, text := "0", "clean"
	if failWith != "" {
		code, text = "1", failWith
	}
	return map[string]any{"steps": []map[string]any{
		{"id": "fake.lint", "tier": 0, "langs": []string{"py"}, "cmd": []string{os.Args[0], "say", code, text}, "parser": "generic"},
	}}
}

func TestHookIsSilentWhenNothingChanged(t *testing.T) {
	root := hookRepo(t, lintThatSays("app/x.py:1:5: error: bad thing"))
	code, out, errOut := hook(t, "Stop", root)
	if code != 0 || out != "" || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want a silent exit 0 on a clean tree", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".aiguard")); err == nil {
		t.Error("the hook created .aiguard although there was nothing to check")
	}
}

func TestHookBlocksAndHandsTheReportToTheAgent(t *testing.T) {
	root := hookRepo(t, lintThatSays("app/x.py:1:5: error: bad thing"))
	write(t, root, "app/x.py", "x = 2\n")
	code, out, errOut := hook(t, "Stop", root)
	// Claude Code blocks on exit 2 and gives stderr to the model.
	if code != 2 {
		t.Fatalf("exit %d, want 2; stdout %q, stderr %q", code, out, errOut)
	}
	if out != "" {
		t.Errorf("stdout %q, want nothing: the report belongs on stderr", out)
	}
	if !strings.HasPrefix(errOut, "FAIL t0 ") || !strings.Contains(errOut, "app/x.py\n  1:5 bad thing\n") {
		t.Errorf("stderr does not carry the compact report:\n%s", errOut)
	}
	if !strings.Contains(errOut, "ai-guard check") {
		t.Errorf("stderr does not tell the agent what to do next:\n%s", errOut)
	}
	// A failure is not remembered: the same delta is checked again next turn.
	if code, _, _ := hook(t, "Stop", root); code != 2 {
		t.Errorf("second run on the same failing delta: exit %d, want 2", code)
	}
}

func TestHookSkipsADeltaThatAlreadyPassed(t *testing.T) {
	root := hookRepo(t, lintThatSays(""))
	write(t, root, "app/x.py", "x = 2\n")
	if code, out, errOut := hook(t, "Stop", root); code != 0 || out != "" || errOut != "" {
		t.Fatalf("passing delta: exit %d, stdout %q, stderr %q; want a silent exit 0", code, out, errOut)
	}
	// The check would fail now, but the tree is exactly what passed: it must not run.
	setHookConfig(t, root, lintThatSays("app/x.py:1:5: error: bad thing"))
	if code, _, errOut := hook(t, "Stop", root); code != 0 {
		t.Fatalf("unchanged delta: exit %d, want 0 without running the check; stderr:\n%s", code, errOut)
	}
	// One more edit and it is a new delta.
	write(t, root, "app/x.py", "x = 3\n")
	if code, _, _ := hook(t, "Stop", root); code != 2 {
		t.Errorf("edited delta: exit %d, want 2", code)
	}
}

func TestHookForASubagentOnlyActsInItsOwnWorktree(t *testing.T) {
	root := hookRepo(t, lintThatSays("app/x.py:1:5: error: bad thing"))
	write(t, root, "app/x.py", "x = 2\n")

	// Same tree as the session: its own Stop hook checks it. Blocking the
	// subagent here would set it to fixing work in progress that is not its own.
	t.Setenv("CLAUDE_PROJECT_DIR", root)
	if code, _, errOut := hook(t, "SubagentStop", root); code != 0 || errOut != "" {
		t.Errorf("subagent in the session's tree: exit %d, stderr %q; want a silent exit 0", code, errOut)
	}
	// Without the session's directory there is no telling whose tree this is.
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if code, _, _ := hook(t, "SubagentStop", root); code != 0 {
		t.Errorf("subagent, session directory unknown: exit %d, want 0", code)
	}
	// A tree of its own (an isolated worktree) is the subagent's to fix.
	other := t.TempDir()
	git(t, other, "init", "-q", "-b", "main")
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	if code, _, _ := hook(t, "SubagentStop", root); code != 2 {
		t.Errorf("subagent in its own tree: exit %d, want 2", code)
	}
	// The main agent is always checked.
	t.Setenv("CLAUDE_PROJECT_DIR", root)
	if code, _, _ := hook(t, "Stop", root); code != 2 {
		t.Errorf("main agent: exit %d, want 2", code)
	}
}

func TestHookTellsThePersonAboutDeferredChecks(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no RAM admission on this platform")
	}
	root := hookRepo(t, map[string]any{
		"queue_wait_ms": 50,
		"steps": []map[string]any{
			{"id": "fake.types", "tier": 1, "phase": 0, "langs": []string{"py"}, "cmd": []string{os.Args[0], "say", "0", "clean"}, "parser": "generic", "mem_mb": 100000000},
		},
	})
	write(t, root, "app/x.py", "x = 2\n")
	code, out, errOut := hook(t, "Stop", root)
	// Nothing failed, so the agent is not blocked; the person learns what was not checked.
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want exit 0 and nothing for the agent", code, errOut)
	}
	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatalf("stdout is not the hook's JSON: %v\n%s", err, out)
	}
	if !strings.Contains(msg.SystemMessage, "fake.types: no free RAM") {
		t.Errorf("systemMessage = %q, want the deferred step and why", msg.SystemMessage)
	}
	// A deferred run is not remembered either: next turn the RAM may be there.
	if _, err := os.Stat(filepath.Join(root, ".aiguard", "hook-last-ok")); err == nil {
		t.Error("a deferred run was recorded as passed")
	}
}

func TestHookReportsABrokenToolWithoutBlocking(t *testing.T) {
	root := hookRepo(t, map[string]any{"steps": []map[string]any{
		{"id": "fake.lint", "tier": 0, "langs": []string{"py"}, "cmd": []string{filepath.Join(t.TempDir(), "no-such-tool")}, "parser": "generic"},
	}})
	write(t, root, "app/x.py", "x = 2\n")
	code, out, errOut := hook(t, "Stop", root)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want exit 0: a broken tool is not an error in the change", code, errOut)
	}
	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if err := json.Unmarshal([]byte(out), &msg); err != nil || !strings.Contains(msg.SystemMessage, "fake.lint") {
		t.Errorf("stdout %q (%v), want a systemMessage naming the step that could not run", out, err)
	}
}

func TestHookDoesNothingOutsideARepositoryOrWhenSwitchedOff(t *testing.T) {
	root := hookRepo(t, lintThatSays("app/x.py:1:5: error: bad thing"))
	write(t, root, "app/x.py", "x = 2\n")

	if code, out, errOut := hook(t, "Stop", t.TempDir()); code != 0 || out != "" || errOut != "" {
		t.Errorf("outside a repository: exit %d, stdout %q, stderr %q; want a silent exit 0", code, out, errOut)
	}
	t.Setenv("AI_GUARD_HOOK", "off")
	if code, out, errOut := hook(t, "Stop", root); code != 0 || out != "" || errOut != "" {
		t.Errorf("AI_GUARD_HOOK=off: exit %d, stdout %q, stderr %q; want a silent exit 0", code, out, errOut)
	}
}

func TestHookPassesCheckFlagsThrough(t *testing.T) {
	root := hookRepo(t, lintThatSays("app/x.py:1:5: error: bad thing"))
	write(t, root, "app/x.py", "x = 2\n")
	if code, _, errOut := hook(t, "Stop", root, "--disable", "fake.lint"); code != 0 {
		t.Errorf("hook --disable fake.lint: exit %d, want 0; stderr:\n%s", code, errOut)
	}
}

func TestHookInputToleratesAnythingOnStdin(t *testing.T) {
	for _, in := range []string{"", "not json", "null", `"text"`, "[1,2]"} {
		if got := readHookInput(strings.NewReader(in)); got != (hookInput{}) {
			t.Errorf("readHookInput(%q) = %+v, want the zero value", in, got)
		}
	}
	got := readHookInput(strings.NewReader(`{"hook_event_name":"SubagentStop","cwd":"/repo","session_id":"x","stop_hook_active":true}`))
	if got.Event != "SubagentStop" || got.Cwd != "/repo" {
		t.Errorf("readHookInput = %+v, want the event and the directory", got)
	}
}
