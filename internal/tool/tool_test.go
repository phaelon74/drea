package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConfinement(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"a.txt", false},
		{"sub/b.txt", false},
		{".", false},
		{"../escape.txt", true},
		{"sub/../../escape.txt", true},
		{"/etc/passwd", true},
	}
	for _, tc := range cases {
		_, err := resolve(root, tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("resolve(%q): err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestResolveRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolve(root, inside)
	if err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
	if !strings.Contains(err.Error(), "relative") {
		t.Fatalf("error should mention relative paths, got %v", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the workspace pointing outside it must not be a way out.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(root, "link/secret.txt"); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
	// A symlink that stays inside the workspace is still allowed.
	inside := filepath.Join(root, "real")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "innerlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(root, "innerlink/ok.txt"); err != nil {
		t.Fatalf("in-workspace symlink should be allowed: %v", err)
	}
}

func TestRunCommandOutputCap(t *testing.T) {
	root := t.TempDir()
	rc := &runCommand{root: root}
	// Produce far more than maxOutputBytes; result must be bounded and flagged.
	out, err := run(t, rc, map[string]any{"command": "yes x | head -c 500000"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("expected truncation marker, got %d bytes", len(out))
	}
	if len(out) > maxOutputBytes+256 {
		t.Fatalf("captured output not bounded: %d bytes", len(out))
	}
}

func TestRunCommandTimeout(t *testing.T) {
	root := t.TempDir()
	rc := &runCommand{root: root}
	out, err := run(t, rc, map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected timeout marker, got %q", out)
	}
}

func run(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), json.RawMessage(raw))
}

func TestWriteReadEdit(t *testing.T) {
	root := t.TempDir()
	w := &writeFile{root: root}
	if _, err := run(t, w, map[string]any{"path": "pkg/x.txt", "content": "one two two"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "pkg/x.txt")); string(data) != "one two two" {
		t.Fatalf("write produced %q", data)
	}

	e := &editFile{root: root}
	// Non-unique without replace_all should error.
	if _, err := run(t, e, map[string]any{"path": "pkg/x.txt", "old_string": "two", "new_string": "2"}); err == nil {
		t.Error("expected error on ambiguous edit")
	}
	if _, err := run(t, e, map[string]any{"path": "pkg/x.txt", "old_string": "two", "new_string": "2", "replace_all": true}); err != nil {
		t.Fatal(err)
	}
	r := &readFile{root: root}
	out, err := run(t, r, map[string]any{"path": "pkg/x.txt"})
	if err != nil || !strings.Contains(out, "one 2 2") {
		t.Fatalf("read got %q err %v", out, err)
	}
}

func TestSearch(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\nfunc Foo() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("Foo in text\n"), 0o644)

	s := &search{root: root}
	out, err := run(t, s, map[string]any{"pattern": "Foo", "glob": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2") || strings.Contains(out, "b.txt") {
		t.Fatalf("search glob failed: %q", out)
	}
}

func TestSearchSkipsSymlinkToOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := "TOPSECRET_UNIQUE_MARKER"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &search{root: root}
	out, err := run(t, s, map[string]any{"pattern": "TOPSECRET|safe"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("search leaked symlink target content: %q", out)
	}
	if !strings.Contains(out, "ok.txt") {
		t.Fatalf("expected in-workspace match, got %q", out)
	}
}

func TestSearchLineLengthLimit(t *testing.T) {
	root := t.TempDir()
	exact := strings.Repeat("x", maxSearchLineBytes)
	if err := os.WriteFile(filepath.Join(root, "exact.txt"), []byte(exact+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, &search{root: root}, map[string]any{"pattern": "^x+$", "path": "exact.txt"})
	if err != nil {
		t.Fatalf("exact-limit line rejected: %v", err)
	}
	if !strings.Contains(out, "exact.txt:1:") {
		t.Fatalf("exact-limit line not matched: %q", out)
	}

	if err := os.WriteFile(filepath.Join(root, "over.txt"), []byte(strings.Repeat("x", maxSearchLineBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &search{root: root}, map[string]any{"pattern": "x", "path": "over.txt"}); err == nil {
		t.Fatal("expected one-over-limit line to be rejected")
	}
}

func TestSearchManyShortLines(t *testing.T) {
	root := t.TempDir()
	var content strings.Builder
	for i := 0; i < 50000; i++ {
		content.WriteString("short\n")
	}
	content.WriteString("unique-marker\n")
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, &search{root: root}, map[string]any{"pattern": "unique-marker"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "many.txt:50001:unique-marker") {
		t.Fatalf("final short-line match missing: %q", out)
	}
}

func TestRunCommand(t *testing.T) {
	root := t.TempDir()
	rc := &runCommand{root: root}
	out, err := run(t, rc, map[string]any{"command": "echo hello && pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "[exit code 0]") {
		t.Fatalf("run_command output: %q", out)
	}

	out, _ = run(t, rc, map[string]any{"command": "exit 3"})
	if !strings.Contains(out, "[exit code 3]") {
		t.Fatalf("expected exit code 3, got %q", out)
	}
}

func TestRegistrySpecs(t *testing.T) {
	r := NewRegistry(t.TempDir())
	specs := r.Specs()
	if len(specs) != 12 {
		t.Fatalf("got %d specs, want 12", len(specs))
	}
	for _, s := range specs {
		var js map[string]any
		if err := json.Unmarshal(s.Function.Parameters, &js); err != nil {
			t.Errorf("tool %s has invalid schema: %v", s.Function.Name, err)
		}
	}
}

func TestReplyTool(t *testing.T) {
	r := NewRegistry(t.TempDir())
	tool, ok := r.Get("reply")
	if !ok {
		t.Fatal("reply tool not registered")
	}
	if tool.Mutating() {
		t.Error("reply tool should not be mutating")
	}
	if tool.AlwaysConfirm() {
		t.Error("reply tool should not always confirm")
	}
	out, err := run(t, tool, map[string]any{"message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("reply tool returned %q, want empty", out)
	}
	if got := tool.Summary(json.RawMessage(`{"message":"hello"}`)); got != "hello" {
		t.Errorf("summary = %q, want hello", got)
	}
	if got := tool.Summary(json.RawMessage(`{}`)); got != "(no message)" {
		t.Errorf("empty summary = %q, want (no message)", got)
	}
}

func TestGitRollbackAlwaysConfirm(t *testing.T) {
	r := NewRegistry(t.TempDir())
	rb, ok := r.Get("git_rollback")
	if !ok {
		t.Fatal("git_rollback not registered")
	}
	if !rb.AlwaysConfirm() {
		t.Fatal("git_rollback must AlwaysConfirm")
	}
	// Decision logic used by agent.dispatch: AlwaysConfirm wins over AutoApprove.
	autoApprove := true
	requireApproval := (rb.Mutating() && !autoApprove) || rb.AlwaysConfirm()
	if !requireApproval {
		t.Fatal("git_rollback must prompt even when AutoApprove is true")
	}
	wc, ok := r.Get("write_file")
	if !ok {
		t.Fatal("write_file not registered")
	}
	if wc.AlwaysConfirm() {
		t.Fatal("write_file must not AlwaysConfirm")
	}
	requireApproval = (wc.Mutating() && !autoApprove) || wc.AlwaysConfirm()
	if requireApproval {
		t.Fatal("write_file should not require approval when AutoApprove is true")
	}
}

func TestScrubEnvRemovesAPIKey(t *testing.T) {
	t.Setenv("DREA_API_KEY", "secret-should-not-leak")
	t.Setenv("OPENAI_API_KEY", "openai-should-not-leak")
	root := t.TempDir()
	rc := &runCommand{root: root}
	out, err := run(t, rc, map[string]any{"command": "printenv DREA_API_KEY; printenv OPENAI_API_KEY; true"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret-should-not-leak") || strings.Contains(out, "openai-should-not-leak") {
		t.Fatalf("API key leaked into child env: %q", out)
	}
}

func TestPassEnvAllowsListedButNotAPIKeys(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-pass-through")
	t.Setenv("DREA_API_KEY", "drea-never")
	SetPassEnv([]string{"GH_TOKEN", "DREA_API_KEY"})
	defer SetPassEnv(nil)

	root := t.TempDir()
	rc := &runCommand{root: root}
	out, err := run(t, rc, map[string]any{"command": "printenv GH_TOKEN; printenv DREA_API_KEY; true"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gh-pass-through") {
		t.Fatalf("expected GH_TOKEN to pass through, got %q", out)
	}
	if strings.Contains(out, "drea-never") {
		t.Fatalf("DREA_API_KEY must never pass through, got %q", out)
	}
}

func TestRegistryReadFileLimitedBoundsAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry(root)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, exists, err := registry.ReadFileLimited("large.txt", 3)
	if err != nil || !exists || string(data) != "1234" {
		t.Fatalf("limited read = %q exists=%v err=%v", data, exists, err)
	}
	if data, exists, err := registry.ReadFileLimited("missing.txt", 3); err != nil || exists || data != nil {
		t.Fatalf("missing read = %q exists=%v err=%v", data, exists, err)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := registry.ReadFileLimited("link.txt", 100); err == nil {
		t.Fatal("expected symlink read rejection")
	}
}
