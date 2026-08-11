//go:build linux

package tool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

type fileIdentity struct {
	dev uint64
	ino uint64
}

func openSecureRegular(root, path string) (*os.File, os.FileMode, fileIdentity, error) {
	parent, name, err := openParent(root, path, false)
	if err != nil {
		return nil, 0, fileIdentity{}, err
	}
	defer syscall.Close(parent)
	fd, err := syscall.Openat(parent, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fileIdentity{}, os.NewSyscallError("openat", err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, fileIdentity{}, errors.New("not a regular file")
	}
	st := info.Sys().(*syscall.Stat_t)
	return f, info.Mode().Perm(), fileIdentity{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func readSecureDir(root, path string) ([]os.DirEntry, error) {
	parent, name, err := openParent(root, path, false)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(parent)
	fd, err := syscall.Openat(parent, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, os.NewSyscallError("openat directory", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("not a directory")
	}
	return f.ReadDir(-1)
}

func inspectSecureTarget(root, path string) (os.FileMode, *fileIdentity, error) {
	f, mode, id, err := openSecureRegular(root, path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return 0o644, nil, nil
		}
		return 0, nil, err
	}
	if err := f.Close(); err != nil {
		return 0, nil, err
	}
	return mode, &id, nil
}

func secureWriteAtomic(root, path string, data []byte, mode os.FileMode, expected *fileIdentity) error {
	parent, name, err := openParent(root, path, true)
	if err != nil {
		return err
	}
	defer syscall.Close(parent)

	tmpName, fd, err := createTempAt(parent, mode)
	if err != nil {
		return err
	}
	tmpExists := true
	defer func() {
		if tmpExists {
			_ = syscall.Unlinkat(parent, tmpName)
		}
	}()
	tmp := os.NewFile(uintptr(fd), tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if expected == nil {
		if err := renameat2(parent, tmpName, parent, name, renameNoReplace); err != nil {
			return os.NewSyscallError("renameat2", err)
		}
		tmpExists = false
		if err := syscall.Fsync(parent); err != nil {
			return os.NewSyscallError("fsync parent", err)
		}
		return nil
	}
	if err := renameat2(parent, tmpName, parent, name, renameExchange); err != nil {
		return os.NewSyscallError("renameat2", err)
	}
	swapped := true
	defer func() {
		if swapped {
			_ = renameat2(parent, tmpName, parent, name, renameExchange)
		}
	}()
	oldFD, err := syscall.Openat(parent, tmpName, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return os.NewSyscallError("openat replaced destination", err)
	}
	var st syscall.Stat_t
	statErr := syscall.Fstat(oldFD, &st)
	closeErr := syscall.Close(oldFD)
	if statErr != nil {
		return os.NewSyscallError("fstat replaced destination", statErr)
	}
	if closeErr != nil {
		return os.NewSyscallError("close replaced destination", closeErr)
	}
	if uint64(st.Dev) != expected.dev || st.Ino != expected.ino {
		return errors.New("destination changed during operation")
	}
	if err := syscall.Unlinkat(parent, tmpName); err != nil {
		return os.NewSyscallError("unlinkat", err)
	}
	tmpExists = false
	swapped = false
	if err := syscall.Fsync(parent); err != nil {
		return os.NewSyscallError("fsync parent", err)
	}
	return nil
}

func openParent(root, path string, create bool) (int, string, error) {
	relPath, err := filepath.Rel(root, path)
	if err != nil {
		return -1, "", err
	}
	parts := strings.Split(relPath, string(filepath.Separator))
	if len(parts) == 0 || parts[len(parts)-1] == "" || parts[len(parts)-1] == "." {
		return -1, "", errors.New("invalid file path")
	}
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", os.NewSyscallError("open workspace", err)
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if openErr == syscall.ENOENT && create {
			if err := syscall.Mkdirat(fd, part, 0o755); err != nil && err != syscall.EEXIST {
				syscall.Close(fd)
				return -1, "", os.NewSyscallError("mkdirat", err)
			}
			next, openErr = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			syscall.Close(fd)
			return -1, "", os.NewSyscallError("openat parent", openErr)
		}
		syscall.Close(fd)
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func createTempAt(parent int, mode os.FileMode) (string, int, error) {
	var random [8]byte
	for i := 0; i < 100; i++ {
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return "", -1, err
		}
		name := ".drea-write-" + hex.EncodeToString(random[:])
		fd, err := syscall.Openat(parent, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, uint32(mode.Perm()))
		if err == nil {
			return name, fd, nil
		}
		if err != syscall.EEXIST {
			return "", -1, os.NewSyscallError("openat temp", err)
		}
	}
	return "", -1, errors.New("could not create unique temporary file")
}

const (
	renameNoReplace = 1
	renameExchange  = 2
)

func renameat2(oldDir int, oldName string, newDir int, newName string, flags uintptr) error {
	oldPtr, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPtr, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_RENAMEAT2, uintptr(oldDir), uintptr(unsafe.Pointer(oldPtr)), uintptr(newDir), uintptr(unsafe.Pointer(newPtr)), flags, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
