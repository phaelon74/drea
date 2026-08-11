package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dreaagent/drea/internal/patch"
)

// ---- apply_patch ----

type applyPatch struct{ root string }

func (t *applyPatch) Name() string   { return "apply_patch" }
func (t *applyPatch) Mutating() bool      { return true }
func (t *applyPatch) AlwaysConfirm() bool { return false }
func (t *applyPatch) Description() string {
	return "Apply several find/replace edits to one file in a single call. Edits are applied in order and all must succeed, so the file is never left half-edited. If an exact match fails, the text is matched line-by-line ignoring leading/trailing whitespace, so small indentation differences still apply. A match must be unambiguous: include enough surrounding context, or set replace_all for an exactly repeated string. Prefer this over repeated edit_file calls when changing several places in the same file."
}
func (t *applyPatch) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"File path relative to the workspace root."},
    "edits":{
      "type":"array",
      "description":"Edits to apply in order. Each is applied to the result of the previous one.",
      "items":{
        "type":"object",
        "properties":{
          "old_string":{"type":"string","description":"Text to find. Include surrounding lines so it is unique."},
          "new_string":{"type":"string","description":"Replacement text. Use an empty string to delete."},
          "replace_all":{"type":"boolean","description":"Replace every occurrence. Default false."}
        },
        "required":["old_string","new_string"]
      }
    }
  },
  "required":["path","edits"]
}`)
}
func (t *applyPatch) Summary(args json.RawMessage) string {
	a, err := decodePatchArgs(args)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s (%d edit(s))", a.Path, len(a.Edits))
}
func (t *applyPatch) Run(_ context.Context, args json.RawMessage) (string, error) {
	a, err := decodePatchArgs(args)
	if err != nil {
		return "", err
	}
	p, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}
	res, err := applyEdits(t.root, p, a.Edits)
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel(t.root, p), err)
	}
	msg := fmt.Sprintf("applied %d edit(s), %d replacement(s) in %s", len(a.Edits), res.Replacements, rel(t.root, p))
	if res.Fuzzy > 0 {
		msg += fmt.Sprintf(" (%d matched ignoring whitespace)", res.Fuzzy)
	}
	return msg, nil
}

// patchArgs is the decoded form of an apply_patch call.
type patchArgs struct {
	Path  string       `json:"path"`
	Edits []patch.Edit `json:"-"`
}

// rawEdit mirrors patch.Edit with the tool's snake_case JSON names.
type rawEdit struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// decodePatchArgs parses an apply_patch argument object. It is shared with the
// agent's diff preview so the preview and the write use identical inputs.
func decodePatchArgs(args json.RawMessage) (patchArgs, error) {
	var a struct {
		Path  string    `json:"path"`
		Edits []rawEdit `json:"edits"`
	}
	if err := decode(args, &a); err != nil {
		return patchArgs{}, err
	}
	if len(a.Edits) == 0 {
		return patchArgs{}, errors.New("edits is required and must not be empty")
	}
	out := patchArgs{Path: a.Path, Edits: make([]patch.Edit, len(a.Edits))}
	for i, e := range a.Edits {
		out.Edits[i] = patch.Edit{Old: e.OldString, New: e.NewString, ReplaceAll: e.ReplaceAll}
	}
	return out, nil
}

// ParseEdits extracts the target path and edits from an edit_file or
// apply_patch call. It lets callers (the agent's diff preview) reproduce a
// pending change with exactly the same inputs and algorithm the tool will use,
// so the preview can never diverge from what is written. ok is false for other
// tools or unparseable arguments.
func ParseEdits(name string, args json.RawMessage) (path string, edits []patch.Edit, ok bool) {
	switch name {
	case "apply_patch":
		a, err := decodePatchArgs(args)
		if err != nil || a.Path == "" {
			return "", nil, false
		}
		return a.Path, a.Edits, true
	case "edit_file":
		var a struct {
			Path string `json:"path"`
			rawEdit
		}
		if decode(args, &a) != nil || a.Path == "" {
			return "", nil, false
		}
		return a.Path, []patch.Edit{{Old: a.OldString, New: a.NewString, ReplaceAll: a.ReplaceAll}}, true
	}
	return "", nil, false
}

// applyEdits reads path, applies edits in memory and only writes when every
// edit succeeded. The secure write rejects a replaced destination. A hostile
// process can still mutate the same inode through a hard link while it is read;
// repository confinement does not imply isolation from local processes.
// When an edit fails, the current file content is included in the error so the
// model can retry with an old_string that matches the actual file. display
// paths in errors are relative to root.
func applyEdits(root, path string, edits []patch.Edit) (patch.Result, error) {
	f, mode, identity, err := openSecureRegular(root, path)
	if err != nil {
		return patch.Result{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	f.Close()
	if err != nil {
		return patch.Result{}, err
	}
	if len(data) > maxFileBytes {
		return patch.Result{}, fmt.Errorf("%s exceeds the %d byte read limit", rel(root, path), maxFileBytes)
	}
	res, err := patch.Apply(string(data), edits)
	if err != nil {
		return patch.Result{}, fmt.Errorf("%w\n\nCurrent file content (%s):\n%s", err, rel(root, path), trimForError(string(data)))
	}
	if len(res.Text) > maxFileBytes {
		return patch.Result{}, fmt.Errorf("edited content exceeds the %d byte write limit", maxFileBytes)
	}
	if err := secureWriteAtomic(root, path, []byte(res.Text), mode, &identity); err != nil {
		return patch.Result{}, err
	}
	return res, nil
}

// trimForError limits content included in an error message to avoid flooding
// the model context, while still giving it enough to retry the edit.
func trimForError(content string) string {
	const maxLines = 120
	lines := strings.Split(content, "\n")
	if len(lines) <= maxLines {
		return content
	}
	return strings.Join(lines[:maxLines], "\n") + "\n... (truncated)"
}
