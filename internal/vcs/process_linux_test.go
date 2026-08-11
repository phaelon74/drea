//go:build linux
// +build linux

package vcs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	dreaprocess "github.com/dreaagent/drea/internal/process"
)

func TestGitHookGrandchildKilledOnCancel(t *testing.T) {
	requireGitAndBash(t)
	dir := repo(t)
	write(t, dir, "hooked.txt", "hooked\n")
	pidFile := filepath.Join(t.TempDir(), "hook-child.pid")
	hook := fmt.Sprintf("#!/bin/bash\nsleep 30 &\necho $! > %q\nwait\n", pidFile)
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForVCSFile(pidFile, 2*time.Second)
		cancel()
	}()
	start := time.Now()
	_, _, err := Checkpoint(ctx, dir, "blocked by hook")
	if err == nil {
		t.Fatal("expected canceled commit")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("canceled git hook did not return promptly")
	}
	assertVCSProcessGone(t, readVCSPID(t, pidFile))
}

func TestGitHookGrandchildKilledOnTimeout(t *testing.T) {
	requireGitAndBash(t)
	dir := repo(t)
	write(t, dir, "hooked.txt", "hooked\n")
	pidFile := filepath.Join(t.TempDir(), "hook-child.pid")
	hook := fmt.Sprintf("#!/bin/bash\nsleep 30 &\necho $! > %q\nwait\n", pidFile)
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	oldTimeout := commandTimeout
	commandTimeout = time.Second
	defer func() { commandTimeout = oldTimeout }()
	start := time.Now()
	_, _, err := Checkpoint(context.Background(), dir, "blocked by hook")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timed-out git hook did not return promptly")
	}
	assertVCSProcessGone(t, readVCSPID(t, pidFile))
}

func TestGitOutputIsBounded(t *testing.T) {
	requireGitAndBash(t)
	dir := repo(t)
	out, err := run(context.Background(), dir,
		"-c", "alias.spam=!yes x | head -c 500000", "spam")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutput+256 || !strings.Contains(out, "output truncated") {
		t.Fatalf("git output was not capped: %d bytes", len(out))
	}
}

func TestGitUsesScrubbedMinimalEnvironment(t *testing.T) {
	requireGitAndBash(t)
	t.Setenv("DREA_API_KEY", "drea-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-secret")
	t.Setenv("GH_TOKEN", "explicit-token")
	dreaprocess.SetPassEnv([]string{"GH_TOKEN"})
	defer dreaprocess.SetPassEnv(nil)

	out, err := run(context.Background(), repo(t), "-c",
		"alias.env=!printf '%s|%s|%s|%s' \"$DREA_API_KEY\" \"$AWS_ACCESS_KEY_ID\" \"$GH_TOKEN\" \"$HOME\"",
		"env")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out, "|")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "" || parts[2] != "explicit-token" || parts[3] == "" {
		t.Fatalf("unexpected git environment %q", out)
	}
}

func requireGitAndBash(t *testing.T) {
	t.Helper()
	for _, name := range []string{"git", "bash"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not available", name)
		}
	}
}

func waitForVCSFile(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readVCSPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse hook child pid: %v", err)
	}
	return pid
}

func assertVCSProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !vcsProcessRunning(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("hook process %d still exists after cancellation", pid)
}

func vcsProcessRunning(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return false
	}
	if err == nil {
		rest := string(stat)
		if i := strings.LastIndex(rest, ") "); i >= 0 && len(rest) > i+2 && rest[i+2] == 'Z' {
			return false
		}
	}
	return syscall.Kill(pid, 0) != syscall.ESRCH
}
