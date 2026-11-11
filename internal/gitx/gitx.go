// Package gitx finds the files an agent has touched.
package gitx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// Fingerprint identifies the uncommitted state of the working tree: what
// differs from HEAD, staged or not, plus every untracked file. Two calls
// return the same value only if nothing was edited in between, so a check
// that passed for a fingerprint need not run again. clean is true when there
// is nothing uncommitted at all.
func Fingerprint(root string) (fp string, clean bool, err error) {
	status, err := run(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", false, err
	}
	if len(status) == 0 {
		return "", true, nil
	}
	h := sha256.New()
	part := func(b []byte) { // length-prefixed, so parts cannot run into each other
		fmt.Fprintf(h, "%d\x00", len(b))
		h.Write(b)
	}
	part(status)
	// Content of everything tracked. No HEAD yet (a repository without
	// commits) leaves this empty: the status above still covers the names.
	diff, _ := run(root, "diff", "HEAD", "--no-ext-diff")
	part(diff)
	untracked, _ := run(root, "ls-files", "--others", "--exclude-standard", "-z")
	for _, p := range splitZ(untracked) {
		// Size and modification time stand in for the content of files git does not track.
		if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			part([]byte(fmt.Sprintf("%s\x00%d\x00%d", p, fi.Size(), fi.ModTime().UnixNano())))
		} else {
			part([]byte(p))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), false, nil
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
