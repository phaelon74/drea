package diff

import "testing"

func TestStat(t *testing.T) {
	added, removed := Stat("a\nb\nc\n", "a\nB\nc\nd\n")
	if added != 2 || removed != 1 {
		t.Fatalf("stat = +%d/-%d, want +2/-1", added, removed)
	}
}

func TestStatCreation(t *testing.T) {
	added, removed := Stat("", "x\ny\n")
	if added != 2 || removed != 0 {
		t.Fatalf("stat = +%d/-%d, want +2/-0", added, removed)
	}
}

func TestLinesInterleave(t *testing.T) {
	lines := Lines("a\nb\n", "a\nc\n")
	want := []Line{{' ', "a"}, {'-', "b"}, {'+', "c"}}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
}

func TestUnifiedCollapsesContext(t *testing.T) {
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	nw := "1\n2\n3\n4\n5\n6\n7\n8\nCHANGED\n"
	out := Unified(old, nw, 1)
	if want := "  …"; !contains(out, want) {
		t.Fatalf("expected collapsed context marker in:\n%s", out)
	}
	if !contains(out, "+ CHANGED") || !contains(out, "- 9") {
		t.Fatalf("expected the change to be shown in:\n%s", out)
	}
	if contains(out, "  1") {
		t.Fatalf("distant context line 1 should have been collapsed:\n%s", out)
	}
}

func TestSplitLinesEmpty(t *testing.T) {
	if got := splitLines(""); got != nil {
		t.Fatalf("splitLines(\"\") = %v, want nil", got)
	}
	if got := splitLines("\n"); len(got) != 1 || got[0] != "" {
		t.Fatalf("splitLines(newline) = %v, want [\"\"]", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
