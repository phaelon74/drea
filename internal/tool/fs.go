package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dreaagent/drea/internal/patch"
)

// ---- read_file ----

type readFile struct{ root string }

func (t *readFile) Name() string   { return "read_file" }
func (t *readFile) Mutating() bool { return false }
func (t *readFile) Description() string {
	return "Read a UTF-8 text file within the workspace. Optionally start at a 1-based line offset and limit the number of lines returned. Output is prefixed with line numbers."
}
func (t *readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"File path relative to the workspace root."},
    "offset":{"type":"integer","description":"1-based line to start from. Optional."},
    "limit":{"type":"integer","description":"Maximum number of lines to return. Optional."}
  },
  "required":["path"]
}`)
}
func (t *readFile) Summary(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(args, &a)
	return a.Path
}
func (t *readFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if b.Len() == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

// ---- write_file ----

type writeFile struct{ root string }

func (t *writeFile) Name() string   { return "write_file" }
func (t *writeFile) Mutating() bool { return true }
func (t *writeFile) Description() string {
	return "Create a new file or overwrite an existing one with the given content. Parent directories are created automatically."
}
func (t *writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"File path relative to the workspace root."},
    "content":{"type":"string","description":"Full file content to write."}
  },
  "required":["path","content"]
}`)
}
func (t *writeFile) Summary(args json.RawMessage) string {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = decode(args, &a)
	return fmt.Sprintf("%s (%d bytes)", a.Path, len(a.Content))
}
func (t *writeFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), rel(t.root, p)), nil
}

// ---- edit_file ----

type editFile struct{ root string }

func (t *editFile) Name() string   { return "edit_file" }
func (t *editFile) Mutating() bool { return true }
func (t *editFile) Description() string {
	return "Replace a string in a file. By default old_string must match exactly once; set replace_all to replace every occurrence. Use enough surrounding context to make old_string unique. If no exact match exists, the text is matched line-by-line ignoring leading/trailing whitespace. To change several places in one file, prefer apply_patch."
}
func (t *editFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"File path relative to the workspace root."},
    "old_string":{"type":"string","description":"Exact text to find."},
    "new_string":{"type":"string","description":"Replacement text."},
    "replace_all":{"type":"boolean","description":"Replace every occurrence. Default false."}
  },
  "required":["path","old_string","new_string"]
}`)
}
func (t *editFile) Summary(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(args, &a)
	return a.Path
}
func (t *editFile) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}
	// A single-hunk apply_patch: one matching engine for both tools.
	res, err := applyEdits(p, []patch.Edit{{Old: a.OldString, New: a.NewString, ReplaceAll: a.ReplaceAll}})
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel(t.root, p), err)
	}
	msg := fmt.Sprintf("replaced %d occurrence(s) in %s", res.Replacements, rel(t.root, p))
	if res.Fuzzy > 0 {
		msg += " (matched ignoring whitespace)"
	}
	return msg, nil
}

// ---- list_dir ----

type listDir struct{ root string }

func (t *listDir) Name() string   { return "list_dir" }
func (t *listDir) Mutating() bool { return false }
func (t *listDir) Description() string {
	return "List the immediate entries of a directory within the workspace. Directories are suffixed with '/'."
}
func (t *listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Directory path relative to the workspace root. Defaults to '.'"}
  }
}`)
}
func (t *listDir) Summary(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(args, &a)
	if a.Path == "" {
		return "."
	}
	return a.Path
}
func (t *listDir) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Path == "" {
		a.Path = "."
	}
	p, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(names, "\n"), nil
}
