package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	p, err := Save(Settings{BaseURL: "https://example.com/v1", Model: "m1", ReasoningEffort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "drea", "settings.json"); p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}

	got, ok, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got.BaseURL != "https://example.com/v1" || got.Model != "m1" || got.ReasoningEffort != "medium" {
		t.Fatalf("loaded %+v", got)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, ok, err := Load()
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if ok {
		t.Fatal("ok = true for missing file, want false")
	}
	if got.BaseURL != "" || got.Model != "" || got.Verify != "" || got.ContextTokens != 0 ||
		len(got.AllowCommands) != 0 || len(got.DenyCommands) != 0 {
		t.Fatalf("got %+v, want zero value", got)
	}
}

func TestLoadMalformed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "drea", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(); err == nil {
		t.Fatal("expected error for malformed settings")
	}
}

func TestSaveKeyLoadKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	p, err := SaveKey("sk-test-1234")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "drea", "key"); p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}

	got, ok, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "sk-test-1234" {
		t.Fatalf("key = %q, want %q", got, "sk-test-1234")
	}
}

func TestLoadKeyMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, ok, err := LoadKey()
	if err != nil {
		t.Fatalf("missing key file should not error: %v", err)
	}
	if ok {
		t.Fatal("ok = true for missing key file, want false")
	}
	if got != "" {
		t.Fatalf("key = %q, want empty", got)
	}
}

func TestLoadKeyTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "drea", "key")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("  sk-ws-key  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "sk-ws-key" {
		t.Fatalf("key = %q, ok = %v; want %q", got, ok, "sk-ws-key")
	}
}

func TestSaveKeyEmptyRemovesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "drea", "key")
	if _, err := SaveKey("sk-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("key file should exist: %v", err)
	}
	if _, err := SaveKey(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("key file should be removed, stat err = %v", err)
	}
	if _, err := SaveKey("   "); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("blank key file should be removed, stat err = %v", err)
	}
}
