//go:build linux
// +build linux

package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunShellKillsGrandchildOnTimeout(t *testing.T) {
	requireBash(t)
	pidFile := t.TempDir() + "/child.pid"
	command := fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile)

	start := time.Now()
	_, _, timedOut, err := RunShell(context.Background(), t.TempDir(), command, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !timedOut {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not return promptly")
	}
	assertProcessGone(t, readPID(t, pidFile))
}

func TestRunShellKillsGrandchildOnCancel(t *testing.T) {
	requireBash(t)
	pidFile := t.TempDir() + "/child.pid"
	command := fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForFile(pidFile, 2*time.Second)
		cancel()
	}()

	start := time.Now()
	_, code, timedOut, err := RunShell(ctx, t.TempDir(), command, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if timedOut || code != -1 {
		t.Fatalf("cancel result: code=%d timedOut=%v", code, timedOut)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation did not return promptly")
	}
	assertProcessGone(t, readPID(t, pidFile))
}

func TestRunShellOutputIsBounded(t *testing.T) {
	requireBash(t)
	out, _, _, err := RunShell(context.Background(), t.TempDir(), "yes x | head -c 500000", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutputBytes+256 || !strings.Contains(out, "output truncated") {
		t.Fatalf("output was not capped: %d bytes", len(out))
	}
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func waitForFile(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	return pid
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after cancellation", pid)
}

func processRunning(pid int) bool {
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
