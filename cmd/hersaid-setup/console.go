package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// console is the terminal an enrolment asks its questions at.
//
// There is one type rather than an interface with a real and a fake
// implementation, because the part worth testing is the masking loop: what it
// echoes, what it does with a backspace, and what it refuses to put on screen.
// Only raw differs between a real terminal and a scripted test — it is what
// stops the terminal echoing for the duration of a masked prompt, and it is nil
// where there is no terminal to change. Everything else is the same code in
// both.
type console struct {
	in  *bufio.Reader
	out io.Writer
	raw func() (restore func(), err error)
}

// errCancelled is Ctrl-C or Ctrl-D at a prompt. During a masked prompt the
// terminal is not generating signals, so cancelling is this loop's job rather
// than the kernel's.
var errCancelled = errors.New("cancelled")

func errIsCancelled(err error) bool { return errors.Is(err, errCancelled) }

// openTerminal returns the console to prompt at, or nil when in is not a
// terminal. A pipe, a file and /dev/null all answer nil, which is what the
// non-interactive skip rests on: with no terminal there is no way to mask
// input, and a token typed at a visible prompt is a token in the scrollback.
func openTerminal(in io.Reader, out io.Writer) *console {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return &console{
		in:  bufio.NewReader(f),
		out: out,
		raw: func() (func(), error) { return enterNoEcho(f) },
	}
}

// Line reads an answer that the terminal echoes itself, which is right for a
// y/N: it is not a secret, and seeing it typed is how someone knows the prompt
// took the keystroke.
func (c *console) Line(prompt string) (string, error) {
	fmt.Fprint(c.out, prompt)
	s, err := c.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || s == "") {
		if errors.Is(err, io.EOF) {
			return "", errCancelled
		}
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// Secret reads a token without echoing it.
func (c *console) Secret(prompt string) (string, error) {
	fmt.Fprint(c.out, prompt)
	if c.raw != nil {
		// The terminal stops echoing inside c.raw and starts again at the
		// restore, and a process killed in between leaves the operator's shell
		// that way — visibly broken, and broken well after the tool they would
		// connect it to has exited. Ctrl-C cannot land there, since the same
		// window is what stops the terminal generating it; this is for a signal
		// sent from somewhere else.
		//
		// So the guard is armed before the terminal is touched rather than
		// after, and waits for the restore it will need: armed afterwards, a
		// signal in between would find no handler at all. Waiting cannot hang
		// the signal — c.raw is a pair of terminal ioctls, and it always
		// publishes something here, nil included.
		resolved := make(chan func(), 1)
		stop := onAbort(func() {
			if restore := <-resolved; restore != nil {
				restore()
			}
		})
		defer stop()

		restore, err := c.raw()
		resolved <- restore
		if err != nil {
			return "", err
		}
		defer restore()
	}

	token, err := readMasked(c.in, c.out)
	// A paste that ended CRLF leaves its second byte behind, and the next
	// prompt would read it as an answer nobody gave — on the one prompt where
	// the silent default decides whether a credential is replaced. Only what
	// has already arrived is taken, so a later keystroke is never eaten.
	for c.in.Buffered() > 0 {
		r, _, e := c.in.ReadRune()
		if e != nil {
			break
		}
		if r != '\n' && r != '\r' {
			_ = c.in.UnreadRune()
			break
		}
	}
	return token, err
}

// abortSignals are the catchable ways this process can be told to stop, the
// same three the audit log guards its own critical window against.
var abortSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// exit is a variable so a test can watch the signal path without ending the
// test binary.
var exit = os.Exit

// onAbort runs f for the first of those signals and then exits with the
// conventional 128+n code. The returned function stops handling and must be
// called once the window it guards has closed.
func onAbort(f func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, abortSignals...)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			f()
			exit(exitCode(sig))
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// signalExitBase is the shell convention: a process killed by signal n exits
// with 128+n.
const signalExitBase = 128

func exitCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return signalExitBase + int(s)
	}
	return 1
}

// The control characters a masked prompt has to answer for itself, because the
// terminal has been told to stop interpreting them.
const (
	ctrlC     = '\x03'
	ctrlD     = '\x04'
	backspace = '\x08'
	del       = '\x7f'
)

// readMasked reads one line, echoing an asterisk per character rather than
// nothing.
//
// A silent prompt gives no way to tell a failed paste from a successful one —
// no feedback at all until the token is either accepted or rejected minutes
// later — so the count of asterisks is the first thing that shows a paste
// arrived and roughly how much of it did.
func readMasked(in *bufio.Reader, echo io.Writer) (string, error) {
	var out []rune
	for {
		r, _, err := in.ReadRune()
		switch {
		case errors.Is(err, io.EOF):
			// What was typed before the input ended is kept rather than
			// discarded: a truncated token fails verification, which is a
			// better answer than an unexplained empty prompt.
			fmt.Fprintln(echo)
			return string(out), nil
		case err != nil:
			return "", err
		}

		switch r {
		case '\r', '\n':
			fmt.Fprintln(echo)
			return string(out), nil
		case ctrlC, ctrlD:
			fmt.Fprintln(echo)
			return "", errCancelled
		case backspace, del:
			if len(out) > 0 {
				out = out[:len(out)-1]
				// Back over the asterisk, overwrite it with a space, back
				// again: a terminal has no other way to unprint a character.
				fmt.Fprint(echo, "\b \b")
			}
		default:
			out = append(out, r)
			fmt.Fprint(echo, "*")
		}
	}
}
