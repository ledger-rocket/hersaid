//go:build linux

package main

import "golang.org/x/sys/unix"

// The termios ioctls spell the same two operations differently per kernel.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
