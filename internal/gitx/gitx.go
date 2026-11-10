// Package gitx finds the files an agent has touched.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Mode selects what "changed" means.
type Mode int

const (
	Worktree Mode = iota // staged + unstaged + untracked, relative to HEAD
	Staged               // index only
	Since                // everything that differs from Ref, plus untracked
)

// Options configures Changed.
type Options struct {
	Mode Mode
	Ref  string // base ref for Since
}

// Root returns the working-tree root containing dir. It walks up looking for
// a .git entry (a directory, or a file in linked worktrees and submodules)
// instead of spawning git: this runs on every invocation.
func Root(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("gitx: %s is not inside a git repository", abs)
		}
		d = parent
	}
}

func run(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"--no-optional-locks", "-C", root, "-c", "core.quotepath=false"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func splitZ(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// Changed lists changed files as repository-relative slash paths, sorted and
// de-duplicated. Deleted paths and anything that is not a regular file
// (submodules, directories) are left out: there is nothing to check in them.
func Changed(root string, o Options) ([]string, error) {
	var paths []string
	switch o.Mode {
	case Staged:
		out, err := run(root, "diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR")
		if err != nil {
			return nil, err
		}
		paths = splitZ(out)
	case Since:
		if o.Ref == "" {
			return nil, errors.New("gitx: Since needs a ref")
		}
		base := o.Ref
		if mb, err := run(root, "merge-base", o.Ref, "HEAD"); err == nil { // branch point, not the tip of Ref
			base = strings.TrimSpace(string(mb))
		}
		out, err := run(root, "diff", "--name-only", "-z", "--diff-filter=ACMR", base)
		if err != nil {
			return nil, err
		}
		untracked, err := run(root, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return nil, err
		}
		paths = append(splitZ(out), splitZ(untracked)...)
	default:
		// One process for staged, unstaged and untracked. --no-renames keeps
		// every record to a single path.
		out, err := run(root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
		if err != nil {
			return nil, err
		}
		for _, rec := range splitZ(out) {
			if len(rec) > 3 { // "XY path"
				paths = append(paths, rec[3:])
			}
		}
	}
	seen := make(map[string]struct{}, len(paths))
	var files []string
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files, nil
}

// Tracked lists up to limit tracked files (repository-relative slash paths).
func Tracked(root string, limit int) ([]string, error) {
	out, err := run(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	files := splitZ(out)
	sort.Strings(files)
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}
