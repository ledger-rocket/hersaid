//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

// darwin and linux are the platforms hersaid targets; elsewhere there is no
// implementation of "stop echoing", and the honest answer to "is this a
// terminal I can prompt at" is no. Enrolment then reports itself
// non-interactive and stores nothing, rather than prompting for a token that
// would appear on screen as it was typed.
func isTerminal(*os.File) bool { return false }

func enterNoEcho(*os.File) (restore func(), err error) {
	return nil, errors.New("masked input is not supported on this platform")
}
