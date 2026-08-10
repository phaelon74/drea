package agent

import "testing"

func TestStreamingFieldPartial(t *testing.T) {
	// Arguments arrive fragment by fragment; content should decode as far as
	// the bytes seen so far allow.
	buf := `{"path":"a.txt","content":"line1\nline2`
	got, ok := streamingField(buf, "content")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "line1\nline2" {
		t.Fatalf("content = %q, want %q", got, "line1\nline2")
	}
}

func TestStreamingFieldComplete(t *testing.T) {
	buf := `{"path":"a.txt","content":"done\n"}`
	got, ok := streamingField(buf, "content")
	if !ok || got != "done\n" {
		t.Fatalf("content = %q ok=%v, want %q", got, ok, "done\n")
	}
}

func TestStreamingFieldAbsent(t *testing.T) {
	if _, ok := streamingField(`{"path":"a.txt"`, "content"); ok {
		t.Fatal("ok = true, want false (field not present yet)")
	}
}

func TestStreamingFieldIncompleteEscape(t *testing.T) {
	// A trailing lone backslash must not be mis-decoded or panic.
	got, ok := streamingField(`{"content":"ab\`, "content")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "ab" {
		t.Fatalf("content = %q, want %q", got, "ab")
	}
}

func TestStreamingFieldUnicode(t *testing.T) {
	got, ok := streamingField(`{"content":"caf\u00e9"}`, "content")
	if !ok || got != "café" {
		t.Fatalf("content = %q ok=%v, want café", got, ok)
	}
}
