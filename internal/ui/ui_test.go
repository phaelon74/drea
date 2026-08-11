package ui

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSanitizeLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a\tb", "a   b"},            // tab -> next stop of 4
		{"\tx", "    x"},             // leading tab
		{"has\rcr", "hascr"},         // carriage return dropped
		{"esc\x1b[31mred", "escred"}, // ESC sequence stripped
		{"bell\x07", "bell"},         // other control dropped
		{"keeps ünïcode ☃", "keeps ünïcode ☃"},
	}
	for _, c := range cases {
		if got := sanitizeLine(c.in, 4); got != c.want {
			t.Errorf("sanitizeLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewWithIOUsesInjectedStreams(t *testing.T) {
	var out bytes.Buffer
	u := NewWithIO(strings.NewReader("yes\n"), &out)
	if !u.Confirm("continue?") {
		t.Fatal("injected input was not used")
	}
	if !strings.Contains(out.String(), "continue?") {
		t.Fatalf("injected output = %q", out.String())
	}
}

func TestClip(t *testing.T) {
	if got := clip("hello", 10); got != "hello" {
		t.Errorf("no clip: got %q", got)
	}
	if got := clip("hello world", 5); got != "hell…" {
		t.Errorf("clip: got %q", got)
	}
	if got := clip("héllo wörld", 4); len([]rune(got)) != 4 {
		t.Errorf("clip rune count = %d, want 4 (%q)", len([]rune(got)), got)
	}
	if got := clip("anything", 0); got != "" {
		t.Errorf("clip 0: got %q", got)
	}
}

func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\n", 2); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("tail = %v", got)
	}
	if got := lastLines("only", 5); len(got) != 1 || got[0] != "only" {
		t.Errorf("single = %v", got)
	}
}

// TestLiveViewRedrawCount verifies the cursor-up count on redraw equals the
// number of lines drawn in the previous frame — the invariant that was broken
// when wrapped lines caused duplicate output.
func TestLiveViewRedrawCount(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, tty: true, colour: false}
	lv := u.LiveView("writing x", 10)

	lv.Update("l1\nl2\nl3")
	first := buf.String()
	if strings.Contains(first, "\x1b[") && strings.Contains(first, "A\r") {
		t.Fatalf("first frame should not move the cursor up: %q", first)
	}

	buf.Reset()
	lv.Update("l1\nl2\nl3\nl4")
	second := buf.String()
	if !strings.Contains(second, "\x1b[3A\r\x1b[J") {
		t.Fatalf("second frame should move up 3 lines (prev body), got %q", second)
	}
}

// TestLiveViewClipsLongLines ensures a very long line is truncated so it fits
// on one physical row (no wrapping, hence no duplication on redraw).
func TestLiveViewClipsLongLines(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, tty: true, colour: false}
	lv := u.LiveView("writing x", 10)

	long := strings.Repeat("A", 500)
	lv.Update(long)

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, livePrefix) {
			if n := len([]rune(line)); n > 80 {
				t.Errorf("rendered line width %d exceeds 80: %q", n, line)
			}
			if !strings.Contains(line, "…") {
				t.Errorf("long line should be clipped with ellipsis: %q", line)
			}
		}
	}
}

// TestStreamFilter verifies that streamed assistant text is stripped of escape
// sequences and control characters while newlines and tabs survive.
func TestStreamFilter(t *testing.T) {
	var f streamFilter
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"keep\nnewline", "keep\nnewline"},
		{"keep\ttab", "keep\ttab"},
		{"drop\rcr", "dropcr"},
		{"bell\x07here", "bellhere"},
		{"\x1b[31mred\x1b[0m", "red"},   // CSI colour sequences stripped
		{"move\x1b[2Kaway", "moveaway"}, // CSI erase stripped
		{"esc\x1bXchar", "escchar"},     // two-char escape: ESC + one byte
		{"keeps ünïcode ☃", "keeps ünïcode ☃"},
	}
	for _, c := range cases {
		f = streamFilter{}
		if got := f.feed(c.in); got != c.want {
			t.Errorf("feed(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStreamFilterSplitEscape ensures an escape sequence split across chunks is
// still neutralised (state carries between feed calls).
func TestStreamFilterSplitEscape(t *testing.T) {
	var f streamFilter
	var got strings.Builder
	got.WriteString(f.feed("hi\x1b"))   // ESC at end of a chunk
	got.WriteString(f.feed("[1;31m"))   // rest of the CSI sequence
	got.WriteString(f.feed("bold red")) // visible text
	if got.String() != "hibold red" {
		t.Errorf("split escape = %q, want %q", got.String(), "hibold red")
	}
}

// TestAssistantSanitises checks the full Assistant path strips injected escapes
// (the reported bug: a random-character test string rendered like AI text).
func TestAssistantSanitises(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, colour: false}
	u.AssistantHeader()
	buf.Reset()
	u.Assistant("output: \x1b[31m$%^&*[]\x1b[0m done\n")
	if got := buf.String(); got != "output: $%^&*[] done\n" {
		t.Errorf("Assistant wrote %q", got)
	}
}

func TestReadInputSingleLine(t *testing.T) {
	u := &UI{in: bufio.NewReader(strings.NewReader("hello\n"))}
	got, err := u.ReadInput()
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if got != "hello" {
		t.Errorf("single-line ReadInput = %q, want %q", got, "hello")
	}
}

func TestReadInputMultiline(t *testing.T) {
	// Ctrl-D (EOF) after content sends the message; blank lines are content.
	u := &UI{in: bufio.NewReader(strings.NewReader("line one\n\nline two\n"))}
	u.SetMultiline(true)
	got, err := u.ReadInput()
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if got != "line one\n\nline two" {
		t.Errorf("multi-line ReadInput = %q", got)
	}
}

// TestReadInputPastedCode simulates pasting a 20-line block of code in
// multi-line mode. The whole block must come back as a single message, not as
// twenty one-line messages, even when it contains blank lines.
func TestReadInputPastedCode(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&sb, "line %02d of pasted code\n", i)
		if i%5 == 0 {
			sb.WriteString("\n") // blank line inside the block is ordinary content
		}
	}

	u := &UI{in: bufio.NewReader(strings.NewReader(sb.String()))}
	u.SetMultiline(true)
	got, err := u.ReadInput()
	if err != nil {
		t.Fatalf("ReadInput: %v", err)
	}
	if n := strings.Count(got, "\n"); n != 22 {
		t.Errorf("pasted block has %d newlines, want 22 (20 lines + 4 blank, trailing newline trimmed, one message)", n)
	}
	if !strings.HasPrefix(got, "line 01 of pasted code") || !strings.HasSuffix(got, "line 20 of pasted code") {
		t.Errorf("pasted block not preserved as one message: %q", got)
	}
}

func TestStatusBarDoesNotWipeMidLineAssistantText(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, colour: false, tty: true, lineStart: true}
	u.AssistantHeader()
	u.Assistant("Paris") // no trailing newline — the common short-answer case
	u.SetStatus(10, 100)
	u.ShowStatus()
	u.HideStatus()
	got := buf.String()
	if !strings.Contains(got, "Paris") {
		t.Fatalf("status redraw erased mid-line assistant text: %q", got)
	}
	// The answer must appear before any status-bar clear/redraw escapes.
	paris := strings.Index(got, "Paris")
	clear := strings.Index(got, "\r\033[K")
	if clear >= 0 && clear < paris {
		t.Fatalf("status clear happened before assistant text: %q", got)
	}
}

func TestTermWidthFallback(t *testing.T) {
	// In the test harness stdout is not a terminal, so termWidth must fall
	// back to a sane default rather than 0.
	if w := termWidth(); w <= 0 {
		t.Errorf("termWidth = %d, want > 0", w)
	}
}

func TestInfoStripsESC(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, colour: false}
	u.Info("hello\x1b[31mRED\x1b[0m\x07world")
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("Info left control characters: %q", got)
	}
	if !strings.Contains(got, "helloREDworld") {
		t.Errorf("Info dropped visible text: %q", got)
	}
}

func TestStatusBarStripsESC(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, colour: false, tty: true}
	u.SetStatus(10, 100)
	u.SetStatusWorkdir("/tmp/\x1b[31mevil\x1b[0m")
	u.SetStatusMessage("msg\x1b[2K\x07boom")
	u.ShowStatus()
	got := buf.String()
	if strings.Contains(got, "\x1b[31m") || strings.Contains(got, "\x1b[2K") || strings.Contains(got, "\x07") {
		t.Errorf("status bar left ESC/control sequences: %q", got)
	}
	if !strings.Contains(got, "evil") || !strings.Contains(got, "boom") {
		t.Errorf("status bar dropped visible text: %q", got)
	}
}

func TestPrintlnStripsESC(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{out: &buf, colour: false}
	u.Println("a\x1b[31mb\x07c")
	if got := buf.String(); strings.ContainsAny(got, "\x1b\x07") || !strings.Contains(got, "abc") {
		t.Errorf("Println = %q", got)
	}
}

func TestBannerAndConfirmSanitizeUntrustedText(t *testing.T) {
	var buf bytes.Buffer
	u := &UI{
		out:       &buf,
		in:        bufio.NewReader(strings.NewReader("n\n")),
		colour:    false,
		lineStart: true,
	}
	u.Banner("m\x1b[31model", "work\x07dir", false)
	u.Confirm("approve\x1b[2J?")
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("banner/confirm emitted controls: %q", got)
	}
	if !strings.Contains(got, "mmodel") || !strings.Contains(got, "workdir") || !strings.Contains(got, "approve?") {
		t.Fatalf("banner/confirm dropped visible text: %q", got)
	}
}

type overlapWriter struct {
	bytes.Buffer
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return w.Buffer.Write(p)
}

// TestConcurrentOutputNoHang exercises concurrent calls to Info, Warn,
// Assistant, status, StartThinking and StopThinking under the race detector.
func TestConcurrentOutputNoHang(t *testing.T) {
	writer := &overlapWriter{entered: make(chan struct{}), release: make(chan struct{})}
	u := &UI{out: writer, colour: false, tty: true, lineStart: true}
	u.SetStatus(50, 100)

	start := make(chan struct{})
	done := make(chan struct{})
	var workers sync.WaitGroup
	var ready sync.WaitGroup
	workers.Add(4)
	ready.Add(4)
	go func() {
		defer workers.Done()
		<-start
		ready.Done()
		for i := 0; i < 50; i++ {
			u.Info("info line")
			u.Warn("warn line")
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		ready.Done()
		for i := 0; i < 50; i++ {
			u.AssistantHeader()
			u.Assistant("chunk ")
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		ready.Done()
		for i := 0; i < 50; i++ {
			u.SetStatus(i, 100)
			u.SetStatusMessage("working")
			u.ShowStatus()
			u.HideStatus()
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		ready.Done()
		for i := 0; i < 50; i++ {
			u.StartThinking()
			u.StopThinking()
		}
	}()
	go func() {
		workers.Wait()
		close(done)
	}()
	close(start)

	readyDone := make(chan struct{})
	go func() {
		ready.Wait()
		close(readyDone)
	}()
	select {
	case <-readyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("UI workers did not overlap")
	}
	select {
	case <-writer.entered:
		close(writer.release)
	case <-time.After(2 * time.Second):
		t.Fatal("UI writer was not exercised")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent UI operations deadlocked")
	}
}
