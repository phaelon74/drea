package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFindsFilesInPriorityOrder(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "readme body")
	write(t, root, "AGENTS.md", "agents body")
	write(t, root, "CONVENTIONS.md", "conventions body")
	write(t, root, "unrelated.md", "should not be read")

	docs := Load(root)
	var names []string
	for _, d := range docs {
		names = append(names, d.Name)
	}
	want := []string{"AGENTS.md", "CONVENTIONS.md", "README.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", names, want)
	}
	for _, d := range docs {
		if d.Truncated {
			t.Errorf("%s should not be truncated", d.Name)
		}
	}
}

func TestLoadMissingFilesIsEmpty(t *testing.T) {
	if docs := Load(t.TempDir()); len(docs) != 0 {
		t.Fatalf("expected no docs, got %v", docs)
	}
	if docs := Load(filepath.Join(t.TempDir(), "nonexistent")); len(docs) != 0 {
		t.Fatalf("expected no docs for a missing root, got %v", docs)
	}
	if Prompt(nil) != "" {
		t.Error("Prompt of no docs must be empty")
	}
}

func TestLoadIsCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "readme.md", "lower case readme")
	docs := Load(root)
	if len(docs) != 1 || docs[0].Name != "readme.md" {
		t.Fatalf("got %+v", docs)
	}
}

func TestLoadBoundsFileAndTotalSize(t *testing.T) {
	root := t.TempDir()
	huge := strings.Repeat("line of project docs\n", 2000) // ~42 KiB
	write(t, root, "AGENTS.md", huge)
	write(t, root, "CONVENTIONS.md", huge)
	write(t, root, "README.md", huge)

	docs := Load(root)
	total := 0
	for _, d := range docs {
		if len(d.Content) > MaxFileBytes {
			t.Errorf("%s is %d bytes, over the per-file cap", d.Name, len(d.Content))
		}
		if !d.Truncated {
			t.Errorf("%s should be marked truncated", d.Name)
		}
		// Truncation must not cut a line in half.
		if !strings.HasSuffix(d.Content, "\n") {
			t.Errorf("%s does not end on a line boundary", d.Name)
		}
		total += len(d.Content)
	}
	if total > MaxTotalBytes {
		t.Errorf("total %d bytes exceeds the budget %d", total, MaxTotalBytes)
	}
}

// A symlink must not be followed: a repository should not be able to make the
// agent read an arbitrary file by linking to it.
func TestLoadIgnoresSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if docs := Load(root); len(docs) != 0 {
		t.Fatalf("symlinked conventions were read: %+v", docs)
	}
}

func TestLoadSkipsEmptyFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "AGENTS.md", "   \n\n")
	if docs := Load(root); len(docs) != 0 {
		t.Fatalf("expected blank file to be skipped, got %+v", docs)
	}
}

func TestPromptIncludesNamesAndBodies(t *testing.T) {
	out := Prompt([]Doc{
		{Name: "AGENTS.md", Content: "use tabs"},
		{Name: "README.md", Content: "a project\n", Truncated: true},
	})
	for _, want := range []string{"AGENTS.md", "use tabs", "README.md", "a project", "truncated"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
}
