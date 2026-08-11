package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	dreaprocess "github.com/dreaagent/drea/internal/process"
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

// SetPassEnv configures which credential-like environment variable names may
// be passed through to child processes despite scrubbing. DREA_API_KEY and
// OPENAI_API_KEY are never passed, even when listed.
func SetPassEnv(names []string) {
	dreaprocess.SetPassEnv(names)
}

// PassEnv returns a copy of the current pass-through allow-list.
func PassEnv() []string {
	return dreaprocess.PassEnv()
}

func (t *runCommand) Name() string        { return "run_command" }
func (t *runCommand) Mutating() bool      { return true }
func (t *runCommand) AlwaysConfirm() bool { return false }
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

	text, exitCode, timedOut, err := runArgv(ctx, t.root, "bash", []string{"-c", a.Command}, timeout)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if timedOut {
		fmt.Fprintf(&b, "[timed out after %s]\n", timeout)
	}
	b.WriteString(text)
	switch {
	case exitCode == 0:
		b.WriteString("\n[exit code 0]")
	case timedOut:
		// message already emitted above
	default:
		fmt.Fprintf(&b, "\n[exit code %d]", exitCode)
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
	result := dreaprocess.Run(ctx, dir, append([]string{name}, args...), timeout, maxOutputBytes)
	if !result.Started {
		return "", -1, false, result.Err
	}
	text := result.Output
	if result.Truncated {
		text += "\n… (output truncated)"
	}
	timedOut = result.TimedOut
	switch {
	case result.Err == nil:
		exitCode = 0
	case timedOut:
		exitCode = -1
	default:
		if exitErr, ok := result.Err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
			text += fmt.Sprintf("\n[error: %v]", result.Err)
		}
	}
	return text, exitCode, timedOut, nil
}
