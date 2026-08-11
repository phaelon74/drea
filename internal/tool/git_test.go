package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dreaagent/drea/internal/vcs"
)

// initRepo creates a git repository in a temp dir with one initial commit and
// returns its path. It skips the test if git is unavailable.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@e")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "initial")
	return dir
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGitReadStatusAndDiff(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()

	// Clean tree.
	rd := &gitRead{root: dir}
	out, err := rd.Run(ctx, mustJSON(t, map[string]string{"action": "status"}))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "##") && !strings.Contains(out, "clean") {
		t.Errorf("unexpected status output: %q", out)
	}

	// Modify a tracked file; diff should show it.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = rd.Run(ctx, mustJSON(t, map[string]string{"action": "diff"}))
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "-one") || !strings.Contains(out, "+two") {
		t.Errorf("diff missing change: %q", out)
	}
}

func TestGitCommitAndRollback(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()

	// Make a change and commit it via the tool.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := &gitCommit{root: dir}
	if _, err := commit.Run(ctx, mustJSON(t, map[string]string{"message": "add b"})); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Capture the committed state's ref (HEAD), then make a bad change.
	rd := &gitRead{root: dir}
	if _, err := rd.Run(ctx, mustJSON(t, map[string]string{"action": "log"})); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("BROKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Roll back uncommitted changes to HEAD.
	rb := &gitRollback{root: dir}
	if _, err := rb.Run(ctx, mustJSON(t, map[string]string{})); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new file\n" {
		t.Errorf("rollback did not restore committed content, got %q", got)
	}
}

func TestGitCommitNothingToCommit(t *testing.T) {
	dir := initRepo(t)
	commit := &gitCommit{root: dir}
	out, err := commit.Run(context.Background(), mustJSON(t, map[string]string{"message": "noop"}))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !strings.Contains(out, "nothing to commit") {
		t.Errorf("expected 'nothing to commit', got %q", out)
	}
}

func TestGitRollbackCleanRemovesUntracked(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rb := &gitRollback{root: dir}
	if _, err := rb.Run(ctx, mustJSON(t, map[string]any{"clean": true})); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Errorf("expected untracked file removed, stat err = %v", err)
	}
}

func TestGitNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	rd := &gitRead{root: dir}
	_, err := rd.Run(context.Background(), mustJSON(t, map[string]string{"action": "status"}))
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitInitCreatesRepoAndIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	init := &gitInit{root: dir}

	out, err := init.Run(ctx, nil)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("unexpected init output: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("expected .git directory: %v", err)
	}

	// A second call must not re-initialise or fail.
	out, err = init.Run(ctx, nil)
	if err != nil {
		t.Fatalf("second init: %v", err)
	}
	if !strings.Contains(out, "already a git repository") {
		t.Errorf("expected idempotent report, got %q", out)
	}
}

// A freshly initialised repository has no commits; read actions must report
// that rather than failing, since git itself exits non-zero on this state.
func TestGitReadOnEmptyRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := (&gitInit{root: dir}).Run(ctx, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	rd := &gitRead{root: dir}
	for _, action := range []string{"status", "log", "show"} {
		out, err := rd.Run(ctx, mustJSON(t, map[string]string{"action": action}))
		if err != nil {
			t.Errorf("%s on empty repo: %v", action, err)
		}
		if action != "status" && !strings.Contains(out, "no commits yet") {
			t.Errorf("%s: expected 'no commits yet', got %q", action, out)
		}
	}
	if _, err := (&gitRollback{root: dir}).Run(ctx, mustJSON(t, map[string]string{})); err == nil {
		t.Error("expected rollback to fail with no commits")
	}
}

// git_init followed by git_commit is the checkpoint path a task starts with, and
// it must work on a machine with no configured git identity — the default on a
// fresh install, where git otherwise refuses to commit at all.
func TestGitInitThenCommitWithoutIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	ctx := context.Background()

	if _, err := (&gitInit{root: dir}).Run(ctx, nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(vcs.IdentityArgs(ctx, dir)) == 0 {
		t.Fatal("test environment still has a git identity; the fallback path is not being exercised")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := (&gitCommit{root: dir}).Run(ctx, mustJSON(t, map[string]string{"message": "baseline"}))
	if err != nil {
		t.Fatalf("commit without configured identity: %v", err)
	}
	if !strings.Contains(out, "committed") {
		t.Errorf("unexpected commit output: %q", out)
	}
}

// No tool may be named after a shell command: that invites the model to treat
// the tool and the real binary as interchangeable and reach for run_command,
// which bypasses the approval prompt and the git tools' argument validation.
func TestNoToolIsNamedAfterAShellCommand(t *testing.T) {
	shellNames := map[string]bool{"git": true, "ls": true, "cat": true, "grep": true, "find": true, "sed": true, "rm": true}
	for _, spec := range NewRegistry(t.TempDir()).Specs() {
		if shellNames[spec.Function.Name] {
			t.Errorf("tool %q shares its name with a shell command", spec.Function.Name)
		}
	}
}

func TestGitInspectNormalizesShellStyleAction(t *testing.T) {
	dir := initRepo(t)
	rd := &gitRead{root: dir}
	for _, action := range []string{"status", "git status", " Status "} {
		if _, err := rd.Run(context.Background(), mustJSON(t, map[string]string{"action": action})); err != nil {
			t.Errorf("action %q: %v", action, err)
		}
	}
	if _, err := rd.Run(context.Background(), mustJSON(t, map[string]string{"action": "push"})); err == nil {
		t.Error("expected unknown action to be rejected")
	}
}

func TestValidRef(t *testing.T) {
	ok := []string{"HEAD", "main", "abc123", "HEAD~2", "v1.0.0"}
	for _, r := range ok {
		if err := vcs.ValidRef(r); err != nil {
			t.Errorf("vcs.ValidRef(%q) unexpected error: %v", r, err)
		}
	}
	bad := []string{"-f", "--hard", "a b", "a;rm", "a`x`", "a$(x)", "a|b"}
	for _, r := range bad {
		if err := vcs.ValidRef(r); err == nil {
			t.Errorf("vcs.ValidRef(%q) expected error", r)
		}
	}
}

func TestGitRollbackRejectsBadRef(t *testing.T) {
	dir := initRepo(t)
	rb := &gitRollback{root: dir}
	_, err := rb.Run(context.Background(), mustJSON(t, map[string]string{"ref": "--hard"}))
	if err == nil {
		t.Fatal("expected error for flag-like ref")
	}
}

func TestGitInitAllowsNestedWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent := initRepo(t)
	nested := filepath.Join(parent, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Nested dir is inside a work tree but is not a repo root.
	rd := &gitRead{root: nested}
	if _, err := rd.Run(ctx, mustJSON(t, map[string]string{"action": "status"})); err == nil {
		t.Fatal("git_inspect in a nested workspace must refuse the parent repo")
	}

	init := &gitInit{root: nested}
	out, err := init.Run(ctx, nil)
	if err != nil {
		t.Fatalf("git_init in nested workspace: %v", err)
	}
	if !strings.Contains(out, "initialized") {
		t.Fatalf("expected nested init, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(nested, ".git")); err != nil {
		t.Fatalf("expected nested .git: %v", err)
	}

	// Parent sibling must remain untouched by nested commits.
	if err := os.WriteFile(filepath.Join(nested, "x.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&gitCommit{root: nested}).Run(ctx, mustJSON(t, map[string]string{"message": "nested"})); err != nil {
		t.Fatalf("nested commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("nested commit must not create files in the parent worktree")
	}
}
