package gitx

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo creates a repository with one commit on branch main.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "src/keep.ts", "export const a = 1;\n")
	write(t, root, "src/edit.ts", "export const b = 1;\n")
	write(t, root, "src/gone.py", "x = 1\n")
	write(t, root, "src/old_name.go", "package src\n")
	write(t, root, ".gitignore", "build/\n*.log\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")
	return root
}

func TestChangedListsModifiedStagedAndUntrackedButNotDeletedOrIgnored(t *testing.T) {
	root := repo(t)
	write(t, root, "src/edit.ts", "export const b = 2;\n") // modified, unstaged
	write(t, root, "src/staged.py", "y = 1\n")             // new, staged
	git(t, root, "add", "src/staged.py")
	write(t, root, "new dir/ñandú file.rs", "fn main() {}\n") // untracked, awkward name
	os.Remove(filepath.Join(root, "src", "gone.py"))          // deleted
	git(t, root, "mv", "src/old_name.go", "src/new_name.go")  // renamed
	write(t, root, "build/out.ts", "ignored\n")               // ignored dir
	write(t, root, "debug.log", "ignored\n")                  // ignored pattern

	got, err := Changed(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new dir/ñandú file.rs", "src/edit.ts", "src/new_name.go", "src/staged.py"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changed =\n  %q\nwant\n  %q", got, want)
	}
}

func TestChangedStagedOnlyIgnoresUnstagedEdits(t *testing.T) {
	root := repo(t)
	write(t, root, "src/edit.ts", "export const b = 2;\n")
	write(t, root, "src/staged.py", "y = 1\n")
	git(t, root, "add", "src/staged.py")
	write(t, root, "untracked.go", "package x\n")

	got, err := Changed(root, Options{Mode: Staged})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/staged.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Changed(staged) = %q, want %q", got, want)
	}
}

func TestChangedSinceRefCoversCommitsAndWorkingTree(t *testing.T) {
	root := repo(t)
	git(t, root, "checkout", "-q", "-b", "feature")
	write(t, root, "src/committed.ts", "export const c = 1;\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "feature work")
	write(t, root, "src/edit.ts", "export const b = 3;\n") // uncommitted
	write(t, root, "fresh.py", "z = 1\n")                  // untracked

	got, err := Changed(root, Options{Mode: Since, Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fresh.py", "src/committed.ts", "src/edit.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changed(since main) = %q, want %q", got, want)
	}
}

func TestChangedWorksBeforeTheFirstCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, root, "a.py", "x = 1\n")
	got, err := Changed(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.py"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Changed = %q, want %q", got, want)
	}
}

func TestChangedReportsGitFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := Changed(t.TempDir(), Options{}); err == nil {
		t.Error("Changed succeeded outside a git repository")
	}
}

func TestRootFindsRepositoryFromNestedDirectory(t *testing.T) {
	root := repo(t)
	nested := filepath.Join(root, "src", "deep", "er")
	os.MkdirAll(nested, 0o755)
	got, err := Root(nested)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, _ := filepath.EvalSymlinks(root)
	gotReal, _ := filepath.EvalSymlinks(got)
	if gotReal != wantReal {
		t.Errorf("Root = %q, want %q", got, root)
	}
}

func TestRootTreatsGitFileAsWorktreeRoot(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644)
	sub := filepath.Join(dir, "pkg")
	os.MkdirAll(sub, 0o755)
	got, err := Root(sub)
	if err != nil || got != dir {
		t.Errorf("Root = %q, %v; want %q (linked worktrees have a .git file)", got, err, dir)
	}
}

func TestRootFailsOutsideARepository(t *testing.T) {
	if _, err := Root(t.TempDir()); err == nil {
		t.Error("Root succeeded in a directory with no .git above it")
	}
}

func TestTrackedListsCommittedFilesUpToALimit(t *testing.T) {
	root := repo(t)
	write(t, root, "untracked.txt", "x")
	got, err := Tracked(root, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "src/edit.ts", "src/gone.py", "src/keep.ts", "src/old_name.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tracked = %q, want %q", got, want)
	}
	if got, _ := Tracked(root, 2); len(got) != 2 {
		t.Errorf("Tracked(limit 2) returned %d files", len(got))
	}
}

func TestChangedNeverRewritesTheIndex(t *testing.T) {
	// A background tool must not take git's locks: an agent may be committing
	// at the same moment. Plain `git status` refreshes and rewrites the index
	// when a file's timestamps changed; ours has to leave it byte-for-byte alone.
	root := repo(t)
	index := filepath.Join(root, ".git", "index")
	later := time.Now().Add(90 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "src", "keep.ts"), later, later); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Changed(root, Options{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(index)
	if !bytes.Equal(before, after) {
		t.Error("Changed rewrote .git/index (it must run git with --no-optional-locks)")
	}
}
