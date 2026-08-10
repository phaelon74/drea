package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// search is a self-contained, regexp-based file content search. It is
// implemented in pure Go (no external grep) so it works identically anywhere
// and adds no runtime dependency.
type search struct{ root string }

func (t *search) Name() string   { return "search" }
func (t *search) Mutating() bool { return false }
func (t *search) Description() string {
	return "Search file contents for a regular expression, returning matching lines as path:line:text. Optionally restrict to a subdirectory and to filenames matching a glob (e.g. '*.go')."
}
func (t *search) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"RE2 regular expression to search for."},
    "path":{"type":"string","description":"Subdirectory to search. Defaults to workspace root."},
    "glob":{"type":"string","description":"Only search files whose base name matches this glob, e.g. '*.go'. Optional."},
    "max_results":{"type":"integer","description":"Maximum matching lines to return. Default 100."}
  },
  "required":["pattern"]
}`)
}
func (t *search) Summary(args json.RawMessage) string {
	var a struct {
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
	}
	_ = decode(args, &a)
	if a.Glob != "" {
		return fmt.Sprintf("%q in %s", a.Pattern, a.Glob)
	}
	return fmt.Sprintf("%q", a.Pattern)
}

// skipDirs are never descended into during a search.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

// errEnough stops the walk once max_results is reached. A sentinel error is
// used instead of filepath.SkipAll to keep compatibility with Go 1.19.
var errEnough = errors.New("enough results")

func (t *search) Run(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		MaxResults int    `json:"max_results"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 100
	}
	base, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}

	var out []string
	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if a.Glob != "" {
			if ok, _ := filepath.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		if len(out) >= a.MaxResults {
			return errEnough
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || isBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d:%s", rel(t.root, path), i+1, strings.TrimSpace(line)))
				if len(out) >= a.MaxResults {
					return errEnough
				}
			}
		}
		return nil
	})
	if walkErr != nil && walkErr != errEnough {
		return "", walkErr
	}
	if len(out) == 0 {
		return "(no matches)", nil
	}
	return strings.Join(out, "\n"), nil
}

// isBinary heuristically detects binary files by looking for NUL bytes in the
// first 8KiB, so the search does not emit garbage.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
