package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/session"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

func TestRunStopsAtMaxSteps(t *testing.T) {
	var turns int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`,
		))
	}))
	defer srv.Close()

	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = t.TempDir()
	cfg.Persist = false
	cfg.MaxSteps = 3
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	ag := New(cfg, llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, ""), tool.NewRegistry(cfg.Workdir), ui.New())
	if err := ag.Run(context.Background(), "keep looking"); err != nil {
		t.Fatal(err)
	}
	if turns != cfg.MaxSteps {
		t.Fatalf("model turns = %d, want MaxSteps %d", turns, cfg.MaxSteps)
	}
}

func TestRunPersistsSessionTranscript(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("APPDATA", configDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(
			`{"choices":[{"delta":{"content":"persisted answer"},"finish_reason":"stop"}]}`,
		))
	}))
	defer srv.Close()

	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = t.TempDir()
	cfg.Persist = true
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	ag := New(cfg, llm.NewClient(srv.URL, "", "m", 0, 5*time.Second, false, ""), tool.NewRegistry(cfg.Workdir), ui.New())
	if err := ag.Run(context.Background(), "persist me"); err != nil {
		t.Fatal(err)
	}

	saved, ok, err := session.Load(cfg.Workdir)
	if err != nil || !ok {
		t.Fatalf("Load() ok=%v err=%v", ok, err)
	}
	if saved.Workdir != cfg.Workdir || len(saved.Messages) != ag.MessageCount() {
		t.Fatalf("saved session = %+v, agent messages = %d", saved, ag.MessageCount())
	}
	last := saved.Messages[len(saved.Messages)-1]
	if last.Role != llm.RoleAssistant || last.Content != "persisted answer" {
		t.Fatalf("saved final message = %+v", last)
	}
}
