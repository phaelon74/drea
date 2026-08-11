package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (t *search) Name() string        { return "search" }
func (t *search) Mutating() bool      { return false }
func (t *search) AlwaysConfirm() bool { return false }
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

const (
	maxSearchResults     = 1000
	maxSearchFileBytes   = 1 << 20  // 1 MiB per file
	maxSearchScanBytes   = 32 << 20 // 32 MiB total scanned
	maxSearchOutputBytes = 1 << 20  // 1 MiB of result text
	maxSearchLineBytes   = 256 << 10
)

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
	if a.MaxResults > maxSearchResults {
		a.MaxResults = maxSearchResults
	}
	base, err := resolve(t.root, a.Path)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	var results, scanned int
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
		// Lstat: never follow symlinks or search non-regular files.
		fi, lerr := os.Lstat(path)
		if lerr != nil || !fi.Mode().IsRegular() {
			return nil
		}
		if a.Glob != "" {
			if ok, _ := filepath.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		if results >= a.MaxResults || scanned >= maxSearchScanBytes {
			return errEnough
		}
		relPath, rerr := filepath.Rel(t.root, path)
		if rerr != nil {
			return nil
		}
		// Re-resolve before reading so a symlink race cannot pull content
		// from outside the workspace.
		resolved, rerr := resolve(t.root, relPath)
		if rerr != nil {
			return nil
		}
		f, _, _, oerr := openSecureRegular(t.root, resolved)
		if oerr != nil {
			return nil
		}
		readLimit := maxSearchFileBytes + 1
		if remaining := maxSearchScanBytes - scanned + 1; remaining < readLimit {
			readLimit = remaining
		}
		reader := bufio.NewReaderSize(io.LimitReader(f, int64(readLimit)), maxSearchLineBytes+1)
		prefix, _ := reader.Peek(8192)
		if isBinary(prefix) {
			scanned += len(prefix)
			f.Close()
			if scanned > maxSearchScanBytes {
				return errEnough
			}
			return nil
		}
		stop, rerr := scanSearchFile(reader, rel(t.root, resolved), re, a.MaxResults, &results, &scanned, &out)
		closeErr := f.Close()
		if rerr != nil {
			return rerr
		}
		if closeErr != nil {
			return nil
		}
		if stop {
			return errEnough
		}
		return nil
	})
	if walkErr != nil && walkErr != errEnough {
		return "", walkErr
	}
	if results == 0 {
		return "(no matches)", nil
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func scanSearchFile(r *bufio.Reader, path string, re *regexp.Regexp, maxResults int, results, scanned *int, out *strings.Builder) (bool, error) {
	fileBytes := 0
	lineNo := 0
	for {
		line, err := r.ReadSlice('\n')
		fileBytes += len(line)
		*scanned += len(line)
		if fileBytes > maxSearchFileBytes || *scanned > maxSearchScanBytes {
			return true, nil
		}
		if err == bufio.ErrBufferFull {
			return false, fmt.Errorf("%s contains a line exceeding the %d byte limit", path, maxSearchLineBytes)
		}
		lineNo++
		text := line
		if len(text) > 0 && text[len(text)-1] == '\n' {
			text = text[:len(text)-1]
		}
		if len(text) > maxSearchLineBytes {
			return false, fmt.Errorf("%s contains a line exceeding the %d byte limit", path, maxSearchLineBytes)
		}
		if re.Match(text) {
			entry := fmt.Sprintf("%s:%d:%s\n", path, lineNo, strings.TrimSpace(string(text)))
			if out.Len()+len(entry) > maxSearchOutputBytes {
				return true, nil
			}
			out.WriteString(entry)
			(*results)++
			if *results >= maxResults {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
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
