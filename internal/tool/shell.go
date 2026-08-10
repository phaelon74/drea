package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// runCommand executes a shell command inside the workspace root. It is the
// most powerful tool, so it is Mutating (requires approval by default) and is
// bounded by a wall-clock timeout with combined stdout/stderr capture.
type runCommand struct{ root string }

// maxCommandTimeout caps how long any single command may run.
const maxCommandTimeout = 15 * time.Minute

// defaultCommandTimeout is used when the model omits a timeout.
const defaultCommandTimeout = 120 * time.Second

// maxOutputBytes bounds captured output so a runaway command cannot exhaust
// memory or flood the model's context.
const maxOutputBytes = 64 * 1024

func (t *runCommand) Name() string   { return "run_command" }
func (t *runCommand) Mutating() bool { return true }
func (t *runCommand) Description() string {
	return "Run a shell command with 'bash -c' from the workspace root. Combined stdout and stderr are returned along with the exit code. Use this to build, run tests, install project-local dependencies, use git, etc. Long-running or interactive processes are not supported."
}
func (t *runCommand) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","description":"The shell command to execute."},
    "timeout_seconds":{"type":"integer","description":"Wall-clock timeout in seconds. Default 120, max 900."}
  },
  "required":["command"]
}`)
}
func (t *runCommand) Summary(args json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = decode(args, &a)
	cmd := strings.TrimSpace(a.Command)
	if len(cmd) > 120 {
		cmd = cmd[:117] + "..."
	}
	return "$ " + cmd
}
func (t *runCommand) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout := defaultCommandTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}
	if timeout > maxCommandTimeout {
		timeout = maxCommandTimeout
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Run in its own process group so a timeout/cancel kills the whole tree
	// (bash plus any children), not just the bash leader.
	cmd := exec.Command("bash", "-c", a.Command)
	cmd.Dir = t.root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cap output as it is produced so a runaway command cannot exhaust memory;
	// the process keeps running (its writes are discarded past the cap).
	sink := &capWriter{limit: maxOutputBytes}
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		return "", err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	select {
	case <-cctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done // reap
		runErr = cctx.Err()
	case runErr = <-done:
	}
	timedOut := cctx.Err() == context.DeadlineExceeded

	text := sink.String()
	if sink.overflow {
		text += "\n… (output truncated)"
	}

	var b strings.Builder
	if timedOut {
		fmt.Fprintf(&b, "[timed out after %s]\n", timeout)
	}
	b.WriteString(text)
	switch {
	case runErr == nil:
		b.WriteString("\n[exit code 0]")
	case timedOut:
		// message already emitted above
	default:
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			fmt.Fprintf(&b, "\n[exit code %d]", exitErr.ExitCode())
		} else {
			fmt.Fprintf(&b, "\n[error: %v]", runErr)
		}
	}
	return b.String(), nil
}

// RunShell executes command with 'bash -c' from dir, bounded by timeout and a
// combined-output cap, in its own process group (killed as a tree on
// timeout/cancel). It returns the captured output, the exit code (-1 when the
// command did not exit normally), and whether it timed out. It is exported so
// the agent's verification loop reuses the same hardened runner as the
// run_command tool. err is non-nil only for failures to start the process.
func RunShell(ctx context.Context, dir, command string, timeout time.Duration) (output string, exitCode int, timedOut bool, err error) {
	return runArgv(ctx, dir, "bash", []string{"-c", command}, timeout)
}

// runArgv executes name with an explicit argument list (never through a shell)
// from dir, bounded by timeout and the combined-output cap, in its own process
// group (killed as a tree on timeout/cancel). Passing an explicit argv avoids
// shell interpretation, which is what the git tools rely on to stay confined.
func runArgv(ctx context.Context, dir, name string, args []string, timeout time.Duration) (output string, exitCode int, timedOut bool, err error) {
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	if timeout > maxCommandTimeout {
		timeout = maxCommandTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sink := &capWriter{limit: maxOutputBytes}
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		return "", -1, false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	select {
	case <-cctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		runErr = cctx.Err()
	case runErr = <-done:
	}
	timedOut = cctx.Err() == context.DeadlineExceeded

	text := sink.String()
	if sink.overflow {
		text += "\n… (output truncated)"
	}
	switch {
	case runErr == nil:
		exitCode = 0
	case timedOut:
		exitCode = -1
	default:
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			text += fmt.Sprintf("\n[error: %v]", runErr)
		}
	}
	return text, exitCode, timedOut, nil
}

// capWriter buffers up to limit bytes and discards the rest, recording whether
// any data was dropped. Write never blocks or errors, so the child process is
// free to keep producing output that we simply ignore past the cap.
type capWriter struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.overflow = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.overflow = true
	}
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }
