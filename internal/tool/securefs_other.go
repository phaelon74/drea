//go:build !linux

package tool

import (
	"errors"
	"os"
	"path/filepath"
)

type fileIdentity struct {
	info os.FileInfo
}

func openSecureRegular(_ string, path string) (*os.File, os.FileMode, fileIdentity, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fileIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, 0, fileIdentity{}, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fileIdentity{}, err
	}
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fileIdentity{}, err
	}
	if !os.SameFile(before, after) {
		f.Close()
		return nil, 0, fileIdentity{}, errors.New("destination changed during operation")
	}
	return f, after.Mode().Perm(), fileIdentity{info: after}, nil
}

func readSecureDir(_ string, path string) ([]os.DirEntry, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a directory")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		return nil, errors.New("directory changed during operation")
	}
	return f.ReadDir(-1)
}

func inspectSecureTarget(root, path string) (os.FileMode, *fileIdentity, error) {
	f, mode, id, err := openSecureRegular(root, path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0o644, nil, nil
		}
		return 0, nil, err
	}
	if err := f.Close(); err != nil {
		return 0, nil, err
	}
	return mode, &id, nil
}

// Non-Linux platforms lack the stdlib fd-relative primitives used by the
// Linux implementation. Revalidation narrows races but cannot eliminate the
// final check-to-rename window against a concurrent hostile process.
func secureWriteAtomic(_ string, path string, data []byte, mode os.FileMode, expected *fileIdentity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if expected == nil {
		if err == nil {
			return errors.New("destination appeared during operation")
		}
		if !os.IsNotExist(err) {
			return err
		}
	} else {
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || !os.SameFile(expected.info, current) {
			return errors.New("destination changed during operation")
		}
	}
	return writeFileAtomic(path, data, mode)
}
