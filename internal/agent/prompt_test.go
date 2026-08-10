package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptIncludesProjectConventions(t *testing.T) {
	root := t.TempDir()
	const rule = "Never use third-party dependencies."
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	p := systemPrompt(root, []string{"read_file", "apply_patch"})
	if !strings.Contains(p, rule) {
		t.Errorf("prompt does not carry the project rule:\n%s", p)
	}
	if !strings.Contains(p, "AGENTS.md") {
		t.Error("prompt does not name the source file")
	}
	if !strings.Contains(p, "apply_patch") {
		t.Error("prompt does not list the tools")
	}
}

// A project with no instruction files must produce a prompt with no dangling
// conventions section.
func TestSystemPromptWithoutConventions(t *testing.T) {
	p := systemPrompt(t.TempDir(), []string{"read_file"})
	if strings.Contains(p, "Project instructions") {
		t.Errorf("unexpected conventions section:\n%s", p)
	}
}
