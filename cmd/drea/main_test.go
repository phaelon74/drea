package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/agent"
	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/eval"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/session"
	"github.com/dreaagent/drea/internal/settings"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
	"github.com/dreaagent/drea/internal/vcs"
)

func TestStringList(t *testing.T) {
	var sl stringList
	if s := sl.String(); s != "" {
		t.Errorf("empty stringList.String() = %q, want %q", s, "")
	}
	if err := sl.Set("hello"); err != nil {
		t.Fatal(err)
	}
	if err := sl.Set("world"); err != nil {
		t.Fatal(err)
	}
	if len(sl) != 2 || sl[0] != "hello" || sl[1] != "world" {
		t.Errorf("got %v", sl)
	}
	if s := sl.String(); s != "hello, world" {
		t.Errorf("String() = %q, want %q", s, "hello, world")
	}
}

func TestStringListIgnoresBlank(t *testing.T) {
	var sl stringList
	if err := sl.Set("  "); err != nil {
		t.Fatal(err)
	}
	if len(sl) != 0 {
		t.Errorf("blank value should be ignored, got %v", sl)
	}
}

func TestMaskKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "(unset)"},
		{"short", "set"},
		{"sk-abcdef1234567890", "set (…7890)"},
	}
	for _, c := range cases {
		if got := maskKey(c.in); got != c.want {
			t.Errorf("maskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func testCLI(t *testing.T, args ...string) (cliApp, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	for _, name := range []string{
		"DREA_API_KEY", "OPENAI_API_KEY", "DREA_DEBUG", "DREA_JSON_FORMAT",
		"DREA_REASONING_EFFORT", "DREA_ALLOW_COMMANDS", "DREA_DENY_COMMANDS",
	} {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	var out, errOut bytes.Buffer
	a := cliApp{
		args:   append([]string{"--workdir", root, "--base-url", "http://127.0.0.1:1"}, args...),
		in:     strings.NewReader(""),
		out:    &out,
		errOut: &errOut,
		loadSaved: func() (config.Saved, error) {
			return config.Saved{Model: "saved-model"}, nil
		},
		newUI: ui.New,
		signals: func() (<-chan os.Signal, func()) {
			return make(chan os.Signal), func() {}
		},
	}
	a.runOneShot = func(context.Context, *agent.Agent, *config.Config, string) error {
		t.Fatal("unexpected one-shot route")
		return nil
	}
	a.runInteractive = func(<-chan os.Signal, *agent.Agent, *ui.UI, *config.Config, *llm.Client, *tool.Registry, io.Reader, io.Writer) error {
		t.Fatal("unexpected interactive route")
		return nil
	}
	return a, &out, &errOut
}

func TestCLIVersionAndHelpUseInjectedWriters(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--version"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "drea "+version+"\n" {
		t.Fatalf("version output = %q", got)
	}

	a, out, errOut := testCLI(t, "--help")
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "Usage: drea [flags]") {
		t.Fatalf("help writers: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestCLIFlagPrecedenceAndRouting(t *testing.T) {
	t.Setenv("DREA_MODEL", "env-model")
	a, _, _ := testCLI(t, "--model", "flag-model", "do", "work")
	called := false
	a.runOneShot = func(_ context.Context, _ *agent.Agent, cfg *config.Config, task string) error {
		called = true
		if cfg.Model != "flag-model" || task != "do work" {
			t.Fatalf("model=%q task=%q", cfg.Model, task)
		}
		return nil
	}
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("one-shot route was not called")
	}

	a, _, _ = testCLI(t)
	called = false
	a.runInteractive = func(_ <-chan os.Signal, _ *agent.Agent, _ *ui.UI, cfg *config.Config, _ *llm.Client, _ *tool.Registry, in io.Reader, _ io.Writer) error {
		called = true
		if cfg.Model != "env-model" || in == nil {
			t.Fatalf("interactive cfg/input not passed: model=%q", cfg.Model)
		}
		return nil
	}
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("interactive route was not called")
	}
}

func TestTaskContextCancelsOnSignal(t *testing.T) {
	ch := make(chan os.Signal, 1)
	ctx, cancel := taskContext(ch)
	defer cancel()
	ch <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("task context was not cancelled")
	}
}

func TestReadInputUsesInjectedReader(t *testing.T) {
	in := bytes.NewBufferString("one\ntwo\n")
	reader := bufio.NewReader(in)
	if got, err := readInput(reader, false); err != nil || got != "one" {
		t.Fatalf("single-line input = %q, %v", got, err)
	}
	if got, err := readInput(reader, true); err != nil || got != "two" {
		t.Fatalf("multiline input = %q, %v", got, err)
	}
}

func TestStateChangingCommandsUpdateRuntime(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		Workdir:    root,
		BaseURL:    "http://127.0.0.1:1",
		Model:      "old",
		AutoApprove: true,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := ui.New()
	client := llm.NewClientWithReasoning(cfg.ChatURL(), "", cfg.Model, 0, 0, "", time.Second, true, "json_schema")
	tools := tool.NewRegistry(root)
	ag := agent.New(cfg, client, tools, u)

	command("/model new-model", u, ag, &cfg, client, tools)
	if cfg.Model != "new-model" || client.Model() != "new-model" {
		t.Fatal("/model did not update config and client")
	}
	command("/host https://localhost:1234/v1", u, ag, &cfg, client, tools)
	if cfg.BaseURL != "https://localhost:1234/v1" {
		t.Fatal("/host did not update config")
	}
	command("/auto off", u, ag, &cfg, client, tools)
	if cfg.AutoApprove || ag.AutoApprove() {
		t.Fatal("/auto off did not update next-dispatch agent state")
	}
	command("/verify go test ./...", u, ag, &cfg, client, tools)
	if cfg.Verify != "go test ./..." || ag.VerifyCommand() != cfg.Verify {
		t.Fatal("/verify did not update agent")
	}
	command("/checkpoint on", u, ag, &cfg, client, tools)
	if !cfg.Checkpoint || !ag.Checkpointing() {
		t.Fatal("/checkpoint did not update agent")
	}
	command("/reasoning high", u, ag, &cfg, client, tools)
	if cfg.ReasoningEffort != "high" {
		t.Fatal("/reasoning did not update config")
	}
	command("/key secret-value", u, ag, &cfg, client, tools)
	if cfg.APIKey != "secret-value" {
		t.Fatal("/key did not update config")
	}
	command("/host http://localhost:1234/v1", u, ag, &cfg, client, tools)
	if cfg.BaseURL != "https://localhost:1234/v1" {
		t.Fatal("/host allowed cleartext transport for an existing key")
	}
	command("/key off", u, ag, &cfg, client, tools)
	command("/host http://localhost:1234/v1", u, ag, &cfg, client, tools)
	command("/key secret-value", u, ag, &cfg, client, tools)
	if cfg.APIKey != "" {
		t.Fatal("/key allowed cleartext transport without explicit opt-in")
	}
}

func TestSaveRoundTripsSamplingValues(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("APPDATA", configHome)
	root := t.TempDir()
	cfg := config.Config{
		BaseURL:     "https://example.test/v1",
		Model:       "m",
		Workdir:     root,
		Temperature: 0.3,
		TopP:        0.7,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := ui.New()
	client := llm.NewClientWithReasoning(cfg.ChatURL(), "", cfg.Model, cfg.Temperature, cfg.TopP, "", time.Second, true, "json_schema")
	tools := tool.NewRegistry(root)
	ag := agent.New(cfg, client, tools, u)
	command("/save", u, ag, &cfg, client, tools)

	got, ok, err := settings.Load()
	if err != nil || !ok {
		t.Fatalf("load saved settings: ok=%v err=%v", ok, err)
	}
	if got.Temperature != 0.3 || got.TopP != 0.7 {
		t.Fatalf("saved sampling = temperature %v top-p %v", got.Temperature, got.TopP)
	}
}

func TestResumeAndResetCommandsUpdateConversation(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("APPDATA", configHome)
	root := t.TempDir()
	cfg := config.Config{
		BaseURL: "https://example.test/v1",
		Model:   "m",
		Workdir: root,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := ui.New()
	client := llm.NewClient(cfg.ChatURL(), "", cfg.Model, 0, time.Second, true, "json_schema")
	tools := tool.NewRegistry(root)
	ag := agent.New(cfg, client, tools, u)
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: "saved system"},
		{Role: llm.RoleUser, Content: "saved task"},
	}
	if err := session.Save(session.Session{Workdir: root, Messages: messages}); err != nil {
		t.Fatal(err)
	}

	command("/resume", u, ag, &cfg, client, tools)
	if ag.MessageCount() != len(messages) {
		t.Fatalf("/resume message count = %d, want %d", ag.MessageCount(), len(messages))
	}
	command("/reset", u, ag, &cfg, client, tools)
	if ag.MessageCount() != 1 {
		t.Fatalf("/reset message count = %d, want 1", ag.MessageCount())
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo"); got != "one" {
		t.Fatalf("firstLine = %q, want one", got)
	}
	long := string(make([]byte, 250))
	if got := firstLine(long); len(got) != 200 {
		t.Fatalf("firstLine length = %d, want 200", len(got))
	}
}

func TestEvalLoadRejectsBeforeSetup(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	raw, err := json.Marshal(map[string]string{
		"prompt":  "p",
		"verify":  "true",
		"workdir": outside,
		"setup":   "echo should-not-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.Load(dir); err == nil {
		t.Fatal("expected absolute workdir rejection before any setup runs")
	}
}

func TestEvalCLIParsingDoesNotRunSpecs(t *testing.T) {
	opts, err := parseEvalArgs([]string{"--allow-external-workdir", "--pass-env", "GOCACHE", "--pass-env=GOFLAGS", "specs"})
	if err != nil || opts.dir != "specs" || !opts.allowExternal || opts.help {
		t.Fatalf("eval parse = %+v err %v", opts, err)
	}
	if len(opts.passEnv) != 2 || opts.passEnv[0] != "GOCACHE" || opts.passEnv[1] != "GOFLAGS" {
		t.Fatalf("pass-env parse = %v", opts.passEnv)
	}
	opts, err = parseEvalArgs([]string{"--help"})
	if err != nil || !opts.help {
		t.Fatalf("help parse = %+v err %v", opts, err)
	}
	if _, err := parseEvalArgs([]string{"--unknown"}); err == nil || !strings.Contains(err.Error(), "unknown eval flag") {
		t.Fatalf("unknown flag error = %v", err)
	}
	if _, err := parseEvalArgs([]string{"--pass-env"}); err == nil {
		t.Fatal("missing --pass-env value should fail")
	}

	var usage bytes.Buffer
	if err := runEval([]string{"--help"}, &usage, ui.New); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(usage.String(), "Usage: drea eval") {
		t.Fatalf("usage output = %q", usage.String())
	}
}

func restoreWorktreeSeams(t *testing.T) {
	t.Helper()
	oldRequire, oldCommits := requireRepoRoot, hasCommits
	oldDirty, oldDir, oldAdd := worktreeDirty, makeWorktreeDir, addWorktree
	oldChanged, oldRemove := worktreeChanged, removeWorktree
	oldCheckpoint, oldMerge := checkpointWork, mergeFFOnly
	t.Cleanup(func() {
		requireRepoRoot, hasCommits = oldRequire, oldCommits
		worktreeDirty, makeWorktreeDir, addWorktree = oldDirty, oldDir, oldAdd
		worktreeChanged, removeWorktree = oldChanged, oldRemove
		checkpointWork, mergeFFOnly = oldCheckpoint, oldMerge
	})
}

func TestEnterWorktreeCleansUpAfterCreateFailure(t *testing.T) {
	restoreWorktreeSeams(t)
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	requireRepoRoot = func(context.Context, string) error { return nil }
	hasCommits = func(context.Context, string) bool { return true }
	worktreeDirty = func(context.Context, string) (bool, error) { return false, nil }
	makeWorktreeDir = func(string) (string, error) { return missing, nil }
	addWorktree = func(_ context.Context, repo, path, branch string) (vcs.Worktree, error) {
		return vcs.Worktree{Repo: repo, Path: path, Branch: branch}, nil
	}
	removed := false
	removeWorktree = func(context.Context, vcs.Worktree) error {
		removed = true
		return nil
	}
	cfg := config.Config{Workdir: root, BaseURL: "http://127.0.0.1:1", Model: "model"}
	if _, err := enterWorktree(&cfg, ui.New()); err == nil {
		t.Fatal("expected normalization failure")
	}
	if !removed {
		t.Fatal("created worktree was not cleaned up")
	}
}

func TestFinishWorktreeUnchangedAndChanged(t *testing.T) {
	restoreWorktreeSeams(t)
	wt := vcs.Worktree{Repo: "repo", Path: "work", Branch: "branch"}
	cfg := config.Config{}
	removed := 0
	removeWorktree = func(context.Context, vcs.Worktree) error {
		removed++
		return nil
	}
	worktreeChanged = func(context.Context, vcs.Worktree) (bool, error) { return false, nil }
	finishWorktree(wt, &cfg, agent.MeasureUnknown, ui.New())
	if removed != 1 {
		t.Fatalf("unchanged remove calls = %d", removed)
	}

	worktreeChanged = func(context.Context, vcs.Worktree) (bool, error) { return true, nil }
	finishWorktree(wt, &cfg, agent.MeasureUnknown, ui.New())
	if removed != 1 {
		t.Fatal("changed worktree was removed")
	}
}

func TestPromoteGatingAndFastForwardFailures(t *testing.T) {
	restoreWorktreeSeams(t)
	wt := vcs.Worktree{Repo: "repo", Path: "work", Branch: "branch"}
	u := ui.New()
	checkpoints := 0
	checkpointWork = func(context.Context, string, string) (string, bool, error) {
		checkpoints++
		return "ref", true, nil
	}
	cfg := config.Config{}
	if promote(context.Background(), wt, &cfg, agent.MeasurePassing, u) {
		t.Fatal("promoted without verification command")
	}
	cfg.Verify = "verify"
	if promote(context.Background(), wt, &cfg, agent.MeasureFailing, u) {
		t.Fatal("promoted failing work")
	}
	if checkpoints != 0 {
		t.Fatal("promotion gates ran checkpoint")
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"dirty", errors.New("the repository has uncommitted changes")},
		{"diverged", errors.New("git merge: not possible to fast-forward")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mergeFFOnly = func(context.Context, string, string) error { return tc.err }
			removeWorktree = func(context.Context, vcs.Worktree) error {
				t.Fatal("failed promotion removed worktree")
				return nil
			}
			if promote(context.Background(), wt, &cfg, agent.MeasurePassing, u) {
				t.Fatal("promotion succeeded despite fast-forward failure")
			}
		})
	}
}

func TestPromoteCommitsFastForwardsAndCleansUp(t *testing.T) {
	restoreWorktreeSeams(t)
	wt := vcs.Worktree{Repo: "repo", Path: "work", Branch: "branch"}
	cfg := config.Config{Verify: "verify"}
	checkpointed, merged, removed := false, false, false
	checkpointWork = func(_ context.Context, path, _ string) (string, bool, error) {
		checkpointed = path == wt.Path
		return "ref", true, nil
	}
	mergeFFOnly = func(_ context.Context, repo, branch string) error {
		merged = repo == wt.Repo && branch == wt.Branch
		return nil
	}
	removeWorktree = func(context.Context, vcs.Worktree) error {
		removed = true
		return nil
	}
	if !promote(context.Background(), wt, &cfg, agent.MeasurePassing, ui.New()) {
		t.Fatal("passing fast-forward promotion failed")
	}
	if !checkpointed || !merged || !removed {
		t.Fatalf("checkpoint=%v merge=%v remove=%v", checkpointed, merged, removed)
	}
}
