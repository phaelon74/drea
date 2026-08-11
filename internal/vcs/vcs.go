// Package vcs wraps the small set of git operations the harness itself needs:
// checking whether a workspace is a repository, taking checkpoints, rolling
// back, and creating scratch worktrees for isolated attempts.
//
// It is separate from the git *tools* (internal/tool) — those exist for the
// model to call, this one is for the harness's own bookkeeping — but both go
// through git with an explicit argument vector, never a shell string, so no
// caller-supplied value can be reinterpreted as a command.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dreaprocess "github.com/dreaagent/drea/internal/process"
)

// timeout bounds any single git invocation the harness makes on its own
// initiative. These are all metadata operations, so a generous bound still
// catches a hung command.
var commandTimeout = 60 * time.Second

// maxOutput caps what is read from git, so a pathological repository cannot
// make the harness buffer an unbounded amount of memory.
const maxOutput = 64 << 10

// run executes `git -C dir args…` and returns its trimmed combined output.
// A non-zero exit is returned as an error with the output attached.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"git", "-C", dir}, args...)
	result := dreaprocess.Run(ctx, dir, argv, commandTimeout, maxOutput)
	if !result.Started {
		return "", fmt.Errorf("git %s: %w", args[0], result.Err)
	}
	out := strings.TrimSpace(result.Output)
	if result.Truncated {
		out += "\n… (output truncated)"
	}
	if result.TimedOut {
		return out, fmt.Errorf("git %s: timed out", args[0])
	}
	if result.Err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		if out == "" {
			return "", fmt.Errorf("git %s: %w", args[0], result.Err)
		}
		return "", fmt.Errorf("git %s: %s", args[0], firstLine(out))
	}
	return out, nil
}

// ok reports whether the command succeeded, discarding its output. It is used
// for the yes/no queries where git communicates via the exit code.
func ok(ctx context.Context, dir string, args ...string) bool {
	_, err := run(ctx, dir, args...)
	return err == nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// RepoRoot returns the canonical absolute path of the repository toplevel that
// contains dir (git rev-parse --show-toplevel), after Abs and EvalSymlinks.
func RepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(strings.TrimSpace(out))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(real), nil
}

// canonicalize returns an absolute, symlink-resolved, cleaned path for dir.
func canonicalize(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(real), nil
}

// RequireRepoRoot errors unless dir is exactly the repository toplevel. This
// stops a nested workspace from running git against a parent repository (and
// mutating sibling paths outside the workspace).
func RequireRepoRoot(ctx context.Context, dir string) error {
	root, err := RepoRoot(ctx, dir)
	if err != nil {
		return errors.New("not a git repository")
	}
	here, err := canonicalize(dir)
	if err != nil {
		return err
	}
	if here != root {
		return fmt.Errorf("workspace is not the repository root (repository is %s); refuse to run git against a parent repository", root)
	}
	return nil
}

// IsRepoRoot reports whether dir itself is a git repository root: it has a
// .git entry, or its show-toplevel equals dir. Unlike IsRepo, a subdirectory
// of another repository is not treated as a root — so git init may create a
// nested repository there.
func IsRepoRoot(ctx context.Context, dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	root, err := RepoRoot(ctx, dir)
	if err != nil {
		return false
	}
	here, err := canonicalize(dir)
	if err != nil {
		return false
	}
	return here == root
}

// HasCommits reports whether the repository has at least one commit. A freshly
// initialised repository has none, and much of git fails outright on that
// state rather than reporting it.
func HasCommits(ctx context.Context, dir string) bool {
	return ok(ctx, dir, "rev-parse", "--verify", "HEAD")
}

// IdentityArgs returns the `-c` arguments needed to make a commit succeed.
// Git refuses to commit when no user.name/user.email is configured, which is
// the default on a fresh machine; that must not be what stops a task from
// checkpointing. An identity already configured — global or per-repository —
// is always left alone, and no git configuration is ever written.
func IdentityArgs(ctx context.Context, dir string) []string {
	if ok(ctx, dir, "var", "GIT_COMMITTER_IDENT") {
		return nil
	}
	return []string{"-c", "user.name=github.com/dreaagent/drea", "-c", "user.email=github.com/dreaagent/drea@localhost"}
}

// Head returns the short hash of HEAD.
func Head(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--short", "HEAD")
}

// resolve returns the full hash a ref points at.
func resolve(ctx context.Context, dir, ref string) (string, error) {
	if err := ValidRef(ref); err != nil {
		return "", err
	}
	return run(ctx, dir, "rev-parse", ref)
}

// UniqueBranch returns base, or base-2, base-3… — whichever is not taken. It
// is best-effort: a caller that loses a race with another process gets an
// error from the branch creation itself rather than clobbering anything.
func UniqueBranch(ctx context.Context, dir, base string) string {
	name := base
	for i := 2; i < 100 && ok(ctx, dir, "show-ref", "--verify", "--quiet", "refs/heads/"+name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// Dirty reports whether the work tree has uncommitted changes, including
// untracked files.
func Dirty(ctx context.Context, dir string) (bool, error) {
	out, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Checkpoint commits everything in the work tree and returns the resulting
// commit. When there is nothing to commit it is a no-op that still returns the
// current HEAD, so callers always get a ref they can roll back to. Repositories
// with no commits at all are checkpointed by creating the first one.
func Checkpoint(ctx context.Context, dir, message string) (ref string, committed bool, err error) {
	if err := RequireRepoRoot(ctx, dir); err != nil {
		return "", false, err
	}
	dirty, err := Dirty(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if !dirty {
		if !HasCommits(ctx, dir) {
			return "", false, errors.New("repository has no commits and nothing to commit")
		}
		ref, err = Head(ctx, dir)
		return ref, false, err
	}
	if _, err := run(ctx, dir, "add", "-A"); err != nil {
		return "", false, err
	}
	args := append(IdentityArgs(ctx, dir), "commit", "-m", message)
	if _, err := run(ctx, dir, args...); err != nil {
		return "", false, err
	}
	ref, err = Head(ctx, dir)
	return ref, true, err
}

// Restore returns the work tree to ref, discarding everything done since:
// changes to tracked files and files added afterwards. Removing new files is
// what makes this a real undo — a reset alone leaves the additions behind, so
// a "rolled back" tree would still contain half of the change. Files a
// Checkpoint had already committed are therefore never lost, and ignored files
// (build output, caches) are left alone.
func Restore(ctx context.Context, dir, ref string) error {
	if err := RequireRepoRoot(ctx, dir); err != nil {
		return err
	}
	if err := ValidRef(ref); err != nil {
		return err
	}
	if _, err := run(ctx, dir, "reset", "--hard", ref); err != nil {
		return err
	}
	_, err := run(ctx, dir, "clean", "-fd")
	return err
}

// Rollback returns the work tree to ref, but keeps everything done since on a
// branch of its own first. Undoing a change should not mean destroying it: the
// attempt stays reachable (`git diff ref branch`), so it can be reviewed, or
// resumed, rather than having to be rediscovered.
//
// preserved is false when the work tree was already at ref and there was
// nothing to keep. If the attempt cannot be preserved, nothing is rolled back —
// failing loudly is better than quietly destroying the work this exists to save.
func Rollback(ctx context.Context, dir, ref, branch string) (attempt string, preserved bool, err error) {
	if err := RequireRepoRoot(ctx, dir); err != nil {
		return "", false, err
	}
	if err := ValidRef(ref); err != nil {
		return "", false, err
	}
	if err := ValidRef(branch); err != nil {
		return "", false, err
	}
	dirty, err := Dirty(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if dirty {
		if _, err := run(ctx, dir, "add", "-A"); err != nil {
			return "", false, err
		}
		args := append(IdentityArgs(ctx, dir), "commit", "-m", "drea: attempt rolled back from "+ref)
		if _, err := run(ctx, dir, args...); err != nil {
			return "", false, err
		}
	}
	head, herr := resolve(ctx, dir, "HEAD")
	base, berr := resolve(ctx, dir, ref)
	if herr == nil && berr == nil && head != base {
		if _, err := run(ctx, dir, "branch", branch, head); err != nil {
			return "", false, fmt.Errorf("could not preserve the attempt, so nothing was rolled back: %w", err)
		}
		attempt, preserved = branch, true
	}
	if err := Restore(ctx, dir, ref); err != nil {
		return attempt, preserved, err
	}
	return attempt, preserved, nil
}

// MergeFFOnly fast-forwards the branch checked out in repo to branch. It is
// deliberately fast-forward only: a merge that needs a commit needs a human,
// because it means the two histories diverged while the attempt was running.
func MergeFFOnly(ctx context.Context, repo, branch string) error {
	if err := ValidRef(branch); err != nil {
		return err
	}
	dirty, err := Dirty(ctx, repo)
	if err != nil {
		return err
	}
	if dirty {
		return errors.New("the repository has uncommitted changes")
	}
	_, err = run(ctx, repo, "merge", "--ff-only", branch)
	return err
}

// ValidRef rejects refs that look like option flags, as defence in depth even
// though every argument is passed as an explicit argv element.
func ValidRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("empty ref")
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n;|&$`\\\"'") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

// Worktree is a scratch checkout of a repository on its own branch, used to
// isolate an attempt from the repository it is working on.
type Worktree struct {
	// Repo is the original repository the worktree was created from.
	Repo string
	// Path is the worktree's directory (outside Repo, so the repository it
	// belongs to stays clean).
	Path string
	// Branch is the branch created for the worktree.
	Branch string
	// Base is the commit the worktree started from, so what it has done since
	// can be told apart from what it inherited.
	Base string
}

// AddWorktree creates a worktree of repo at path on a new branch, starting
// from the repository's current HEAD.
//
// Uncommitted changes in the original work tree are *not* carried over — a
// worktree starts from a commit. Callers should say so, since silently working
// from a different state than the user sees would be worse than not isolating
// at all.
func AddWorktree(ctx context.Context, repo, path, branch string) (Worktree, error) {
	w := Worktree{Repo: repo, Path: path, Branch: branch}
	if err := RequireRepoRoot(ctx, repo); err != nil {
		return w, err
	}
	if !HasCommits(ctx, repo) {
		return w, errors.New("repository has no commits yet; commit a baseline first")
	}
	if err := ValidRef(branch); err != nil {
		return w, err
	}
	base, err := resolve(ctx, repo, "HEAD")
	if err != nil {
		return w, err
	}
	if _, err := run(ctx, repo, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return w, err
	}
	w.Base = base
	return w, nil
}

// Changed reports whether the worktree holds anything the repository does not:
// uncommitted changes, or commits made since it branched off. Checking only for
// uncommitted changes would call a worktree "untouched" the moment the agent
// committed its work, and it would then be cleaned up — deleting it.
func (w Worktree) Changed(ctx context.Context) (bool, error) {
	dirty, err := Dirty(ctx, w.Path)
	if err != nil {
		return true, err
	}
	if dirty {
		return true, nil
	}
	head, err := resolve(ctx, w.Path, "HEAD")
	if err != nil {
		return true, err
	}
	return w.Base != "" && head != w.Base, nil
}

// Remove deletes the worktree and its branch. It is deliberately unforceful at
// both steps — a modified worktree is left in place, and `branch -d` refuses a
// branch holding unmerged commits — so cleanup can never be what loses work.
func (w Worktree) Remove(ctx context.Context) error {
	if _, err := run(ctx, w.Repo, "worktree", "remove", w.Path); err != nil {
		return err
	}
	_, err := run(ctx, w.Repo, "branch", "-d", w.Branch)
	return err
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
