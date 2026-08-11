//go:build !linux
// +build !linux

package process

import (
	"os/exec"
	"time"
)

func configureCommand(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func waitAfterKill(done <-chan error) {
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
