// Package diff produces a compact, unified-style line diff between two texts,
// using only the standard library. It is used to preview file writes and edits
// before they are applied and to summarise what changed afterwards.
package diff

import (
	"fmt"
	"strings"
)

// Line is a single diff line tagged with its operation.
type Line struct {
	// Op is ' ' (context), '-' (removed) or '+' (added).
	Op   byte
	Text string
}

// Lines computes a line-level diff from old to new via a longest-common-
// subsequence backtrace. The result interleaves removals, additions and
// context in original order.
func Lines(oldText, newText string) []Line {
	a := splitLines(oldText)
	b := splitLines(newText)

	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []Line
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, Line{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, Line{'-', a[i]})
			i++
		default:
			out = append(out, Line{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, Line{'-', a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, Line{'+', b[j]})
	}
	return out
}

// Stat reports how many lines were added and removed between old and new.
func Stat(oldText, newText string) (added, removed int) {
	for _, l := range Lines(oldText, newText) {
		switch l.Op {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// Unified renders a diff as text, collapsing long unchanged runs so only
// context lines near a change are shown. context is the number of unchanged
// lines to keep around each change.
func Unified(oldText, newText string, context int) string {
	lines := Lines(oldText, newText)
	keep := visible(lines, context)

	var b strings.Builder
	skipping := false
	for i, l := range lines {
		if !keep[i] {
			if !skipping {
				b.WriteString("  …\n")
				skipping = true
			}
			continue
		}
		skipping = false
		fmt.Fprintf(&b, "%c %s\n", l.Op, l.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// visible marks which lines to render: every changed line plus up to context
// unchanged lines on each side of a change.
func visible(lines []Line, context int) []bool {
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.Op == ' ' {
			continue
		}
		lo := i - context
		if lo < 0 {
			lo = 0
		}
		hi := i + context
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}
	return keep
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
