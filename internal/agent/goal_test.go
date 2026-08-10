package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/vcs"
)

// gitWorkspace turns the agent's workspace into a repository with one commit.
func gitWorkspace(t *testing.T, ag *Agent) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := ag.cfg.Workdir
	cmd := exec.Command("git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vcs.Checkpoint(context.Background(), dir, "base"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitOut runs a git command in dir and returns its output.
func gitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func TestBeginTaskRecordsCheckpointAndBaseline(t *testing.T) {
	ag := newTestAgent(t, "true")
	ag.cfg.Checkpoint = true
	dir := gitWorkspace(t, ag)

	// An uncommitted file must end up inside the checkpoint, so rolling back
	// to it does not lose work that existed before the task.
	if err := os.WriteFile(filepath.Join(dir, "pending.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.beginTask(context.Background(), "do something")

	if ag.checkpoint == "" {
		t.Fatal("no checkpoint recorded")
	}
	if ag.baseline != MeasurePassing {
		t.Fatalf("baseline = %v, want passing", ag.baseline)
	}
	if dirty, err := vcs.Dirty(context.Background(), dir); err != nil || dirty {
		t.Fatalf("checkpoint should have committed pending work: dirty=%v err=%v", dirty, err)
	}
}

func TestBeginTaskRecordsAnAlreadyFailingBaseline(t *testing.T) {
	ag := newTestAgent(t, "false")
	ag.cfg.Checkpoint = true
	gitWorkspace(t, ag)

	ag.beginTask(context.Background(), "fix the failure")
	if ag.baseline != MeasureFailing {
		t.Fatalf("baseline = %v, want failing", ag.baseline)
	}
}

func TestBeginTaskWithoutCheckpointingDoesNothing(t *testing.T) {
	ag := newTestAgent(t, "true")
	gitWorkspace(t, ag)

	ag.beginTask(context.Background(), "task")
	if ag.checkpoint != "" || ag.baseline != MeasureUnknown {
		t.Fatalf("checkpointing is off: checkpoint=%q baseline=%v", ag.checkpoint, ag.baseline)
	}
}

func TestBeginTaskOutsideARepositoryIsNotFatal(t *testing.T) {
	ag := newTestAgent(t, "true")
	ag.cfg.Checkpoint = true

	ag.beginTask(context.Background(), "task") // plain temp dir, no repo
	if ag.checkpoint != "" {
		t.Fatal("no checkpoint is possible outside a repository")
	}
}

// TestRegressionIsRolledBack is the whole point of the goal loop: the measure
// passed before the task, the task broke it, and after the self-correction
// budget is spent the change is undone.
func TestRegressionIsRolledBack(t *testing.T) {
	ag := newTestAgent(t, "test ! -f broken")
	ag.cfg.Checkpoint = true
	ag.cfg.AutoApprove = true // no interactive confirmation in a test
	ag.cfg.VerifyAttempts = 1
	dir := gitWorkspace(t, ag)

	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turns++
		if turns == 1 {
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"broken\",\"content\":\"x\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"done"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()
	ag.client = llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")

	if err := ag.Run(context.Background(), "break it"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "broken")); !os.IsNotExist(err) {
		t.Fatalf("regression was not rolled back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("rollback destroyed pre-existing work: %v", err)
	}
	// Undone, but not destroyed: the attempt is still reachable for review.
	if out, err := gitOut(t, dir, "log", "--all", "--format=%s"); err != nil ||
		!strings.Contains(out, "attempt rolled back") {
		t.Fatalf("the rolled-back attempt was not preserved: %q %v", out, err)
	}
	if ag.Objective() != MeasureFailing {
		t.Fatalf("objective = %v, want failing", ag.Objective())
	}
}

// A measure that was already failing cannot be regressed, so work is kept:
// undoing an attempted fix that did not land would throw away progress.
func TestAlreadyFailingBaselineIsNotRolledBack(t *testing.T) {
	ag := newTestAgent(t, "test -f fixed")
	ag.cfg.Checkpoint = true
	ag.cfg.AutoApprove = true
	ag.cfg.VerifyAttempts = 1
	dir := gitWorkspace(t, ag)

	ag.beginTask(context.Background(), "try to fix")
	if ag.baseline != MeasureFailing {
		t.Fatalf("baseline = %v, want failing", ag.baseline)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.verifyRounds = ag.cfg.VerifyAttempts
	if _, retry := ag.verify(context.Background()); retry {
		t.Fatal("budget is spent; no retry expected")
	}
	if _, err := os.Stat(filepath.Join(dir, "partial.txt")); err != nil {
		t.Fatalf("work was discarded despite no regression: %v", err)
	}
}

func TestUsageAccumulatesAcrossRequests(t *testing.T) {
	ag := newTestAgent(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()
	ag.client = llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")

	for i := 0; i < 2; i++ {
		if err := ag.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	got := ag.Usage()
	if got.PromptTokens != 20 || got.CompletionTokens != 8 || got.Requests != 2 {
		t.Fatalf("session usage = %+v, want 20/8 over 2 requests", got)
	}
	// Per-task usage is reset each Run, so it reports the last task only.
	if ag.task.PromptTokens != 10 {
		t.Fatalf("task usage = %+v, want 10 prompt tokens", ag.task)
	}
}
