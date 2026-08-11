//go:build linux
// +build linux

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

// termWidth returns the current terminal width in columns, falling back to 80
// when it cannot be determined (e.g. output is not a terminal).
func termWidth() int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}
