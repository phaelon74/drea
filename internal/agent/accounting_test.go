package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

type resultClient struct {
	result *llm.Result
	err    error
}

func (c resultClient) Stream(context.Context, []llm.Message, []llm.Tool, llm.Handlers) (*llm.Result, error) {
	return c.result, c.err
}

func (c resultClient) StreamWithOptions(context.Context, []llm.Message, []llm.Tool, llm.Handlers, llm.StreamOpts) (*llm.Result, error) {
	return c.result, c.err
}

func (resultClient) JSONMode() bool { return false }

func TestRunAccountsAllTerminalAttemptOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		err      error
	}{
		{"retry success", 2, nil},
		{"retry exhaustion", 6, errors.New("giving up")},
		{"nonretry failure", 1, errors.New("bad request")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults(config.Saved{})
			cfg.Workdir = t.TempDir()
			cfg.Persist = false
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			client := resultClient{
				result: &llm.Result{
					Content:  "done",
					Usage:    llm.Usage{PromptTokens: 7, CompletionTokens: 3},
					Attempts: tc.attempts,
				},
				err: tc.err,
			}
			ag := New(cfg, nil, tool.NewRegistry(cfg.Workdir), ui.New())
			ag.client = client

			err := ag.Run(context.Background(), "work")
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("Run error = %v, want error %v", err, tc.err != nil)
			}
			got := ag.Usage()
			if got.PromptTokens != 7 || got.CompletionTokens != 3 || got.Requests != tc.attempts {
				t.Fatalf("usage = %+v, want 7/3 over %d requests", got, tc.attempts)
			}
		})
	}
}
