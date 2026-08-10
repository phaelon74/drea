package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/llm"
)

func call(name, args string) []llm.ToolCall {
	tc := llm.ToolCall{ID: "1", Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return []llm.ToolCall{tc}
}

func TestStallNudgesThenAborts(t *testing.T) {
	var s stallDetector
	same := call("read_file", `{"path":"a.go"}`)

	for i := 1; i < stallNudge; i++ {
		if nudge, abort := s.observe(same); nudge || abort {
			t.Fatalf("repeat %d should be tolerated: nudge=%v abort=%v", i, nudge, abort)
		}
	}
	nudge, abort := s.observe(same)
	if !nudge || abort {
		t.Fatalf("repeat %d should nudge: nudge=%v abort=%v", stallNudge, nudge, abort)
	}
	// Warned once, not on every subsequent repeat.
	if nudge, _ := s.observe(same); nudge {
		t.Fatal("the model should be nudged once, not repeatedly")
	}
	if _, abort := s.observe(same); !abort {
		t.Fatalf("repeat %d should abort", stallAbort)
	}
	if msg := s.stallMessage(); !strings.Contains(msg, "read_file") {
		t.Fatalf("stall message should name the repeated tool: %q", msg)
	}
}

// Different arguments are ordinary progress (reading two files, editing two
// places) and must never look like a stall.
func TestDifferentArgumentsAreNotAStall(t *testing.T) {
	var s stallDetector
	for i := 0; i < stallAbort*2; i++ {
		calls := call("read_file", fmt.Sprintf(`{"path":"f%d.go"}`, i))
		if nudge, abort := s.observe(calls); nudge || abort {
			t.Fatalf("distinct calls flagged as a stall at %d", i)
		}
	}
}

func TestStallResetsWhenTheModelChangesAction(t *testing.T) {
	var s stallDetector
	same := call("search", `{"query":"x"}`)
	s.observe(same)
	s.observe(same)
	s.observe(call("list_dir", `{"path":"."}`))
	for i := 1; i < stallNudge; i++ {
		if nudge, _ := s.observe(same); nudge {
			t.Fatalf("counter did not reset after a different action (repeat %d)", i)
		}
	}
}

// TestRunAbortsAStalledTask drives the real loop with a model that repeats the
// same call forever; the task must end well before MaxSteps.
func TestRunAbortsAStalledTask(t *testing.T) {
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		turns++
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		))
	}))
	defer srv.Close()

	ag := newTestAgent(t, "")
	ag.cfg.MaxSteps = 50
	ag.client = llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, "")

	if err := ag.Run(context.Background(), "loop"); err != nil {
		t.Fatal(err)
	}
	if turns != stallAbort {
		t.Fatalf("expected the task to stop after %d identical calls, got %d turns", stallAbort, turns)
	}
}
