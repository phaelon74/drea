package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo creates a git repository with one commit and returns its path.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := run(ctx, dir, "init"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "one\n")
	if _, _, err := Checkpoint(ctx, dir, "initial"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsRepoAndHasCommits(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	if !IsRepo(ctx, dir) || !HasCommits(ctx, dir) {
		t.Fatal("expected an initialised repository with a commit")
	}
	plain := t.TempDir()
	if IsRepo(ctx, plain) {
		t.Fatal("a plain directory must not be reported as a repository")
	}
}

func TestCheckpointCommitsAndIsANoOpWhenClean(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)

	write(t, dir, "b.txt", "two\n")
	ref, committed, err := Checkpoint(ctx, dir, "work")
	if err != nil || !committed || ref == "" {
		t.Fatalf("checkpoint: ref=%q committed=%v err=%v", ref, committed, err)
	}

	// Clean tree: still yields a usable ref, but creates no commit.
	again, committed, err := Checkpoint(ctx, dir, "work")
	if err != nil || committed {
		t.Fatalf("clean checkpoint should not commit: committed=%v err=%v", committed, err)
	}
	if again != ref {
		t.Fatalf("clean checkpoint should report the same HEAD: %q vs %q", again, ref)
	}
}

func TestCheckpointRejectsNonRepo(t *testing.T) {
	if _, _, err := Checkpoint(context.Background(), t.TempDir(), "x"); err == nil {
		t.Fatal("expected an error outside a repository")
	}
}

func TestRestoreReturnsToTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	ref, _, err := Checkpoint(ctx, dir, "base")
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.txt", "ruined\n")
	if _, _, err := Checkpoint(ctx, dir, "bad change"); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, dir, ref); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(got) != "one\n" {
		t.Fatalf("rollback did not restore the file: %q %v", got, err)
	}
}

// A reset alone leaves files the task added behind, which would make "rolled
// back" mean "half the change is still there".
func TestRestoreRemovesFilesAddedAfterTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	ref, _, err := Checkpoint(ctx, dir, "base")
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "added.txt", "new\n")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, filepath.Join("sub", "also.txt"), "new\n")

	if err := Restore(ctx, dir, ref); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"added.txt", "sub"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived the rollback: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("checkpointed file was destroyed: %v", err)
	}
}

// Rolling back must undo the change without destroying it: the tree returns to
// the checkpoint, and the attempt stays reachable on its own branch.
func TestRollbackKeepsTheAttemptOnABranch(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	ref, _, err := Checkpoint(ctx, dir, "base")
	if err != nil {
		t.Fatal(err)
	}

	write(t, dir, "a.txt", "ruined\n")
	write(t, dir, "added.txt", "new\n")

	attempt, preserved, err := Rollback(ctx, dir, ref, "drea/attempt-1")
	if err != nil || !preserved || attempt != "drea/attempt-1" {
		t.Fatalf("rollback: attempt=%q preserved=%v err=%v", attempt, preserved, err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(got) != "one\n" {
		t.Fatalf("work tree not restored: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("added file survived the rollback: %v", err)
	}

	// The whole point: the discarded work is still there to look at.
	out, err := run(ctx, dir, "show", attempt+":added.txt")
	if err != nil || strings.TrimSpace(out) != "new" {
		t.Fatalf("attempt was not preserved on the branch: %q %v", out, err)
	}
}

func TestRollbackWithNothingToPreserve(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	ref, _, err := Checkpoint(ctx, dir, "base")
	if err != nil {
		t.Fatal(err)
	}
	attempt, preserved, err := Rollback(ctx, dir, ref, "drea/attempt-1")
	if err != nil || preserved || attempt != "" {
		t.Fatalf("expected no branch for an unchanged tree: attempt=%q preserved=%v err=%v", attempt, preserved, err)
	}
}

func TestUniqueBranchAvoidsExistingNames(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	if got := UniqueBranch(ctx, dir, "drea/attempt-1"); got != "drea/attempt-1" {
		t.Fatalf("unused name should be returned as is, got %q", got)
	}
	if _, err := run(ctx, dir, "branch", "drea/attempt-1"); err != nil {
		t.Fatal(err)
	}
	if got := UniqueBranch(ctx, dir, "drea/attempt-1"); got != "drea/attempt-1-2" {
		t.Fatalf("taken name should be avoided, got %q", got)
	}
}

func TestMergeFFOnly(t *testing.T) {
	ctx := context.Background()
	src := repo(t)
	path := filepath.Join(t.TempDir(), "attempt")
	wt, err := AddWorktree(ctx, src, path, "drea/attempt")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "b.txt", "from the attempt\n")
	if _, _, err := Checkpoint(ctx, path, "attempt work"); err != nil {
		t.Fatal(err)
	}
	if err := MergeFFOnly(ctx, src, wt.Branch); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "b.txt")); err != nil {
		t.Fatalf("work was not merged into the repository: %v", err)
	}
}

// A merge that is not a fast-forward needs a human, and uncommitted work in the
// repository must never be walked over.
func TestMergeFFOnlyRefusesDivergedOrDirtyRepositories(t *testing.T) {
	ctx := context.Background()
	src := repo(t)
	path := filepath.Join(t.TempDir(), "attempt")
	wt, err := AddWorktree(ctx, src, path, "drea/attempt")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "b.txt", "from the attempt\n")
	if _, _, err := Checkpoint(ctx, path, "attempt work"); err != nil {
		t.Fatal(err)
	}

	write(t, src, "c.txt", "uncommitted local work\n")
	if err := MergeFFOnly(ctx, src, wt.Branch); err == nil {
		t.Fatal("expected a refusal while the repository is dirty")
	}
	if _, _, err := Checkpoint(ctx, src, "local work"); err != nil { // now diverged
		t.Fatal(err)
	}
	if err := MergeFFOnly(ctx, src, wt.Branch); err == nil {
		t.Fatal("expected a refusal for a non-fast-forward merge")
	}
}

// Cleanup keys on "unchanged", so a worktree whose work has been committed must
// not look unchanged — otherwise removing it would delete the work.
func TestWorktreeChangedSeesCommittedWork(t *testing.T) {
	ctx := context.Background()
	src := repo(t)
	path := filepath.Join(t.TempDir(), "attempt")
	wt, err := AddWorktree(ctx, src, path, "drea/attempt")
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := wt.Changed(ctx); err != nil || changed {
		t.Fatalf("a fresh worktree is unchanged: changed=%v err=%v", changed, err)
	}
	write(t, path, "b.txt", "work\n")
	if changed, err := wt.Changed(ctx); err != nil || !changed {
		t.Fatalf("uncommitted work: changed=%v err=%v", changed, err)
	}
	if _, _, err := Checkpoint(ctx, path, "work"); err != nil {
		t.Fatal(err)
	}
	if changed, err := wt.Changed(ctx); err != nil || !changed {
		t.Fatalf("committed work must still count as changed: changed=%v err=%v", changed, err)
	}
	if err := wt.Remove(ctx); err == nil {
		t.Fatal("cleanup must not delete a branch holding unmerged commits")
	}
}

func TestValidRefRejectsOptionsAndMetacharacters(t *testing.T) {
	for _, bad := range []string{"", "--hard", "-f", "HEAD; rm -rf /", "a b", "$(id)", "a`id`"} {
		if err := ValidRef(bad); err == nil {
			t.Errorf("ref %q should be rejected", bad)
		}
	}
	for _, good := range []string{"HEAD", "HEAD~2", "abc1234", "drea/attempt-1"} {
		if err := ValidRef(good); err != nil {
			t.Errorf("ref %q should be accepted: %v", good, err)
		}
	}
}

func TestWorktreeIsolatesAndCleansUp(t *testing.T) {
	ctx := context.Background()
	src := repo(t)
	path := filepath.Join(t.TempDir(), "attempt")

	wt, err := AddWorktree(ctx, src, path, "drea/attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "a.txt")); err != nil {
		t.Fatalf("worktree missing repository content: %v", err)
	}

	// A change in the worktree must not touch the original work tree.
	write(t, path, "a.txt", "changed in the attempt\n")
	orig, err := os.ReadFile(filepath.Join(src, "a.txt"))
	if err != nil || string(orig) != "one\n" {
		t.Fatalf("original workspace was modified: %q %v", orig, err)
	}
	if dirty, err := Dirty(ctx, src); err != nil || dirty {
		t.Fatalf("original workspace should stay clean: dirty=%v err=%v", dirty, err)
	}

	// Removal only succeeds once the attempt is discarded, which is the point:
	// work is not silently thrown away.
	if err := wt.Remove(ctx); err == nil {
		t.Fatal("expected removal of a modified worktree to fail")
	}
	write(t, path, "a.txt", "one\n")
	if err := wt.Remove(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still present: %v", err)
	}
	out, _ := run(ctx, src, "branch", "--list", "drea/attempt")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("branch was not deleted: %q", out)
	}
}

func TestAddWorktreeNeedsARepositoryWithCommits(t *testing.T) {
	ctx := context.Background()
	if _, err := AddWorktree(ctx, t.TempDir(), filepath.Join(t.TempDir(), "w"), "b"); err == nil {
		t.Fatal("expected an error outside a repository")
	}

	empty := t.TempDir()
	if _, err := run(ctx, empty, "init"); err != nil {
		t.Skipf("git init: %v", err)
	}
	_, err := AddWorktree(ctx, empty, filepath.Join(t.TempDir(), "w"), "b")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("expected a 'no commits' error, got %v", err)
	}
}

func TestRequireRepoRootRejectsNestedDir(t *testing.T) {
	ctx := context.Background()
	dir := repo(t)
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RequireRepoRoot(ctx, nested); err == nil {
		t.Fatal("expected nested directory to be rejected")
	}
	if err := RequireRepoRoot(ctx, dir); err != nil {
		t.Fatalf("repo root should be accepted: %v", err)
	}
	if IsRepoRoot(ctx, nested) {
		t.Fatal("nested directory must not be reported as a repo root")
	}
	if !IsRepoRoot(ctx, dir) {
		t.Fatal("repo root should be reported as a repo root")
	}
}

func TestNestedWorkspaceCannotMutateParent(t *testing.T) {
	ctx := context.Background()
	parent := repo(t)
	write(t, parent, "sibling.txt", "parent sibling\n")
	nested := filepath.Join(parent, "ws")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Checkpoint(ctx, nested, "should fail"); err == nil {
		t.Fatal("checkpoint in a nested workspace must fail")
	}
	if err := Restore(ctx, nested, "HEAD"); err == nil {
		t.Fatal("restore in a nested workspace must fail")
	}
	got, err := os.ReadFile(filepath.Join(parent, "sibling.txt"))
	if err != nil || string(got) != "parent sibling\n" {
		t.Fatalf("parent sibling must be untouched: %q %v", got, err)
	}
}
