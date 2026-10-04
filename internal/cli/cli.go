// Package cli holds what every hersaid command must answer the same way: the
// two shared flags, and the exit statuses.
//
// They are shared because the cost of each binary deciding for itself is paid
// by whoever reads the exit code. And they are answered first, before anything
// else the command does — before the audit log is opened or a credential
// resolved. Asking a tool what it does must not
// enrol the user in anything, create a file, or prompt for a keychain unlock.
package cli

import (
	"fmt"
	"io"

	"github.com/ledger-rocket/hersaid/internal/version"
)

// Exit statuses. A caller reading these — an agent, a script, a CI step — can
// only act on them if all three commands agree, so they live here rather than
// in each command.
const (
	// ExitOK: the command did what was asked, including a render or a dry run
	// that deliberately did nothing.
	ExitOK = 0
	// ExitFailure: the command refused, or Slack did.
	ExitFailure = 1
	// ExitUsage: the invocation was wrong — an unknown flag, the wrong number
	// of arguments, an unknown command.
	ExitUsage = 2
	// ExitAuditFailure: something was attempted and the record of it could not
	// be written. A lost record is fatal: an attempt nobody can account for is
	// what the log exists to prevent, and it outranks the outcome it was meant
	// to describe.
	ExitAuditFailure = 3
)

// Help prints usage as output — it was asked for, so stdout and ExitOK. A usage
// *error* is the opposite case: see Usage.
func Help(stdout io.Writer, usage string) int {
	fmt.Fprint(stdout, usage)
	return ExitOK
}

// Version prints the running build of name.
func Version(stdout io.Writer, name string) int {
	fmt.Fprintln(stdout, version.String(name))
	return ExitOK
}

// Usage prints usage as a diagnostic: the command was invoked wrongly, so
// stderr and ExitUsage. msg names what was wrong and may be empty.
func Usage(stderr io.Writer, name, usage, msg string) int {
	if msg != "" {
		fmt.Fprintf(stderr, "%s: %s\n", name, msg)
	}
	fmt.Fprint(stderr, usage)
	return ExitUsage
}

// Intercept answers the shared flags when one leads the command line, for a
// command that dispatches on its first argument rather than parsing options —
// `hear`, and the enrolment binary after it.
//
// It looks at the first argument only. A command with an option parser routes
// the same two flags through Help and Version from inside that parser instead,
// because only the parser knows which options take a value: in
// `sez --thread --help`, "--help" is the thread timestamp, and answering it
// here would turn a malformed invocation into a help screen.
func Intercept(args []string, name, usage string, stdout io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "-h", "--help":
		return Help(stdout, usage), true
	case "--version":
		return Version(stdout, name), true
	}
	return 0, false
}
