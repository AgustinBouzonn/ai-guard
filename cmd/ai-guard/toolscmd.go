package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/runner"
	"github.com/AgustinBouzonn/ai-guard/internal/tools"
)

// managedPackages is what `ai-guard tools install` fetches: the native
// TypeScript compiler, for projects that only ship the JavaScript one.
var managedPackages = []string{"@typescript/native-preview@latest"}

// resolveToolsDir picks the directory of the managed tools: $AIGUARD_TOOLS, a
// "tools" directory that travels next to the executable, or the user's cache.
func resolveToolsDir(fromEnv, exeDir, cacheDir string) string {
	if fromEnv != "" {
		return fromEnv
	}
	if exeDir != "" {
		if fi, err := os.Stat(filepath.Join(exeDir, "tools")); err == nil && fi.IsDir() {
			return filepath.Join(exeDir, "tools")
		}
	}
	if cacheDir != "" {
		return filepath.Join(cacheDir, "ai-guard", "tools")
	}
	return ""
}

func toolsDir() string {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	cache, _ := os.UserCacheDir()
	return resolveToolsDir(os.Getenv("AIGUARD_TOOLS"), exeDir, cache)
}

// toolsStatus prints what the managed directory provides.
func toolsStatus(w io.Writer, dir string) {
	fmt.Fprintf(w, "tools dir: %s\n", dir)
	env := machineEnv("")
	env.ToolsDir = dir
	tsgo, ok := tools.ManagedTSC(env)
	if !ok {
		fmt.Fprintln(w, "typescript native compiler: not installed (run: ai-guard tools install)")
		return
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "node_modules", "@typescript", "native-preview", "package.json")); err == nil {
		json.Unmarshal(data, &pkg)
	}
	if pkg.Version == "" {
		pkg.Version = "version unknown"
	}
	fmt.Fprintf(w, "typescript native compiler: %s (%s)\n", tsgo, pkg.Version)
}

// cmdTools manages tools that ai-guard can use when a project does not ship
// them. They live in their own npm prefix, never in the project.
func cmdTools(args []string, stdout, stderr io.Writer) int {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := newFlags("tools", stderr)
	dirFlag := fs.String("dir", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := *dirFlag
	if dir == "" {
		dir = toolsDir()
	}
	if dir == "" {
		fmt.Fprintln(stderr, "ai-guard: no tools directory: set AIGUARD_TOOLS or pass --dir")
		return 2
	}
	switch sub {
	case "status":
		toolsStatus(stdout, dir)
		return 0
	case "install":
	default:
		fmt.Fprintf(stderr, "ai-guard: unknown tools command %q (want: ai-guard tools [install] [--dir DIR])\n", sub)
		return 2
	}

	if _, err := exec.LookPath("npm"); err != nil {
		fmt.Fprintln(stderr, "ai-guard: npm not found on PATH: the tools are npm packages, install Node.js first")
		return 3
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "ai-guard: %v\n", err)
		return 3
	}
	// npm installs into the nearest directory that has a manifest: without one
	// here, the packages would land in whatever project encloses this directory.
	manifest := filepath.Join(dir, "package.json")
	if _, err := os.Stat(manifest); err != nil {
		if err := os.WriteFile(manifest, []byte("{\n  \"name\": \"ai-guard-tools\",\n  \"private\": true,\n  \"description\": \"tools managed by ai-guard (ai-guard tools install)\"\n}\n"), 0o644); err != nil {
			fmt.Fprintf(stderr, "ai-guard: %v\n", err)
			return 3
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Through the shell: on Windows npm is a .cmd script.
	line := "npm install --no-audit --no-fund --loglevel=error " + strings.Join(managedPackages, " ")
	r := runner.Run(ctx, runner.Spec{Shell: line, Dir: dir, Timeout: 10 * time.Minute, Stdout: stderr, Stderr: stderr})
	switch {
	case r.Err != nil:
		fmt.Fprintf(stderr, "ai-guard: cannot run npm: %v\n", r.Err)
		return 3
	case r.Killed != runner.NotKilled:
		fmt.Fprintf(stderr, "ai-guard: npm install stopped (%s) in %s\n", r.Killed, dir)
		return 3
	case r.ExitCode != 0:
		fmt.Fprintf(stderr, "ai-guard: npm install failed with exit %d in %s\n", r.ExitCode, dir)
		return 3
	}
	toolsStatus(stdout, dir)
	return 0
}
