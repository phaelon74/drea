// Package tool defines the agent's capabilities: a small, auditable set of
// file, search and shell tools. Every tool is confined to a workspace root and
// can declare whether it mutates state (and therefore needs user approval).
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dreaagent/drea/internal/llm"
)

// Tool is a single capability exposed to the model.
type Tool interface {
	// Name is the function name advertised to the model.
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// Schema is the JSON-schema object describing the tool's parameters.
	Schema() json.RawMessage
	// Mutating reports whether the tool changes state (writes files, runs
	// commands) and therefore requires confirmation unless auto-approve is on.
	Mutating() bool
	// AlwaysConfirm reports whether this tool must prompt for approval even
	// when auto-approve is enabled (e.g. destructive rollback).
	AlwaysConfirm() bool
	// Summary renders a short, human-readable description of a specific call,
	// shown in the UI and in approval prompts.
	Summary(args json.RawMessage) string
	// Run executes the tool and returns text to feed back to the model.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry holds the active tool set, all confined to Root.
type Registry struct {
	Root  string
	tools map[string]Tool
	order []string
}

// NewRegistry builds the default tool set rooted at workdir.
func NewRegistry(workdir string) *Registry {
	r := &Registry{Root: workdir, tools: map[string]Tool{}}
	r.add(&readFile{root: workdir})
	r.add(&writeFile{root: workdir})
	r.add(&editFile{root: workdir})
	r.add(&applyPatch{root: workdir})
	r.add(&listDir{root: workdir})
	r.add(&search{root: workdir})
	r.add(&runCommand{root: workdir})
	r.add(&gitRead{root: workdir})
	r.add(&gitInit{root: workdir})
	r.add(&gitCommit{root: workdir})
	r.add(&gitRollback{root: workdir})
	r.add(&reply{})
	return r
}

func (r *Registry) add(t Tool) {
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Specs returns the tool definitions to advertise to the model.
func (r *Registry) Specs() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, llm.Tool{
			Type: "function",
			Function: llm.FunctionSpec{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return out
}

// ReadFileLimited opens a workspace-relative regular file without following
// its final component and reads at most max+1 bytes. Missing files are reported
// with exists=false so callers can distinguish a new-file preview.
func (r *Registry) ReadFileLimited(path string, max int) (data []byte, exists bool, err error) {
	if max < 0 {
		return nil, false, errors.New("negative read limit")
	}
	resolved, err := resolve(r.Root, path)
	if err != nil {
		return nil, false, err
	}
	f, _, _, err := openSecureRegular(r.Root, resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// resolve joins a user-supplied path against the workspace root and verifies
// the result stays inside it, defeating path traversal (e.g. "../../etc") and
// symlinks that point outside the workspace. Absolute paths are rejected so
// every tool argument is unambiguously relative to the workspace.
func resolve(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative to the workspace root", p)
	}
	clean := filepath.Clean(filepath.Join(root, p))
	rootClean := filepath.Clean(root)
	if !within(rootClean, clean) {
		return "", fmt.Errorf("path %q escapes the workspace root %q", p, root)
	}
	// A lexical check is not enough: a symlink inside the workspace could point
	// outside it. Resolve symlinks (in the deepest existing ancestor, so paths
	// being newly created still work) and re-check containment against the
	// real, symlink-resolved workspace root.
	realRoot := evalSymlinks(rootClean)
	if real := evalSymlinks(clean); !within(realRoot, real) {
		return "", fmt.Errorf("path %q escapes the workspace root %q via a symlink", p, root)
	}
	return clean, nil
}

// within reports whether p is root itself or lies beneath it.
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// evalSymlinks resolves symlinks in p. If p does not exist, it resolves the
// deepest existing ancestor and re-appends the remaining components, so paths
// that are about to be created are still checked against a real root.
func evalSymlinks(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p // reached the filesystem root
	}
	return filepath.Join(evalSymlinks(parent), filepath.Base(p))
}

// rel renders an absolute path relative to root for tidy display.
func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// decode unmarshals tool arguments into v, returning a friendly error.
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
