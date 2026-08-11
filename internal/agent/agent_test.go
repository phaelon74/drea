package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

// TestAgentLoopExecutesToolThenStops runs the full loop against a scripted SSE
// endpoint: turn 1 asks to write a file, turn 2 gives a final answer.
func TestAgentLoopExecutesToolThenStops(t *testing.T) {
	root := t.TempDir()

	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turn++
		if turn == 1 {
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"hello.txt\",\"content\":\"hi\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"Done, wrote hello.txt."}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()

	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = root
	cfg.AutoApprove = true
	cfg.Persist = false
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	client := llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")
	ag := New(cfg, client, tool.NewRegistry(root), ui.New())

	if err := ag.Run(context.Background(), "make hello.txt"); err != nil {
		t.Fatal(err)
	}
	if turn != 2 {
		t.Fatalf("expected 2 model turns, got %d", turn)
	}
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("file content = %q err %v", data, err)
	}
	// Conversation should contain the tool result message.
	var sawTool bool
	for _, m := range ag.messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "hello.txt") {
			sawTool = true
		}
	}
	if !sawTool {
		t.Error("tool result not recorded in conversation")
	}
}

// TestCommandPolicyBlocksDeniedCommand verifies a run_command matching the deny
// policy is refused without executing, and the refusal is fed back to the model.
// TestAgentReplyToolEndsTurn verifies that a native "reply" tool call is
// treated as assistant prose and ends the turn without being dispatched.
func TestAgentReplyToolEndsTurn(t *testing.T) {
	root := t.TempDir()

	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turn++
		if turn == 1 {
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"reply","arguments":"{\"message\":\"Hello from the reply tool.\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			))
			return
		}
		t.Error("unexpected second turn")
	}))
	defer srv.Close()

	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = root
	cfg.AutoApprove = true
	cfg.Persist = false
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	client := llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")
	ag := New(cfg, client, tool.NewRegistry(root), ui.New())

	if err := ag.Run(context.Background(), "say hello"); err != nil {
		t.Fatal(err)
	}
	if turn != 1 {
		t.Fatalf("expected 1 model turn, got %d", turn)
	}
	last := ag.messages[len(ag.messages)-1]
	if last.Role != llm.RoleAssistant || last.Content != "Hello from the reply tool." {
		t.Fatalf("expected assistant reply message, got %+v", last)
	}
}

func TestCommandPolicyBlocksDeniedCommand(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "created.txt")

	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turn++
		if turn == 1 {
			// Ask to run a command the deny policy forbids.
			fmt.Fprint(w, sse(
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"touch `+"created.txt"+`\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			))
			return
		}
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"ok"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()

	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = root
	cfg.AutoApprove = true // even with auto-approve, deny must win
	cfg.Persist = false
	cfg.DenyCommands = []string{`\btouch\b`}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	client := llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")
	ag := New(cfg, client, tool.NewRegistry(root), ui.New())

	if err := ag.Run(context.Background(), "touch a file"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied command was executed (marker exists): %v", err)
	}
	var sawBlock bool
	for _, m := range ag.messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "blocked by the configured command policy") {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Error("policy-deny feedback not recorded in conversation")
	}
}

func TestShortErrStripsControl(t *testing.T) {
	err := fmt.Errorf("endpoint returned 429: \x1b[31malert\x1b[0m\x07rate limited")
	got := shortErr(err)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("shortErr left control characters: %q", got)
	}
	if !strings.Contains(got, "alert") || !strings.Contains(got, "rate limited") {
		t.Errorf("shortErr dropped visible text: %q", got)
	}
}

func TestApprovalCommandLines(t *testing.T) {
	lines := approvalCommandLines("echo one\necho two")
	if len(lines) != 2 || lines[0] != "echo one" || lines[1] != "echo two" {
		t.Fatalf("multiline = %#v", lines)
	}
	big := strings.Repeat("A", maxApprovalCommandBytes+500) + "TAILMARK"
	out := strings.Join(approvalCommandLines(big), "\n")
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker in %q", out[:80])
	}
	if !strings.Contains(out, "TAILMARK") {
		t.Fatal("expected visible tail after truncation")
	}
	if strings.HasPrefix(out, strings.Repeat("A", 100)) == false {
		t.Fatal("expected visible head after truncation")
	}
}

func TestDispatchApprovalMatrix(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = root
	cfg.Persist = false
	cfg.AllowCommands = []string{`^true$`}
	cfg.DenyCommands = []string{`^false$`}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	ag := New(cfg, nil, tool.NewRegistry(root), ui.New())

	var confirms int
	ag.confirm = func(string) bool {
		confirms++
		return false
	}

	// User denial blocks a normal mutation.
	write := llm.ToolCall{Function: llm.FunctionCall{
		Name:      "write_file",
		Arguments: `{"path":"denied.txt","content":"no"}`,
	}}
	if got := ag.dispatch(context.Background(), write); !strings.Contains(got, "denied") {
		t.Fatalf("denial result = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write reached filesystem: %v", err)
	}

	// Turning auto off affects the very next dispatch.
	ag.SetAutoApprove(true)
	ag.SetAutoApprove(false)
	ag.dispatch(context.Background(), write)
	if confirms != 2 {
		t.Fatalf("next dispatch did not prompt after SetAutoApprove(false): %d confirmations", confirms)
	}

	// Destructive rollback always confirms even under auto approval.
	ag.SetAutoApprove(true)
	rollback := llm.ToolCall{Function: llm.FunctionCall{
		Name:      "git_rollback",
		Arguments: `{"ref":"HEAD"}`,
	}}
	if got := ag.dispatch(context.Background(), rollback); !strings.Contains(got, "denied") {
		t.Fatalf("rollback denial result = %q", got)
	}
	if confirms != 3 {
		t.Fatalf("rollback under auto did not confirm: %d confirmations", confirms)
	}

	// Deny policy takes precedence and never asks.
	deniedCommand := llm.ToolCall{Function: llm.FunctionCall{
		Name:      "run_command",
		Arguments: `{"command":"false"}`,
	}}
	if got := ag.dispatch(context.Background(), deniedCommand); !strings.Contains(got, "blocked") {
		t.Fatalf("deny result = %q", got)
	}
	if confirms != 3 {
		t.Fatalf("deny policy unexpectedly prompted: %d confirmations", confirms)
	}

	if runtime.GOOS == "linux" {
		// A full-command allow match bypasses normal approval.
		allowed := llm.ToolCall{Function: llm.FunctionCall{
			Name:      "run_command",
			Arguments: `{"command":"true"}`,
		}}
		if got := ag.dispatch(context.Background(), allowed); strings.Contains(got, "denied") || strings.Contains(got, "blocked") {
			t.Fatalf("allow result = %q", got)
		}
		if confirms != 3 {
			t.Fatalf("allow policy unexpectedly prompted: %d confirmations", confirms)
		}
	}
}

func TestDiffLineCountMatchesDiffSplitting(t *testing.T) {
	tests := map[string]int{
		"":       0,
		"a":      1,
		"a\n":    1,
		"a\n\n":  2,
		"a\nb\n": 2,
	}
	for input, want := range tests {
		if got := diffLineCount(input); got != want {
			t.Errorf("diffLineCount(%q) = %d, want %d", input, got, want)
		}
	}
}

func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&b, "data: %s\n\n", f)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}
