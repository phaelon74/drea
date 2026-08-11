package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, root, name, content string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyPatchMultipleEdits(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "a.go", "package a\n\nconst X = 1\nconst Y = 2\n")
	ap := &applyPatch{root: root}
	out, err := run(t, ap, map[string]any{
		"path": "a.go",
		"edits": []map[string]any{
			{"old_string": "const X = 1", "new_string": "const X = 11"},
			{"old_string": "const Y = 2", "new_string": "const Y = 22"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 edit(s)") {
		t.Errorf("unexpected result %q", out)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "package a\n\nconst X = 11\nconst Y = 22\n" {
		t.Errorf("file content %q", data)
	}
}

// If a later edit fails, nothing may be written: the file must be byte-for-byte
// unchanged rather than half-patched.
func TestApplyPatchIsAtomicOnDisk(t *testing.T) {
	root := t.TempDir()
	const original = "alpha\nbeta\n"
	p := writeTemp(t, root, "a.txt", original)
	ap := &applyPatch{root: root}
	_, err := run(t, ap, map[string]any{
		"path": "a.txt",
		"edits": []map[string]any{
			{"old_string": "alpha", "new_string": "ALPHA"},
			{"old_string": "missing", "new_string": "x"},
		},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if data, _ := os.ReadFile(p); string(data) != original {
		t.Errorf("file was modified despite the failure: %q", data)
	}
}

func TestApplyPatchFuzzyMatch(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "a.go", "func f() {\n\treturn\n}\n")
	ap := &applyPatch{root: root}
	// Model reproduced the body with spaces instead of a tab.
	out, err := run(t, ap, map[string]any{
		"path":  "a.go",
		"edits": []map[string]any{{"old_string": "func f() {\n    return\n}", "new_string": "func f() {\n\treturn nil\n}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ignoring whitespace") {
		t.Errorf("expected the fuzzy match to be reported, got %q", out)
	}
	if data, _ := os.ReadFile(p); string(data) != "func f() {\n\treturn nil\n}\n" {
		t.Errorf("file content %q", data)
	}
}

func TestApplyPatchRejectsAmbiguity(t *testing.T) {
	root := t.TempDir()
	const original = "x = 1\nx = 1\n"
	p := writeTemp(t, root, "a.txt", original)
	ap := &applyPatch{root: root}
	if _, err := run(t, ap, map[string]any{
		"path":  "a.txt",
		"edits": []map[string]any{{"old_string": "x = 1", "new_string": "x = 2"}},
	}); err == nil {
		t.Fatal("expected ambiguity to be rejected")
	}
	if data, _ := os.ReadFile(p); string(data) != original {
		t.Errorf("file was modified: %q", data)
	}
}

func TestApplyPatchConfinementAndValidation(t *testing.T) {
	root := t.TempDir()
	ap := &applyPatch{root: root}
	if _, err := run(t, ap, map[string]any{
		"path":  "../escape.txt",
		"edits": []map[string]any{{"old_string": "a", "new_string": "b"}},
	}); err == nil {
		t.Error("expected the path escape to be rejected")
	}
	if _, err := run(t, ap, map[string]any{"path": "a.txt", "edits": []map[string]any{}}); err == nil {
		t.Error("expected empty edits to be rejected")
	}
}

// Patching must not silently change a file's permissions (e.g. a script).
func TestApplyPatchPreservesMode(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "run.sh", "echo old\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	ap := &applyPatch{root: root}
	if _, err := run(t, ap, map[string]any{
		"path":  "run.sh",
		"edits": []map[string]any{{"old_string": "echo old", "new_string": "echo new"}},
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
}

// edit_file shares the patch engine, so it gains the same whitespace tolerance.
func TestEditFileFuzzyMatch(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "a.txt", "hello world   \n")
	e := &editFile{root: root}
	if _, err := run(t, e, map[string]any{
		"path": "a.txt", "old_string": "hello world", "new_string": "hi world",
	}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); string(data) != "hi world   \n" {
		t.Errorf("file content %q", data)
	}
}

func TestParseEdits(t *testing.T) {
	path, edits, ok := ParseEdits("edit_file", []byte(`{"path":"a.txt","old_string":"a","new_string":"b"}`))
	if !ok || path != "a.txt" || len(edits) != 1 || edits[0].Old != "a" || edits[0].New != "b" {
		t.Errorf("edit_file: %q %+v %v", path, edits, ok)
	}
	path, edits, ok = ParseEdits("apply_patch", []byte(`{"path":"b.txt","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d","replace_all":true}]}`))
	if !ok || path != "b.txt" || len(edits) != 2 || !edits[1].ReplaceAll {
		t.Errorf("apply_patch: %q %+v %v", path, edits, ok)
	}
	if _, _, ok := ParseEdits("write_file", []byte(`{"path":"a","content":"x"}`)); ok {
		t.Error("write_file must not be parsed as edits")
	}
	if _, _, ok := ParseEdits("apply_patch", []byte(`{"path":"a"}`)); ok {
		t.Error("apply_patch without edits must not parse")
	}
}

func TestApplyPatchErrorUsesRelativePath(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "sub/a.txt", "hello\n")
	ap := &applyPatch{root: root}
	_, err := run(t, ap, map[string]any{
		"path":  "sub/a.txt",
		"edits": []map[string]any{{"old_string": "missing", "new_string": "x"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error should use a relative path, got %v", err)
	}
	if !strings.Contains(err.Error(), "sub/a.txt") {
		t.Fatalf("error should name the relative path, got %v", err)
	}
}

func TestWriteFileRejectsOversizeContent(t *testing.T) {
	root := t.TempDir()
	w := &writeFile{root: root}
	big := strings.Repeat("x", maxFileBytes+1)
	if _, err := run(t, w, map[string]any{"path": "big.txt", "content": big}); err == nil {
		t.Fatal("expected oversize write to be rejected")
	}
}

func TestReadFileRejectsOversizeFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, maxFileBytes+1)); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	r := &readFile{root: root}
	if _, err := run(t, r, map[string]any{"path": "big.txt"}); err == nil {
		t.Fatal("expected oversize read to be rejected")
	}
}

func TestWriteFileRejectsNonRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &writeFile{root: root}
	if _, err := run(t, w, map[string]any{"path": "dir", "content": "x"}); err == nil {
		t.Fatal("expected write to a directory to be rejected")
	}
}

func TestFileToolsRejectFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := writeTemp(t, root, "target.txt", "old\n")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := run(t, &readFile{root: root}, map[string]any{"path": "link.txt"}); err == nil {
		t.Error("read_file accepted a final symlink")
	}
	if _, err := run(t, &writeFile{root: root}, map[string]any{"path": "link.txt", "content": "new\n"}); err == nil {
		t.Error("write_file accepted a final symlink")
	}
	if _, err := run(t, &editFile{root: root}, map[string]any{"path": "link.txt", "old_string": "old", "new_string": "new"}); err == nil {
		t.Error("edit_file accepted a final symlink")
	}
}

func TestWriteFilePreservesExecutableMode(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "run.sh", "old\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, &writeFile{root: root}, map[string]any{"path": "run.sh", "content": "new\n"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", info.Mode().Perm())
	}
}

func TestSecureWriteRejectsDestinationReplacement(t *testing.T) {
	root := t.TempDir()
	p := writeTemp(t, root, "file.txt", "first")
	mode, expected, err := inspectSecureTarget(root, p)
	if err != nil {
		t.Fatal(err)
	}
	replacement := writeTemp(t, root, "replacement.txt", "second")
	if err := os.Rename(replacement, p); err != nil {
		t.Fatal(err)
	}
	if err := secureWriteAtomic(root, p, []byte("third"), mode, expected); err == nil {
		t.Fatal("expected replaced destination to be rejected")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("replacement was overwritten: %q", data)
	}
}

func TestSecureWriteRejectsAppearingDestination(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "new.txt")
	mode, expected, err := inspectSecureTarget(root, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hostile"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := secureWriteAtomic(root, p, []byte("wanted"), mode, expected); err == nil {
		t.Fatal("expected appearing destination to be rejected")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hostile" {
		t.Fatalf("appearing destination was overwritten: %q", data)
	}
}
