package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

func TestRepeatedEmptyFormsGetExactlyOneNudge(t *testing.T) {
	tests := []struct {
		name     string
		jsonMode bool
		frame    string
	}{
		{"native content", false, `{"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`},
		{"JSON array", true, `{"choices":[{"delta":{"content":"[]"},"finish_reason":"stop"}]}`},
		{"native reply tool", false, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"reply","arguments":"{\"message\":\"\"}"}}]},"finish_reason":"tool_calls"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var turn int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				turn++
				fmt.Fprint(w, sse(tc.frame))
			}))
			defer srv.Close()

			cfg := config.Defaults(config.Saved{})
			cfg.Workdir = root
			cfg.AutoApprove = true
			cfg.Persist = false
			cfg.MaxSteps = 10
			cfg.Verify = "false"
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			client := llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, tc.jsonMode, "")
			ag := New(cfg, client, tool.NewRegistry(root), ui.New())
			err := ag.Run(context.Background(), "do something")
			if err == nil || !strings.Contains(err.Error(), "repeated empty") {
				t.Fatalf("expected repeated empty error, got %v", err)
			}
			if turn != 2 {
				t.Fatalf("expected 2 model turns, got %d", turn)
			}
			var nudges int
			for _, m := range ag.messages {
				if m.Role == llm.RoleUser && strings.Contains(m.Content, "last response was empty") {
					nudges++
				}
			}
			if nudges != 1 {
				t.Fatalf("got %d corrective nudges, want 1", nudges)
			}
			if ag.verifyRounds != 0 {
				t.Fatalf("empty turns reached verification %d time(s)", ag.verifyRounds)
			}
		})
	}
}
