package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dreaagent/drea/internal/agent"
	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/ui"
	"github.com/dreaagent/drea/internal/vcs"
)

// --worktree isolates an attempt: the agent works in a scratch checkout on its
// own branch, so a change that goes wrong cannot damage the repository it was
// asked to improve. What happens to the result afterwards is decided by
// evidence: without --promote the user is simply told where the work is, and
// with it the work is merged only when the verification command says it is
// good and the merge is a clean fast-forward.

// enterWorktree creates a scratch worktree of cfg.Workdir and repoints the
// config at it. Every later consumer of Workdir (the tools, the agent, the
// session file) then sees the isolated copy and nothing else needs to know.
func enterWorktree(cfg *config.Config, u *ui.UI) (vcs.Worktree, error) {
	ctx := context.Background()
	repo := cfg.Workdir

	if !vcs.IsRepo(ctx, repo) {
		return vcs.Worktree{}, fmt.Errorf("--worktree needs a git repository: %s is not one", repo)
	}
	if !vcs.HasCommits(ctx, repo) {
		return vcs.Worktree{}, fmt.Errorf("--worktree needs a commit to branch from; commit a baseline in %s first", repo)
	}
	// A worktree starts from HEAD, so uncommitted work is not carried over.
	// Say so rather than letting the agent silently work from a state the user
	// is not looking at.
	if dirty, err := vcs.Dirty(ctx, repo); err == nil && dirty {
		u.Warn("uncommitted changes in " + repo + " are not carried into the worktree (it starts from HEAD)")
	}

	dir, err := worktreeDir(repo)
	if err != nil {
		return vcs.Worktree{}, err
	}
	branch := "drea/" + filepath.Base(dir)

	wt, err := vcs.AddWorktree(ctx, repo, dir, branch)
	if err != nil {
		return vcs.Worktree{}, fmt.Errorf("could not create worktree: %w", err)
	}
	cfg.Workdir = wt.Path
	if err := cfg.Normalize(); err != nil {
		return wt, err
	}
	u.Info(fmt.Sprintf("isolated in worktree %s (branch %s)", wt.Path, wt.Branch))
	return wt, nil
}

// worktreeDir returns a fresh scratch directory outside the repository, so the
// repository being worked on stays clean even while an attempt is running.
func worktreeDir(repo string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	root := filepath.Join(base, "drea", "worktrees")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d", filepath.Base(repo), time.Now().Unix())
	return filepath.Join(root, name), nil
}

// finishWorktree runs when the session ends. An untouched worktree is removed;
// work that passed the measure is promoted when asked for; anything else is
// kept and reported, because discarding an attempt the user has not seen would
// be worse than leaving a directory behind.
func finishWorktree(wt vcs.Worktree, cfg *config.Config, objective agent.Measure, u *ui.UI) {
	if wt.Path == "" {
		return
	}
	ctx := context.Background()
	changed, err := wt.Changed(ctx)
	if err == nil && !changed {
		if err := wt.Remove(ctx); err == nil {
			u.Info("worktree was unchanged; removed it")
			return
		}
	}
	if cfg.Promote && promote(ctx, wt, cfg, objective, u) {
		return
	}
	u.Info("work left in the worktree: " + wt.Path)
	u.Info("  review:  git -C " + wt.Path + " status")
	u.Info("  keep it: commit in the worktree, then `git -C " + wt.Repo + " merge " + wt.Branch + "`")
	u.Info("  discard: git -C " + wt.Repo + " worktree remove --force " + wt.Path +
		" && git -C " + wt.Repo + " branch -D " + wt.Branch)
}

// promote merges the worktree back into the repository it came from, and only
// on terms that can be checked: there is a verification command, it passed, and
// the repository can fast-forward to the branch. Anything else — no measure, a
// failing one, a repository that moved or has uncommitted changes — is a
// judgement call, and is handed back to the user with the reason.
func promote(ctx context.Context, wt vcs.Worktree, cfg *config.Config, objective agent.Measure, u *ui.UI) bool {
	switch {
	case cfg.Verify == "":
		u.Warn("not promoting: --promote needs --verify, since nothing else says the work is good")
		return false
	case objective != agent.MeasurePassing:
		u.Warn("not promoting: the verification command is not passing")
		return false
	}
	if _, _, err := vcs.Checkpoint(ctx, wt.Path, "drea: work from "+wt.Branch); err != nil {
		u.Warn("not promoting: could not commit the work: " + err.Error())
		return false
	}
	if err := vcs.MergeFFOnly(ctx, wt.Repo, wt.Branch); err != nil {
		u.Warn("not promoting: " + err.Error())
		return false
	}
	u.Info("verification passed; fast-forwarded " + wt.Repo + " to " + wt.Branch)
	if err := wt.Remove(ctx); err != nil {
		u.Info("worktree left at " + wt.Path + " (" + err.Error() + ")")
	}
	return true
}
