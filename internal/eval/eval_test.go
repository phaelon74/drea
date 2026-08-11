package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadParsesAndDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "b.json", `{"prompt":"p2","verify":"true"}`)
	write(t, dir, "a.json", `{"name":"first","prompt":"p1","verify":"true","workdir":"sub"}`)
	write(t, dir, "ignore.txt", "not a spec")

	specs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	// Sorted by filename: a.json before b.json.
	if specs[0].Name != "first" {
		t.Errorf("name = %q, want first", specs[0].Name)
	}
	if specs[0].Workdir != filepath.Join(dir, "sub") {
		t.Errorf("relative workdir not resolved: %q", specs[0].Workdir)
	}
	// b.json has no name/workdir -> defaults derived.
	if specs[1].Name != "b" {
		t.Errorf("default name = %q, want b", specs[1].Name)
	}
	if specs[1].Workdir != dir {
		t.Errorf("default workdir = %q, want %q", specs[1].Workdir, dir)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bad.json", `{"prompt":"only prompt"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for missing verify")
	}
}

func TestLoadEmptyDirErrors(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected error for a directory with no specs")
	}
}

func TestLoadRejectsWorkdirEscape(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "escape.json", `{"prompt":"p","verify":"true","workdir":"../outside"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected escape to be rejected")
	}
}

func TestLoadRejectsMissingAndNonDirectoryWorkdir(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "missing.json", `{"prompt":"p","verify":"true","workdir":"missing"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected missing workdir to be rejected")
	}
	if err := os.Remove(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "file.json", `{"prompt":"p","verify":"true","workdir":"file"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected file workdir to be rejected")
	}
}

func TestLoadRejectsRelativeSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write(t, dir, "escape.json", `{"prompt":"p","verify":"true","workdir":"linked"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
	specs, err := LoadWithOptions(dir, LoadOptions{AllowExternalWorkdir: true})
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Workdir != outside {
		t.Fatalf("workdir = %q, want canonical %q", specs[0].Workdir, outside)
	}
}

func TestLoadRejectsAbsoluteWorkdir(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	body := fmt.Sprintf(`{"prompt":"p","verify":"true","workdir":%q}`, outside)
	write(t, dir, "abs.json", body)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected absolute workdir to be rejected")
	}
	specs, err := LoadWithOptions(dir, LoadOptions{AllowExternalWorkdir: true})
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Workdir != outside {
		t.Fatalf("workdir = %q, want %q", specs[0].Workdir, outside)
	}
}

func TestSummaryAndPassed(t *testing.T) {
	rs := []Result{
		{Name: "ok", Passed: true},
		{Name: "nope", Detail: "verify failed"},
	}
	s := Summary(rs)
	if !strings.Contains(s, "[PASS] ok") || !strings.Contains(s, "[FAIL] nope") {
		t.Errorf("summary missing marks: %q", s)
	}
	if !strings.Contains(s, "1/2 tasks passed") {
		t.Errorf("summary missing count: %q", s)
	}
	if Passed(rs) {
		t.Error("Passed should be false when a task failed")
	}
	if !Passed([]Result{{Name: "x", Passed: true}}) {
		t.Error("Passed should be true when all pass")
	}
	if Passed(nil) {
		t.Error("Passed should be false for no results")
	}
}

func TestLoadRejectsOversizedSpec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxSpecSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOptions(dir, LoadOptions{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized spec error = %v", err)
	}
}
