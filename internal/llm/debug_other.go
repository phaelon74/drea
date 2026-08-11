//go:build !linux
// +build !linux

package llm

import (
	"errors"
	"os"
)

func openDebugAppend(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err == nil && (before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular()) {
		return nil, errors.New("debug log must be a regular file, not a symlink")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(after, opened) {
		f.Close()
		return nil, errors.New("debug log changed while opening")
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
