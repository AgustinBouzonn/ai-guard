// Package remote implements tier 2: ship the working tree elsewhere and
// trigger the heavy pipeline there (full build, link, packaging, E2E), so
// none of it runs on the workstation.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AgustinBouzonn/ai-guard/internal/config"
	"github.com/AgustinBouzonn/ai-guard/internal/runner"
)

// Job is the persisted state of one tier-2 run.
type Job struct {
	ID       string     `json:"id"`
	State    string     `json:"state"` // queued | syncing | running | done | failed
	Reason   string     `json:"reason"`
	SHA      string     `json:"sha,omitempty"`
	Ref      string     `json:"ref,omitempty"`
	Branch   string     `json:"branch,omitempty"`
	Created  time.Time  `json:"created"`
	Finished *time.Time `json:"finished,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Error    string     `json:"error,omitempty"`
	Log      string     `json:"log"` // repository-relative
	Pid      int        `json:"-"`   // worker process, kept in <id>.pid
}

const keepJobs = 20

// Dir is where job state and logs live.
func Dir(root string) string { return filepath.Join(root, ".aiguard", "tier2") }

func statePath(root, id string) string { return filepath.Join(Dir(root), id+".json") }
func pidPath(root, id string) string   { return filepath.Join(Dir(root), id+".pid") }

func save(root string, j Job) error {
	b, _ := json.MarshalIndent(j, "", "  ")
	tmp := statePath(root, j.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(root, j.ID)) // readers never see a half-written file
}

// Load reads one job.
func Load(root, id string) (Job, error) {
	var j Job
	b, err := os.ReadFile(statePath(root, id))
	if err != nil {
		return j, fmt.Errorf("tier2: no job %q", id)
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	// The worker pid lives in its own file: the worker rewrites the state
	// file concurrently and must not race with whoever launched it.
	if pid, err := os.ReadFile(pidPath(root, id)); err == nil {
		j.Pid, _ = strconv.Atoi(strings.TrimSpace(string(pid)))
	}
	return j, nil
}

// List returns all known jobs, newest first.
func List(root string) ([]Job, error) {
	names, err := filepath.Glob(filepath.Join(Dir(root), "*.json"))
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, n := range names {
		if j, err := Load(root, strings.TrimSuffix(filepath.Base(n), ".json")); err == nil {
			jobs = append(jobs, j)
		}
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID > jobs[b].ID })
	return jobs, nil
}

// newID sorts chronologically: UTC timestamp to the microsecond plus entropy.
func newID() string {
	var r [3]byte
	rand.Read(r[:])
	now := time.Now().UTC()
	return fmt.Sprintf("%s%06d-%s", now.Format("20060102-150405"), now.Nanosecond()/1000, hex.EncodeToString(r[:]))
}

// Queue records a job and starts its worker through spawn. It returns at
// once: the sync and the remote command run in the worker.
func Queue(root, reason string, spawn func(id string) (pid int, err error)) (Job, error) {
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return Job{}, err
	}
	if old, _ := List(root); len(old) >= keepJobs {
		for _, j := range old[keepJobs-1:] {
			os.Remove(statePath(root, j.ID))
			os.Remove(pidPath(root, j.ID))
			os.Remove(filepath.Join(Dir(root), j.ID+".log"))
		}
	}
	id := newID()
	j := Job{ID: id, State: "queued", Reason: reason, Created: time.Now().UTC(), Log: ".aiguard/tier2/" + id + ".log"}
	if err := save(root, j); err != nil {
		return j, err
	}
	pid, err := spawn(id)
	if err != nil {
		j.State, j.Error = "failed", "cannot start worker: "+err.Error()
		save(root, j)
		return j, err
	}
	j.Pid = pid
	return j, os.WriteFile(pidPath(root, id), []byte(strconv.Itoa(pid)), 0o644)
}

// Fail marks a job that can no longer finish (its worker is gone).
func Fail(root string, j Job, why string) Job {
	now := time.Now().UTC()
	j.State, j.Error, j.Finished = "failed", why, &now
	save(root, j)
	return j
}

// Final reports whether the job has reached a terminal state.
func (j Job) Final() bool { return j.State == "done" || j.State == "failed" }

func git(ctx context.Context, root string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// Snapshot commits the working tree exactly as it is (staged, unstaged and
// untracked files, minus ignored ones) as a child of HEAD and returns the
// commit. It works on a private copy of the index, so the user's index, HEAD
// and files are untouched.
func Snapshot(root, id string) (string, error) {
	ctx := context.Background()
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return "", err
	}
	index := filepath.Join(Dir(root), id+".index")
	defer os.Remove(index)
	if real, err := git(ctx, root, nil, "rev-parse", "--git-path", "index"); err == nil {
		if !filepath.IsAbs(real) {
			real = filepath.Join(root, real)
		}
		// Start from the stat cache, so that only changed files are hashed. Git
		// trusts a cached entry only if the index file is newer than the entry
		// ("racy git"); a copy written now would be newer than every entry, and
		// an edit made in the same second as the last commit would go unseen.
		// The copy therefore keeps the age of the original.
		if fi, err := os.Stat(real); err == nil {
			if data, err := os.ReadFile(real); err == nil && os.WriteFile(index, data, 0o644) == nil {
				if os.Chtimes(index, fi.ModTime(), fi.ModTime()) != nil {
					os.Remove(index) // cannot vouch for the cache: hash everything instead
				}
			}
		}
	}
	env := []string{"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=ai-guard", "GIT_AUTHOR_EMAIL=ai-guard@localhost",
		"GIT_COMMITTER_NAME=ai-guard", "GIT_COMMITTER_EMAIL=ai-guard@localhost"}
	if _, err := git(ctx, root, env, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(ctx, root, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", "ai-guard snapshot " + id}
	if _, err := git(ctx, root, nil, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		args = append(args, "-p", "HEAD")
	}
	return git(ctx, root, env, args...)
}

func expand(s string, j Job, root string) string {
	return strings.NewReplacer("{id}", j.ID, "{sha}", j.SHA, "{ref}", j.Ref, "{branch}", j.Branch, "{root}", root).Replace(s)
}

// Execute is the worker: sync the tree, run the remote trigger, record the
// outcome. Command output streams to log.
func Execute(ctx context.Context, root string, cfg config.Tier2, id string, log io.Writer) (Job, error) {
	j, err := Load(root, id)
	if err != nil {
		return j, err
	}
	fail := func(err error) (Job, error) {
		now := time.Now().UTC()
		j.State, j.Error, j.Finished = "failed", err.Error(), &now
		fmt.Fprintf(log, "[ai-guard] failed: %v\n", err)
		save(root, j)
		return j, err
	}
	j.State = "syncing"
	j.Branch, _ = git(ctx, root, nil, "rev-parse", "--abbrev-ref", "HEAD")
	save(root, j)

	switch cfg.Sync {
	case "git-ref":
		sha, err := Snapshot(root, id)
		if err != nil {
			return fail(fmt.Errorf("snapshot: %w", err))
		}
		j.SHA, j.Ref = sha, strings.TrimSuffix(cfg.RefPrefix, "/")+"/"+id
		if _, err := git(ctx, root, nil, "update-ref", j.Ref, sha); err != nil {
			return fail(err)
		}
		// Only objects the remote lacks travel: the snapshot's parent is HEAD.
		_, err = git(ctx, root, nil, "push", "--quiet", cfg.Remote, j.Ref+":"+j.Ref)
		git(ctx, root, nil, "update-ref", "-d", j.Ref) // the remote keeps it; do not litter local refs
		if err != nil {
			return fail(fmt.Errorf("push to %s: %w", cfg.Remote, err))
		}
		fmt.Fprintf(log, "[ai-guard] pushed %s to %s as %s\n", sha, cfg.Remote, j.Ref)
	case "rsync":
		if cfg.RsyncDest == "" {
			return fail(errors.New("tier2.rsync_dest is not set"))
		}
		args := append([]string{"-az", "--delete", "--exclude=.git", "--filter=:- .gitignore"}, cfg.RsyncArgs...)
		args = append(args, "./", strings.TrimSuffix(cfg.RsyncDest, "/")+"/")
		cmd := exec.CommandContext(ctx, "rsync", args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, log, log
		if err := cmd.Run(); err != nil {
			return fail(fmt.Errorf("rsync to %s: %w", cfg.RsyncDest, err))
		}
		fmt.Fprintf(log, "[ai-guard] rsynced to %s\n", cfg.RsyncDest)
	}

	j.State = "running"
	save(root, j)
	code := 0
	var cmd *exec.Cmd
	switch {
	case cfg.Shell != "":
		cmd = runner.ShellCommand(expand(cfg.Shell, j, root))
	case len(cfg.Command) > 0:
		argv := make([]string, len(cfg.Command))
		for i, a := range cfg.Command {
			argv[i] = expand(a, j, root)
		}
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	if cmd != nil {
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, log, log
		if err := cmd.Start(); err != nil {
			return fail(fmt.Errorf("remote command: %w", err))
		}
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
			<-waited
			return fail(ctx.Err())
		case err := <-waited:
			var exit *exec.ExitError
			switch {
			case errors.As(err, &exit):
				code = exit.ExitCode()
			case err != nil:
				return fail(fmt.Errorf("remote command: %w", err))
			}
		}
	}
	now := time.Now().UTC()
	j.Finished, j.ExitCode = &now, &code
	if code != 0 {
		j.State, j.Error = "failed", fmt.Sprintf("remote command exited %d", code)
		save(root, j)
		return j, errors.New(j.Error)
	}
	j.State = "done"
	return j, save(root, j)
}
