package remote

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
)

// The test binary doubles as the remote trigger command and as a fake rsync.
func TestMain(m *testing.M) {
	if out := os.Getenv("AIGUARD_RECORD"); out != "" && strings.HasPrefix(filepath.Base(os.Args[0]), "rsync") {
		os.WriteFile(out, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
		os.Exit(0)
	}
	if os.Getenv("AIGUARD_FAKE_TOOL") == "1" && len(os.Args) > 2 && os.Args[1] == "record" { // record <file> <exit> <args...>
		os.WriteFile(os.Args[2], []byte(strings.Join(os.Args[4:], "\n")), 0o644)
		os.Stdout.WriteString("remote says hi\n")
		if os.Args[3] != "0" {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo returns a working repository with one commit and a bare "origin".
func repo(t *testing.T) (root, bare string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("AIGUARD_FAKE_TOOL", "1")
	root, bare = t.TempDir(), filepath.Join(t.TempDir(), "origin.git")
	tgit(t, root, "init", "-q", "-b", "main")
	tgit(t, root, "init", "-q", "--bare", bare)
	write(t, root, ".gitignore", "ignored/\n")
	write(t, root, "tracked.txt", "v1\n")
	write(t, root, "removed.txt", "bye\n")
	write(t, root, ".aiguard/.gitignore", "*\n")
	tgit(t, root, "add", "-A")
	tgit(t, root, "commit", "-q", "-m", "init")
	tgit(t, root, "remote", "add", "origin", bare)
	tgit(t, root, "push", "-q", "origin", "main")
	return root, bare
}

func TestSnapshotCapturesTheWorkingTreeWithoutTouchingIt(t *testing.T) {
	root, _ := repo(t)
	write(t, root, "tracked.txt", "v2 unstaged\n")
	write(t, root, "staged.txt", "staged\n")
	tgit(t, root, "add", "staged.txt")
	write(t, root, "new/untracked.txt", "untracked\n")
	write(t, root, "ignored/build.out", "junk\n")
	write(t, root, ".aiguard/cache/x", "cache\n")
	os.Remove(filepath.Join(root, "removed.txt"))
	head, status := tgit(t, root, "rev-parse", "HEAD"), tgit(t, root, "status", "--porcelain")

	sha, err := Snapshot(root, "job1")
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Split(tgit(t, root, "ls-tree", "-r", "--name-only", sha), "\n")
	want := []string{".gitignore", "new/untracked.txt", "staged.txt", "tracked.txt"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("snapshot holds %q, want %q (work tree as it is: no ignored files, no state dir, deletions applied)", files, want)
	}
	if got := tgit(t, root, "show", sha+":tracked.txt"); got != "v2 unstaged" {
		t.Errorf("snapshot has tracked.txt = %q, want the unstaged work-tree content", got)
	}
	if parent := tgit(t, root, "rev-parse", sha+"^"); parent != head {
		t.Errorf("snapshot parent = %s, want HEAD %s (so the push is a small delta)", parent, head)
	}
	if got := tgit(t, root, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved from %s to %s", head, got)
	}
	if got := tgit(t, root, "status", "--porcelain"); got != status {
		t.Errorf("index or work tree changed:\nbefore:\n%s\nafter:\n%s", status, got)
	}
}

func queued(t *testing.T, root string) Job {
	t.Helper()
	job, err := Queue(root, "deferred: ts.types", func(string) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestExecuteGitRefPushesSnapshotAndRunsCommandWithPlaceholders(t *testing.T) {
	root, bare := repo(t)
	write(t, root, "feature.txt", "work in progress\n")
	args := filepath.Join(t.TempDir(), "args")
	cfg := config.Default().Tier2
	cfg.Command = []string{os.Args[0], "record", args, "0", "id={id}", "{sha}", "{ref}", "{branch}", "{root}"}
	job := queued(t, root)

	var log bytes.Buffer
	done, err := Execute(context.Background(), root, cfg, job.ID, &log)
	if err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", err, log.String())
	}
	if done.State != "done" || done.ExitCode == nil || *done.ExitCode != 0 || len(done.SHA) != 40 {
		t.Fatalf("job = %+v, want state done, exit 0 and a snapshot sha", done)
	}
	ref := "refs/aiguard/" + job.ID
	if got := tgit(t, bare, "rev-parse", ref); got != done.SHA {
		t.Errorf("remote %s = %s, want the snapshot %s", ref, got, done.SHA)
	}
	if got := tgit(t, bare, "show", done.SHA+":feature.txt"); got != "work in progress" {
		t.Errorf("remote snapshot lacks the uncommitted file: %q", got)
	}
	recorded, _ := os.ReadFile(args)
	if want := strings.Join([]string{"id=" + job.ID, done.SHA, ref, "main", root}, "\n"); string(recorded) != want {
		t.Errorf("command received:\n%s\nwant:\n%s", recorded, want)
	}
	if !strings.Contains(log.String(), "remote says hi") {
		t.Errorf("log does not carry the command's output:\n%s", log.String())
	}
	if out, err := exec.Command("git", "-C", root, "rev-parse", "-q", "--verify", ref).Output(); err == nil {
		t.Errorf("local ref %s left behind (%s)", ref, strings.TrimSpace(string(out)))
	}
	if onDisk, _ := Load(root, job.ID); onDisk.State != "done" {
		t.Errorf("state file says %q, want done", onDisk.State)
	}
}

func TestExecuteRecordsCommandFailure(t *testing.T) {
	root, _ := repo(t)
	cfg := config.Default().Tier2
	cfg.Command = []string{os.Args[0], "record", filepath.Join(t.TempDir(), "args"), "3"}
	job := queued(t, root)
	done, err := Execute(context.Background(), root, cfg, job.ID, &bytes.Buffer{})
	if err == nil || done.State != "failed" || done.ExitCode == nil || *done.ExitCode != 3 {
		t.Errorf("err = %v, job = %+v; want state failed with exit code 3", err, done)
	}
}

func TestExecuteDoesNotRunTheCommandWhenSyncFails(t *testing.T) {
	root, _ := repo(t)
	args := filepath.Join(t.TempDir(), "args")
	cfg := config.Default().Tier2
	cfg.Remote = "nowhere"
	cfg.Command = []string{os.Args[0], "record", args, "0"}
	job := queued(t, root)
	done, err := Execute(context.Background(), root, cfg, job.ID, &bytes.Buffer{})
	if err == nil || done.State != "failed" || !strings.Contains(done.Error, "push") {
		t.Errorf("err = %v, job = %+v; want a failed job blaming the push", err, done)
	}
	if _, statErr := os.Stat(args); statErr == nil {
		t.Error("the remote command ran although nothing was synced")
	}
}

func TestExecuteWithoutACommandOnlySyncs(t *testing.T) {
	root, bare := repo(t)
	job := queued(t, root)
	done, err := Execute(context.Background(), root, config.Default().Tier2, job.ID, &bytes.Buffer{})
	if err != nil || done.State != "done" {
		t.Fatalf("err = %v, job = %+v", err, done)
	}
	if got := tgit(t, bare, "rev-parse", "refs/aiguard/"+job.ID); got != done.SHA {
		t.Errorf("remote ref = %s, want %s: a server-side hook or poller can pick it up", got, done.SHA)
	}
}

func TestExecuteRsyncSendsTheTreeHonouringGitignore(t *testing.T) {
	root, _ := repo(t)
	bin := t.TempDir()
	name := "rsync"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	self, _ := os.ReadFile(os.Args[0])
	if err := os.WriteFile(filepath.Join(bin, name), self, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(t.TempDir(), "rsync-args")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIGUARD_RECORD", rec)

	cfg := config.Default().Tier2
	cfg.Sync, cfg.RsyncDest, cfg.RsyncArgs = "rsync", "ci@build:/srv/repo", []string{"--compress-level=3"}
	job := queued(t, root)
	done, err := Execute(context.Background(), root, cfg, job.ID, &bytes.Buffer{})
	if err != nil || done.State != "done" {
		t.Fatalf("err = %v, job = %+v", err, done)
	}
	got, _ := os.ReadFile(rec)
	want := "-az\n--delete\n--exclude=.git\n--filter=:- .gitignore\n--compress-level=3\n./\nci@build:/srv/repo/"
	if string(got) != want {
		t.Errorf("rsync argv:\n%s\nwant:\n%s", got, want)
	}
}

func TestQueueCreatesAJobAndStartsItsWorker(t *testing.T) {
	root, _ := repo(t)
	var started string
	job, err := Queue(root, "requested", func(id string) (int, error) { started = id; return 4242, nil })
	if err != nil {
		t.Fatal(err)
	}
	if started != job.ID || job.State != "queued" || job.Log != ".aiguard/tier2/"+job.ID+".log" {
		t.Errorf("job = %+v, worker started for %q", job, started)
	}
	if onDisk, _ := Load(root, job.ID); onDisk.Pid != 4242 {
		t.Errorf("stored worker pid = %d, want 4242 (needed to notice a dead worker)", onDisk.Pid)
	}
	second, _ := Queue(root, "requested", func(string) (int, error) { return 0, nil })
	jobs, err := List(root)
	if err != nil || len(jobs) != 2 || jobs[0].ID != second.ID || jobs[1].ID != job.ID {
		t.Errorf("List = %+v (%v), want both jobs, newest first", jobs, err)
	}
	if _, err := Load(root, "nope"); err == nil {
		t.Error("Load invented a job that does not exist")
	}
}

func TestSnapshotSeesAnEditMadeInTheSameSecondAsTheLastCommit(t *testing.T) {
	root, _ := repo(t)
	tgit(t, root, "config", "core.trustctime", "false") // as on machines where scanners touch ctime
	// An agent rewrites a tracked file right after a commit: same size, and a
	// modification time in the same second as the one git recorded. Git itself
	// still notices, because the index is no older than the file. A snapshot
	// built on a later copy of that index must notice too.
	file, index := filepath.Join(root, "src", "a.py"), filepath.Join(root, ".git", "index")
	moment := time.Now().Add(-time.Hour).Truncate(time.Second)
	write(t, root, "src/a.py", "x = 1\n")
	os.Chtimes(file, moment, moment)
	tgit(t, root, "add", "-A")
	tgit(t, root, "commit", "-q", "-m", "tracked")
	os.Chtimes(index, moment, moment)
	write(t, root, "src/a.py", "x = 2\n")
	os.Chtimes(file, moment, moment)
	// --no-optional-locks: a plain status would rewrite the index and, with it, repair the timestamps.
	if st := tgit(t, root, "--no-optional-locks", "status", "--porcelain"); !strings.Contains(st, "src/a.py") {
		t.Fatalf("precondition: git status does not see the edit:\n%q", st)
	}

	sha, err := Snapshot(root, "j-racy")
	if err != nil {
		t.Fatal(err)
	}
	if got := tgit(t, root, "show", sha+":src/a.py"); got != "x = 2" {
		t.Errorf("snapshot has src/a.py = %q, want the edit \"x = 2\"", got)
	}
}
