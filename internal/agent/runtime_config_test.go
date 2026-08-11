package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

func TestRuntimeSettersUpdateAgentConfig(t *testing.T) {
	cfg := config.Defaults(config.Saved{})
	cfg.Workdir = t.TempDir()
	cfg.AutoApprove = true
	cfg.Verify = "true"
	cfg.Checkpoint = true
	cfg.Persist = false
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	ag := New(cfg, llm.NewClient("http://127.0.0.1:1/v1/chat/completions", "", "m", 0, 0, false, ""), tool.NewRegistry(cfg.Workdir), ui.New())

	if !ag.AutoApprove() || ag.VerifyCommand() != "true" || !ag.Checkpointing() {
		t.Fatalf("initial state wrong: auto=%v verify=%q check=%v", ag.AutoApprove(), ag.VerifyCommand(), ag.Checkpointing())
	}
	ag.SetAutoApprove(false)
	ag.SetVerify("go test ./...")
	ag.SetCheckpoint(false)
	if ag.AutoApprove() || ag.VerifyCommand() != "go test ./..." || ag.Checkpointing() {
		t.Fatalf("setters did not update agent config: auto=%v verify=%q check=%v", ag.AutoApprove(), ag.VerifyCommand(), ag.Checkpointing())
	}
}

func TestRuntimeVerifyAndCheckpointApplyToNextRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires the Linux shell contract")
	}
	ag := newTestAgent(t, "")
	gitWorkspace(t, ag)
	if err := os.WriteFile(filepath.Join(ag.cfg.Workdir, "keep.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.client = resultClient{result: &llm.Result{Content: "done"}}
	ag.SetVerify("printf verified > verified.txt")
	ag.SetCheckpoint(true)

	if err := ag.Run(context.Background(), "finish"); err != nil {
		t.Fatal(err)
	}
	if ag.checkpoint == "" {
		t.Fatal("SetCheckpoint(true) did not affect the next Run")
	}
	data, err := os.ReadFile(filepath.Join(ag.cfg.Workdir, "verified.txt"))
	if err != nil || string(data) != "verified" {
		t.Fatalf("SetVerify did not affect the next Run: %q %v", data, err)
	}
}
