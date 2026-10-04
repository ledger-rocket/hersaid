//go:build darwin || linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal answers the only question enrolment has about a file descriptor:
// whether its input mode can be changed at all. os.ModeCharDevice would also
// call /dev/null a terminal, and prompting into /dev/null would mask nothing.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// enterNoEcho stops the terminal echoing, stops it buffering a line before
// handing it over, and stops it turning Ctrl-C into a signal, for the duration
// of one masked prompt. Output processing is deliberately left alone: only the
// input side changes, so a newline still returns the cursor to column 0.
//
// ISIG is off so that Ctrl-C arrives as a byte the read loop can answer. That
// is not only for a tidier cancellation message: a signal delivered here would
// kill the process between this call and the restore below, and leave the
// operator with a shell that has stopped echoing.
func enterNoEcho(f *os.File) (func(), error) {
	fd := int(f.Fd())
	prev, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	next := *prev
	next.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	// With ICANON off the driver needs telling when a read is satisfied: one
	// byte, no timer, which is byte-at-a-time.
	next.Cc[unix.VMIN] = 1
	next.Cc[unix.VTIME] = 0
	err = unix.IoctlSetTermios(fd, ioctlSetTermios, &next)
	if err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, prev) }, nil
}
