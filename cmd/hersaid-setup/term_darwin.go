//go:build darwin

package main

import "golang.org/x/sys/unix"

// The termios ioctls spell the same two operations differently per kernel.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
