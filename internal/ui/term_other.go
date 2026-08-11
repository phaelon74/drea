//go:build !linux
// +build !linux

package ui

// termWidth returns a sensible default terminal width on non-Linux platforms
// where the TIOCGWINSZ ioctl is not available through this code path.
func termWidth() int {
	return 80
}
