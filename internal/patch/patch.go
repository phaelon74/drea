// Package patch applies multi-hunk textual edits to a file's content with a
// whitespace-tolerant fallback.
//
// Exact-string replacement is brittle: a model that reproduces a snippet with
// slightly different indentation or trailing whitespace fails the edit and
// wastes a turn. This package first tries an exact substring match, then falls
// back to line-based matching that ignores whitespace differences.
//
// Safety is preserved by never guessing: a fuzzy match is only accepted when it
// is unique. An ambiguous or missing match is an error, and because edits are
// applied to an in-memory copy, a failure part-way through leaves the original
// content untouched (all-or-nothing).
package patch

import (
	"errors"
	"fmt"
	"strings"
)

// Edit is a single find/replace hunk.
type Edit struct {
	// Old is the text to find. It may be a fragment of a line or span many
	// lines. It must not be empty.
	Old string
	// New is the replacement text.
	New string
	// ReplaceAll replaces every exact occurrence instead of requiring a
	// unique one. It only applies to exact matches; a fuzzy match must
	// always be unique.
	ReplaceAll bool
}

// Result describes the outcome of applying a set of edits.
type Result struct {
	// Text is the updated content.
	Text string
	// Replacements is the total number of occurrences replaced.
	Replacements int
	// Fuzzy is how many edits matched only after ignoring whitespace.
	Fuzzy int
}

// strategy is a whitespace-tolerant fallback used only after an exact
// substring match has failed.
type strategy struct {
	// name describes what the strategy ignores, for error messages.
	name string
	// normalize canonicalises a single line before comparison.
	normalize func(string) string
}

// strategies are tried in order of decreasing strictness, so the least
// forgiving match that works is the one taken.
var strategies = []strategy{
	// Ignore trailing whitespace differences.
	{"whitespace", func(s string) string { return strings.TrimRight(s, " \t\r") }},
	// Ignore leading indentation as well, for re-indented code.
	{"indentation", strings.TrimSpace},
}

// Apply runs each edit against content in order and returns the new text. All
// edits must succeed; otherwise an error is returned and content is unchanged.
func Apply(content string, edits []Edit) (Result, error) {
	if len(edits) == 0 {
		return Result{}, errors.New("no edits given")
	}
	res := Result{Text: content}
	for i, e := range edits {
		if e.Old == "" {
			return Result{}, fmt.Errorf("edit %d: old_string is empty", i+1)
		}
		text, n, fuzzy, err := applyOne(res.Text, e)
		if err != nil {
			return Result{}, fmt.Errorf("edit %d: %w", i+1, err)
		}
		res.Text = text
		res.Replacements += n
		if fuzzy {
			res.Fuzzy++
		}
	}
	return res, nil
}

// applyOne applies a single edit, trying an exact match before the
// whitespace-tolerant strategies.
func applyOne(content string, e Edit) (out string, count int, fuzzy bool, err error) {
	// Exact substring match. This also covers fragments within a line, which
	// the line-based fallbacks cannot express.
	if n := strings.Count(content, e.Old); n > 0 {
		if n > 1 && !e.ReplaceAll {
			return "", 0, false, fmt.Errorf("old_string occurs %d times; add more context or set replace_all", n)
		}
		if e.ReplaceAll {
			return strings.ReplaceAll(content, e.Old, e.New), n, false, nil
		}
		return strings.Replace(content, e.Old, e.New, 1), 1, false, nil
	}

	// Whitespace-tolerant fallbacks. A fuzzy match must be unique: replacing
	// the wrong region is far worse than failing the edit.
	for _, s := range strategies {
		start, end, n := locateLines(content, e.Old, s.normalize)
		if n == 0 {
			continue
		}
		if n > 1 {
			return "", 0, false, fmt.Errorf("old_string matches %d regions when ignoring %s; add more context", n, s.name)
		}
		return content[:start] + e.New + content[end:], 1, true, nil
	}
	return "", 0, false, errors.New("old_string not found")
}

// locateLines finds line-aligned regions of content whose lines equal the lines
// of pattern after normalization. It returns the byte range of the single match
// and the number of matches found (callers reject anything but 1).
func locateLines(content, pattern string, normalize func(string) string) (start, end, count int) {
	// A pattern ending in a newline should consume the matched region's
	// trailing newline too, so surrounding lines are not joined together.
	trailingNL := strings.HasSuffix(pattern, "\n")
	if trailingNL {
		pattern = pattern[:len(pattern)-1]
	}
	pat := strings.Split(pattern, "\n")
	for i := range pat {
		pat[i] = normalize(pat[i])
	}

	lines, offsets := splitLines(content)
	if len(pat) > len(lines) {
		return 0, 0, 0
	}
	for i := 0; i+len(pat) <= len(lines); i++ {
		ok := true
		for j := range pat {
			if normalize(lines[i+j]) != pat[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		count++
		if count > 1 {
			return 0, 0, count
		}
		last := i + len(pat) - 1
		start = offsets[i]
		end = offsets[last] + len(lines[last])
		if trailingNL && end < len(content) && content[end] == '\n' {
			end++
		}
	}
	return start, end, count
}

// splitLines splits content into lines (without their terminating newline) and
// the byte offset at which each line starts.
func splitLines(content string) (lines []string, offsets []int) {
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i])
			offsets = append(offsets, start)
			start = i + 1
		}
	}
	// Trailing segment after the last newline (or the whole string when there
	// is none). A trailing empty segment is not a line.
	if start < len(content) {
		lines = append(lines, content[start:])
		offsets = append(offsets, start)
	}
	return lines, offsets
}
