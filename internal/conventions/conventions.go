// Package conventions discovers a project's own instructions — files like
// AGENTS.md, CONVENTIONS.md or the README — so they can be given to the model
// as context.
//
// This is how the agent learns a project's rules (build commands, style, things
// not to touch) without any of them being compiled into the binary: the rules
// live in the repository being worked on, so the same mechanism works for every
// project, including drea's own.
package conventions

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Budgets for how much project instruction text is injected into the system
// prompt. Conventions compete with actual work for the context window, so the
// amount is bounded rather than "whatever the file happens to be".
const (
	// MaxTotalBytes caps the combined size of all discovered documents.
	MaxTotalBytes = 16 << 10
	// MaxFileBytes caps a single document; a long README contributes only its
	// beginning, which is where a project's overview normally lives.
	MaxFileBytes = 8 << 10
	// minUsefulBytes is the smallest leftover budget worth spending on a
	// document. Without it the last file could be admitted with a handful of
	// bytes, which costs a prompt section and conveys nothing.
	minUsefulBytes = 512
)

// candidates are the recognised instruction files, most specific first. Files
// written for coding agents outrank general documentation, so when the budget
// is tight the most directly relevant instructions survive.
var candidates = []string{
	"AGENTS.md",
	"CLAUDE.md",
	"CONVENTIONS.md",
	"CONTRIBUTING.md",
	"README.md",
	"README",
}

// Doc is one discovered instruction file.
type Doc struct {
	// Name is the file name as found on disk.
	Name string
	// Content is the file's text, possibly truncated.
	Content string
	// Truncated reports whether Content is only the start of the file.
	Truncated bool
}

// Load reads the recognised instruction files from the workspace root, in
// priority order, until the byte budget is exhausted. Only regular files
// directly in the root are considered: no traversal, and no symlinks, so a
// repository cannot use a link to make the agent read an arbitrary file.
// Unreadable files are skipped — missing conventions are normal, not an error.
func Load(root string) []Doc {
	found := index(root)
	var docs []Doc
	budget := MaxTotalBytes
	for _, want := range candidates {
		name, ok := found[strings.ToLower(want)]
		if !ok || budget < minUsefulBytes {
			continue
		}
		path := filepath.Join(root, name)
		// Lstat, not Stat: a symlink must not be followed even if it points
		// somewhere legitimate.
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		limit := MaxFileBytes
		if budget < limit {
			limit = budget
		}
		text, truncated := clip(string(data), limit)
		if strings.TrimSpace(text) == "" {
			continue
		}
		docs = append(docs, Doc{Name: name, Content: text, Truncated: truncated})
		budget -= len(text)
	}
	return docs
}

// index maps the lower-cased names of the root's entries to their real names,
// so files are matched case-insensitively (readme.md, Readme.md) with a single
// directory read instead of guessing spellings.
func index(root string) map[string]string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(entries))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// Sort so that if two names differ only in case the choice is stable.
	sort.Strings(names)
	for _, n := range names {
		key := strings.ToLower(n)
		if _, seen := out[key]; !seen {
			out[key] = n
		}
	}
	return out
}

// clip truncates s to at most limit bytes, cutting at a line boundary so the
// model never sees half a line. A single line longer than the limit is the one
// case where it must cut mid-line.
func clip(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		return cut[:i+1], true
	}
	return cut, true
}

// Prompt renders the documents as a section for the system prompt. It returns
// an empty string when nothing was found, so the prompt is unchanged for
// projects that carry no instructions.
func Prompt(docs []Doc) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nProject instructions (from the workspace; follow them over your own defaults):\n")
	for _, d := range docs {
		b.WriteString("\n--- " + d.Name + " ---\n")
		b.WriteString(d.Content)
		if !strings.HasSuffix(d.Content, "\n") {
			b.WriteString("\n")
		}
		if d.Truncated {
			b.WriteString("--- (truncated; read " + d.Name + " for the rest) ---\n")
		}
	}
	return b.String()
}
