package patch

import "testing"

func TestApplyExactSingleAndMultiHunk(t *testing.T) {
	src := "package main\n\nfunc a() int { return 1 }\n\nfunc b() int { return 2 }\n"
	res, err := Apply(src, []Edit{
		{Old: "return 1", New: "return 10"},
		{Old: "return 2", New: "return 20"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "package main\n\nfunc a() int { return 10 }\n\nfunc b() int { return 20 }\n"
	if res.Text != want {
		t.Errorf("got:\n%q\nwant:\n%q", res.Text, want)
	}
	if res.Replacements != 2 || res.Fuzzy != 0 {
		t.Errorf("got %d replacements / %d fuzzy, want 2/0", res.Replacements, res.Fuzzy)
	}
}

func TestApplyIsAtomicOnFailure(t *testing.T) {
	src := "alpha\nbeta\n"
	// The first edit is fine, the second cannot match; nothing must be applied.
	_, err := Apply(src, []Edit{
		{Old: "alpha", New: "ALPHA"},
		{Old: "nonexistent", New: "x"},
	})
	if err == nil {
		t.Fatal("expected an error when one edit fails")
	}
	if got := err.Error(); got == "" {
		t.Error("expected a descriptive error")
	}
}

func TestFuzzyMatchesTrailingWhitespace(t *testing.T) {
	// File has trailing spaces the model did not reproduce.
	src := "func f() {   \n\treturn nil   \n}\n"
	res, err := Apply(src, []Edit{
		{Old: "func f() {\n\treturn nil\n}", New: "func f() error {\n\treturn nil\n}"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Fuzzy != 1 {
		t.Errorf("expected 1 fuzzy match, got %d", res.Fuzzy)
	}
	want := "func f() error {\n\treturn nil\n}\n"
	if res.Text != want {
		t.Errorf("got %q, want %q", res.Text, want)
	}
}

func TestFuzzyMatchesDifferentIndentation(t *testing.T) {
	// File uses a tab; the model emitted four spaces.
	src := "if x {\n\tdoThing()\n}\n"
	res, err := Apply(src, []Edit{
		{Old: "if x {\n    doThing()\n}", New: "if x {\n\tdoOther()\n}"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Fuzzy != 1 {
		t.Errorf("expected fuzzy match, got %d", res.Fuzzy)
	}
	if res.Text != "if x {\n\tdoOther()\n}\n" {
		t.Errorf("unexpected result %q", res.Text)
	}
}

func TestAmbiguousExactRequiresReplaceAll(t *testing.T) {
	src := "x = 1\nx = 1\n"
	if _, err := Apply(src, []Edit{{Old: "x = 1", New: "x = 2"}}); err == nil {
		t.Fatal("expected ambiguity error")
	}
	res, err := Apply(src, []Edit{{Old: "x = 1", New: "x = 2", ReplaceAll: true}})
	if err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if res.Text != "x = 2\nx = 2\n" || res.Replacements != 2 {
		t.Errorf("got %q (%d replacements)", res.Text, res.Replacements)
	}
}

// An ambiguous *fuzzy* match must fail rather than pick a region, even with
// replace_all: guessing the wrong region is worse than failing the edit. Both
// candidate regions here have trailing whitespace, so no exact match exists.
func TestAmbiguousFuzzyIsRejected(t *testing.T) {
	src := "a  \nb\nzzz\na\t\nb\n"
	_, err := Apply(src, []Edit{{Old: "a\nb", New: "c\nd", ReplaceAll: true}})
	if err == nil {
		t.Fatal("expected ambiguous fuzzy match to be rejected")
	}
}

func TestNotFoundAndEmptyEdits(t *testing.T) {
	if _, err := Apply("abc\n", []Edit{{Old: "zzz", New: "y"}}); err == nil {
		t.Error("expected not-found error")
	}
	if _, err := Apply("abc\n", []Edit{{Old: "", New: "y"}}); err == nil {
		t.Error("expected empty old_string error")
	}
	if _, err := Apply("abc\n", nil); err == nil {
		t.Error("expected error for no edits")
	}
}

// Sequential edits must see the result of earlier ones.
func TestEditsApplySequentially(t *testing.T) {
	res, err := Apply("a\n", []Edit{
		{Old: "a", New: "b"},
		{Old: "b", New: "c"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Text != "c\n" {
		t.Errorf("got %q, want %q", res.Text, "c\n")
	}
}

// A pattern ending in a newline must consume the matched line's newline so the
// surrounding lines are not joined together.
func TestDeleteWholeLine(t *testing.T) {
	src := "one\ntwo\nthree\n"
	res, err := Apply(src, []Edit{{Old: "two\n", New: ""}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Text != "one\nthree\n" {
		t.Errorf("got %q, want %q", res.Text, "one\nthree\n")
	}
}

// The final line of a file with no trailing newline must still be matchable
// (its byte range has no newline to consume).
func TestFileWithoutTrailingNewline(t *testing.T) {
	res, err := Apply("alpha\nlast line", []Edit{{Old: "last line   ", New: "final"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Text != "alpha\nfinal" {
		t.Errorf("got %q, want %q", res.Text, "alpha\nfinal")
	}
}

// Internal whitespace is deliberately NOT normalized: collapsing it could match
// a semantically different line (e.g. inside a string literal).
func TestInternalWhitespaceIsNotFuzzed(t *testing.T) {
	if _, err := Apply("last line", []Edit{{Old: "last  line", New: "final"}}); err == nil {
		t.Error("expected internal whitespace difference not to match")
	}
}
