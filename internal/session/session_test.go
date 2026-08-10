package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dreaagent/drea/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	wd := "/some/workspace"
	in := Session{
		Workdir: wd,
		Model:   "gpt-4o",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "sys"},
			{Role: llm.RoleUser, Content: "do a thing"},
			{Role: llm.RoleAssistant, Content: "done"},
		},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}

	got, ok, err := Load(wd)
	if err != nil || !ok {
		t.Fatalf("Load ok=%v err=%v", ok, err)
	}
	if got.Workdir != wd || got.Model != "gpt-4o" || len(got.Messages) != 3 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Updated.IsZero() {
		t.Error("Updated timestamp not set")
	}
}

func TestLoadMissingReturnsNotOK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if _, ok, err := Load("/never/saved"); ok || err != nil {
		t.Fatalf("expected ok=false err=nil, got ok=%v err=%v", ok, err)
	}
}

func TestSaveExcludesAPIKeyAndUses0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	wd := "/ws"
	// A secret only ever appears in memory; it must never reach the transcript.
	if err := Save(Session{Workdir: wd, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	p, err := pathFor(wd)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"api_key", "apikey", "APIKey", "Authorization", "Bearer"} {
		if strings.Contains(string(data), k) {
			t.Errorf("transcript unexpectedly contains %q", k)
		}
	}
}

func TestPathPerWorkdirDiffers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	a, _ := pathFor("/a")
	b, _ := pathFor("/b")
	if a == b {
		t.Fatal("different workdirs must map to different files")
	}
	if filepath.Dir(a) != filepath.Dir(b) {
		t.Fatal("transcripts should live in the same directory")
	}
}
