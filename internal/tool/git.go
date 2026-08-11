package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dreaagent/drea/internal/vcs"
)

// Git tools give the agent first-class, workspace-confined version control so
// it can inspect, snapshot and — crucially for self-improvement — roll back its
// own changes. Every invocation runs `git -C <root> …` with an explicitly
// constructed argument list (never a raw command string), so the model cannot
// point git at another repository or smuggle in extra arguments.

// gitTimeout bounds any single git invocation.
const gitTimeout = 60 * time.Second

// runGit executes git with the given args inside root and returns combined
// output. err is non-nil when git exits non-zero (with the output attached), so
// callers can surface a useful message to the model.
func runGit(ctx context.Context, root string, args ...string) (string, error) {
	full := append([]string{"-C", root}, args...)
	out, code, timedOut, err := runArgv(ctx, root, "git", full, gitTimeout)
	if err != nil {
		return "", err
	}
	out = strings.TrimRight(out, "\n")
	if timedOut {
		return out, errors.New("git timed out")
	}
	if code != 0 {
		if out == "" {
			return "", fmt.Errorf("git exited with code %d", code)
		}
		return "", fmt.Errorf("git failed (exit %d): %s", code, out)
	}
	return out, nil
}

// The harness's own git bookkeeping lives in internal/vcs; the checks below
// are shared with it so the tools and the harness can never disagree about
// whether a workspace is a repository or how a commit is made.

// ensureRepo returns an actionable error when root is not exactly a git
// repository root. Requiring the toplevel (not merely "inside a work tree")
// stops a nested workspace from running git against a parent repository.
func ensureRepo(ctx context.Context, root string) error {
	if err := vcs.RequireRepoRoot(ctx, root); err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return errors.New("not a git repository; use the git_init tool to create one")
		}
		return err
	}
	return nil
}

// ---- git_inspect (read-only) ----
//
// Named git_inspect rather than git: a tool whose name is exactly a shell
// command invites the model to conflate the two and reach for run_command
// instead. Every tool in this family is prefixed the same way.

type gitRead struct{ root string }

func (t *gitRead) Name() string        { return "git_inspect" }
func (t *gitRead) Mutating() bool      { return false }
func (t *gitRead) AlwaysConfirm() bool { return false }
func (t *gitRead) Description() string {
	return "Inspect the workspace git repository (read-only); use this instead of running git through run_command. action is one of: status (short status), diff (unstaged changes; optional 'path' or staged=true), log (recent commits; optional 'limit'), show (a commit or ref given as 'ref'). Use this to understand what has changed before committing or rolling back."
}
func (t *gitRead) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["status","diff","log","show"],"description":"Which read-only git query to run."},
    "path":{"type":"string","description":"Optional path to scope a diff to."},
    "ref":{"type":"string","description":"Commit/ref for 'show' (e.g. HEAD, a hash)."},
    "staged":{"type":"boolean","description":"For diff: show staged changes instead of unstaged."},
    "limit":{"type":"integer","description":"For log: number of commits (default 15)."}
  },
  "required":["action"]
}`)
}
func (t *gitRead) Summary(args json.RawMessage) string {
	var a struct {
		Action string `json:"action"`
	}
	_ = decode(args, &a)
	return "git_inspect " + a.Action
}
func (t *gitRead) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Action string `json:"action"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		Staged bool   `json:"staged"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if err := ensureRepo(ctx, t.root); err != nil {
		return "", err
	}
	switch normalizeAction(a.Action) {
	case "status":
		out, err := runGit(ctx, t.root, "status", "--short", "--branch")
		if err != nil {
			return "", err
		}
		return nonEmpty(out, "(clean working tree)"), nil
	case "diff":
		argv := []string{"diff"}
		if a.Staged {
			argv = append(argv, "--staged")
		}
		if p := strings.TrimSpace(a.Path); p != "" {
			rp, err := resolve(t.root, p)
			if err != nil {
				return "", err
			}
			argv = append(argv, "--", rp)
		}
		out, err := runGit(ctx, t.root, argv...)
		if err != nil {
			return "", err
		}
		return nonEmpty(out, "(no changes)"), nil
	case "log":
		if !vcs.HasCommits(ctx, t.root) {
			return "(no commits yet)", nil
		}
		n := a.Limit
		if n <= 0 || n > 100 {
			n = 15
		}
		return runGit(ctx, t.root, "log", "--oneline", "--decorate", "-n", fmt.Sprintf("%d", n))
	case "show":
		ref := strings.TrimSpace(a.Ref)
		if ref == "" {
			ref = "HEAD"
		}
		if err := vcs.ValidRef(ref); err != nil {
			return "", err
		}
		if !vcs.HasCommits(ctx, t.root) {
			return "(no commits yet)", nil
		}
		return runGit(ctx, t.root, "show", "--stat", ref)
	default:
		return "", fmt.Errorf("unknown action %q; expected one of: status, diff, log, show", a.Action)
	}
}

// ---- git_commit ----

type gitCommit struct{ root string }

func (t *gitCommit) Name() string        { return "git_commit" }
func (t *gitCommit) Mutating() bool      { return true }
func (t *gitCommit) AlwaysConfirm() bool { return false }
func (t *gitCommit) Description() string {
	return "Stage changes and create a commit in the workspace repository. By default stages all changes (tracked and untracked); pass 'paths' to stage only those. A commit is a checkpoint you can later roll back to with git_rollback. 'message' is required."
}
func (t *gitCommit) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "message":{"type":"string","description":"Commit message."},
    "paths":{"type":"array","items":{"type":"string"},"description":"Optional paths to stage; defaults to all changes."}
  },
  "required":["message"]
}`)
}
func (t *gitCommit) Summary(args json.RawMessage) string {
	var a struct {
		Message string `json:"message"`
	}
	_ = decode(args, &a)
	return "git commit -m " + oneLineArg(a.Message, 60)
}
func (t *gitCommit) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Message) == "" {
		return "", errors.New("commit message is required")
	}
	if err := ensureRepo(ctx, t.root); err != nil {
		return "", err
	}

	if len(a.Paths) == 0 {
		if _, err := runGit(ctx, t.root, "add", "-A"); err != nil {
			return "", err
		}
	} else {
		add := []string{"add", "--"}
		for _, p := range a.Paths {
			rp, err := resolve(t.root, p)
			if err != nil {
				return "", err
			}
			add = append(add, rp)
		}
		if _, err := runGit(ctx, t.root, add...); err != nil {
			return "", err
		}
	}

	// Nothing staged? Report cleanly rather than failing with git's exit 1.
	if _, code, _, err := runArgv(ctx, t.root, "git", []string{"-C", t.root, "diff", "--cached", "--quiet"}, gitTimeout); err == nil && code == 0 {
		return "nothing to commit; working tree clean", nil
	}

	commit := append(vcs.IdentityArgs(ctx, t.root), "commit", "-m", a.Message)
	if _, err := runGit(ctx, t.root, commit...); err != nil {
		return "", err
	}
	out, err := runGit(ctx, t.root, "log", "--oneline", "-n", "1")
	if err != nil {
		return "", err
	}
	return "committed " + nonEmpty(out, "(unknown)"), nil
}

// ---- git_init ----

type gitInit struct{ root string }

func (t *gitInit) Name() string        { return "git_init" }
func (t *gitInit) Mutating() bool      { return true }
func (t *gitInit) AlwaysConfirm() bool { return false }
func (t *gitInit) Description() string {
	return "Create a git repository in the workspace so work can be checkpointed and rolled back. Use this at the start of a task when the workspace is not yet under version control; it is safe to call when it already is (it reports that and changes nothing). Follow it with git_commit to record a baseline."
}
func (t *gitInit) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *gitInit) Summary(json.RawMessage) string { return "git init" }
func (t *gitInit) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	// Idempotent for an actual repository root. A workspace that merely sits
	// inside a parent repository is allowed to create its own nested repo so
	// git operations stay confined to the workspace.
	if vcs.IsRepoRoot(ctx, t.root) {
		return "already a git repository; nothing to do", nil
	}
	if _, err := runGit(ctx, t.root, "init"); err != nil {
		return "", err
	}
	return "initialized empty git repository in " + t.root + "; use git_commit to record a baseline checkpoint", nil
}

// ---- git_rollback ----

type gitRollback struct{ root string }

func (t *gitRollback) Name() string        { return "git_rollback" }
func (t *gitRollback) Mutating() bool      { return true }
func (t *gitRollback) AlwaysConfirm() bool { return true }
func (t *gitRollback) Description() string {
	return "Restore the workspace to a previous committed state, discarding uncommitted changes to tracked files (git reset --hard). Use this to undo a change that made things worse. 'ref' defaults to HEAD (discard uncommitted work); pass a commit hash or ref to return to an earlier checkpoint. Untracked files are left in place unless clean=true. This is destructive and always requires approval."
}
func (t *gitRollback) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "ref":{"type":"string","description":"Commit/ref to reset to. Default HEAD."},
    "clean":{"type":"boolean","description":"Also remove untracked files/directories (git clean -fd). Default false."}
  }
}`)
}
func (t *gitRollback) Summary(args json.RawMessage) string {
	var a struct {
		Ref   string `json:"ref"`
		Clean bool   `json:"clean"`
	}
	_ = decode(args, &a)
	ref := strings.TrimSpace(a.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	s := "git reset --hard " + ref
	if a.Clean {
		s += " (+ clean -fd)"
	}
	return s
}
func (t *gitRollback) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Ref   string `json:"ref"`
		Clean bool   `json:"clean"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if err := ensureRepo(ctx, t.root); err != nil {
		return "", err
	}
	ref := strings.TrimSpace(a.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	if err := vcs.ValidRef(ref); err != nil {
		return "", err
	}
	if !vcs.HasCommits(ctx, t.root) {
		return "", errors.New("no commits to roll back to; create a checkpoint with git_commit first")
	}
	if _, err := runGit(ctx, t.root, "reset", "--hard", ref); err != nil {
		return "", err
	}
	out := "reset --hard to " + ref
	if a.Clean {
		if _, err := runGit(ctx, t.root, "clean", "-fd"); err != nil {
			return "", err
		}
		out += "; removed untracked files"
	}
	status, _ := runGit(ctx, t.root, "log", "--oneline", "-n", "1")
	return out + "\nnow at: " + status, nil
}

// normalizeAction accepts the action a model is most likely to send when it is
// thinking in shell terms ("git log", "log") and reduces it to the bare verb.
// This is unambiguous rewriting, not guessing: anything else still falls
// through to the unknown-action error.
func normalizeAction(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimPrefix(s, "git "))
	return s
}

func nonEmpty(s, whenEmpty string) string {
	if strings.TrimSpace(s) == "" {
		return whenEmpty
	}
	return s
}

func oneLineArg(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return "\"" + string(r[:max]) + "…\""
	}
	return "\"" + s + "\""
}
