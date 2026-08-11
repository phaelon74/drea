// Package process runs bounded child processes with a minimal environment.
package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	envMu   sync.RWMutex
	passEnv []string
)

// Result describes a completed process.
type Result struct {
	Output    string
	Err       error
	Started   bool
	TimedOut  bool
	Truncated bool
}

// SetPassEnv configures additional environment names inherited by children.
// Provider API keys used by drea itself are never inherited.
func SetPassEnv(names []string) {
	envMu.Lock()
	passEnv = append([]string(nil), names...)
	envMu.Unlock()
}

// PassEnv returns the configured environment allow-list.
func PassEnv() []string {
	envMu.RLock()
	defer envMu.RUnlock()
	return append([]string(nil), passEnv...)
}

// Env builds the minimal environment used for child processes.
func Env() []string {
	envMu.RLock()
	extra := append([]string(nil), passEnv...)
	envMu.RUnlock()

	allowed := make(map[string]struct{}, len(extra)+16)
	for _, name := range []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL",
		"TMPDIR", "TMP", "TEMP", "TEMPDIR",
		"LANG", "LANGUAGE", "LC_ALL",
		"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
	} {
		allowed[name] = struct{}{}
	}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name != "" && !alwaysBlocked(name) {
			allowed[strings.ToUpper(name)] = struct{}{}
		}
	}

	env := os.Environ()
	out := make([]string, 0, len(allowed))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || alwaysBlocked(name) {
			continue
		}
		upper := strings.ToUpper(name)
		_, keep := allowed[upper]
		if !keep && !strings.HasPrefix(upper, "LC_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Run executes argv[0] with a timeout and bounded combined output.
func Run(ctx context.Context, dir string, argv []string, timeout time.Duration, outputLimit int) Result {
	if len(argv) == 0 {
		return Result{Err: exec.ErrNotFound}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = Env()
	configureCommand(cmd)
	sink := &capWriter{limit: outputLimit}
	cmd.Stdout, cmd.Stderr = sink, sink
	if err := cmd.Start(); err != nil {
		return Result{Err: err}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-cctx.Done():
		killTree(cmd)
		waitAfterKill(done)
		err = cctx.Err()
		timedOut = err == context.DeadlineExceeded
	}
	return Result{
		Output:    sink.String(),
		Err:       err,
		Started:   true,
		TimedOut:  timedOut,
		Truncated: sink.Truncated(),
	}
}

func alwaysBlocked(name string) bool {
	switch strings.ToUpper(name) {
	case "DREA_API_KEY", "OPENAI_API_KEY":
		return true
	}
	return false
}

type capWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
			w.truncated = true
		}
		w.buf.Write(p)
	} else if n > 0 {
		w.truncated = true
	}
	return n, nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
