package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// read drives the production masking loop over a scripted input and returns
// what it accepted alongside what it put on screen.
func read(t *testing.T, input string) (got string, echoed string, err error) {
	t.Helper()
	var out bytes.Buffer
	got, err = readMasked(bufio.NewReader(strings.NewReader(input)), &out)
	return got, out.String(), err
}

func TestMaskedInputEchoesOneAsteriskPerCharacter(t *testing.T) {
	got, echoed, err := read(t, "xoxp-abc123\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "xoxp-abc123" {
		t.Errorf("read %q", got)
	}
	if want := strings.Repeat("*", len("xoxp-abc123")) + "\n"; echoed != want {
		t.Errorf("echoed %q, want %q", echoed, want)
	}
}

// One per *character*, not per byte: a paste that is not ASCII is the wrong
// token, but the prompt still has to count it the way a person does.
func TestMaskedInputCountsCharactersNotBytes(t *testing.T) {
	got, echoed, err := read(t, "héllo\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "héllo" {
		t.Errorf("read %q", got)
	}
	if want := "*****\n"; echoed != want {
		t.Errorf("echoed %q, want %q — five characters, not six bytes", echoed, want)
	}
}

// Backspace has to work, or a mistyped character can only be fixed by starting
// again — and with nothing on screen but asterisks, by starting again blind.
func TestBackspaceRemovesACharacterAndItsAsterisk(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{name: "delete", key: string(del)},
		{name: "backspace", key: string(backspace)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, echoed, err := read(t, "abX"+tc.key+"c\n")
			if err != nil {
				t.Fatal(err)
			}
			if got != "abc" {
				t.Errorf("read %q, want %q", got, "abc")
			}
			// Three asterisks, then the erase sequence, then one more.
			if want := "***\b \b*\n"; echoed != want {
				t.Errorf("echoed %q, want %q", echoed, want)
			}
		})
	}
}

// Backspace on an empty prompt does nothing at all — no underflow, and nothing
// that would rub out the prompt text sitting to the left of the cursor.
func TestBackspaceOnEmptyInputDoesNothing(t *testing.T) {
	got, echoed, err := read(t, string(del)+string(del)+"a\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a" {
		t.Errorf("read %q", got)
	}
	if want := "*\n"; echoed != want {
		t.Errorf("echoed %q, want %q", echoed, want)
	}
}

// Ctrl-C and Ctrl-D cancel. During a masked prompt the terminal has been told
// to stop generating signals, so this loop is the only thing that can answer
// them; without it Ctrl-C would be stored as part of the token.
func TestControlCharactersCancel(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{name: "ctrl-c", key: string(ctrlC)},
		{name: "ctrl-d", key: string(ctrlD)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := read(t, "xoxp-abc"+tc.key)
			if !errors.Is(err, errCancelled) {
				t.Fatalf("err = %v, want errCancelled", err)
			}
			if got != "" {
				t.Errorf("returned %q; a cancelled prompt yields nothing", got)
			}
		})
	}
}

func TestEnterEndsInput(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{name: "newline", key: "\n"},
		{name: "carriage return", key: "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := read(t, "abc"+tc.key+"ignored")
			if err != nil {
				t.Fatal(err)
			}
			if got != "abc" {
				t.Errorf("read %q, want %q", got, "abc")
			}
		})
	}
}

// Input that ends without a newline yields what was typed. A truncated token
// fails verification, which says more than an unexplained empty prompt would.
func TestEndOfInputYieldsWhatWasTyped(t *testing.T) {
	got, _, err := read(t, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Errorf("read %q, want %q", got, "abc")
	}
}

// The one thing the prompt must never do.
func TestMaskedInputNeverEchoesWhatWasTyped(t *testing.T) {
	const secret = "xoxp-9876543210-secret-material"
	_, echoed, err := read(t, secret+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(echoed, "xop0123456789-") {
		t.Fatalf("echo contains characters from the token: %q", echoed)
	}
}

// A y/N answer is not a secret, and seeing it typed is how somebody knows the
// prompt took the keystroke — so Line leaves the echoing to the terminal and
// adds none of its own.
func TestLineReadsAnEchoedAnswerWithoutEchoingIt(t *testing.T) {
	var out bytes.Buffer
	c := &console{in: bufio.NewReader(strings.NewReader("yes\nnext\n")), out: &out}

	got, err := c.Line("Set up? [y/N] ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "yes" {
		t.Errorf("read %q, want %q", got, "yes")
	}
	if out.String() != "Set up? [y/N] " {
		t.Errorf("wrote %q, want the prompt alone", out.String())
	}
}

func TestLineCancelsWhenThereIsNoAnswer(t *testing.T) {
	c := &console{in: bufio.NewReader(strings.NewReader("")), out: io.Discard}
	if _, err := c.Line("? "); !errors.Is(err, errCancelled) {
		t.Fatalf("err = %v, want errCancelled", err)
	}
}

func TestAffirmative(t *testing.T) {
	tests := []struct {
		answer string
		want   bool
	}{
		{answer: "y", want: true},
		{answer: "Y", want: true},
		{answer: "yes", want: true},
		{answer: "Yep", want: true},
		{answer: " y ", want: true},
		{answer: "", want: false},
		{answer: "n", want: false},
		{answer: "no", want: false},
		{answer: "maybe", want: false},
		// Not a yes: the default has to be the answer that stores nothing.
		{answer: "sure", want: false},
	}
	for _, tc := range tests {
		if got := affirmative(tc.answer); got != tc.want {
			t.Errorf("affirmative(%q) = %v, want %v", tc.answer, got, tc.want)
		}
	}
}

// The predicate the non-interactive skip rests on. /dev/null is the case a
// mode-bit check gets wrong: it is a character device, and prompting into it
// would mask nothing because there is nobody there.
func TestOpenTerminalRefusesAnythingThatIsNotATerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, tc := range []struct {
		name string
		in   io.Reader
	}{
		{name: "/dev/null", in: devNull},
		{name: "a pipe", in: r},
		{name: "a regular file", in: f},
		{name: "not a file at all", in: strings.NewReader("y\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := openTerminal(tc.in, io.Discard); c != nil {
				t.Fatal("openTerminal returned a console for something that is not a terminal")
			}
		})
	}
}

// A CRLF paste ends the masked line with a byte to spare. Left in the buffer,
// the next prompt reads it as an answer nobody gave.
func TestACRLFPasteDoesNotSupplyTheNextAnswer(t *testing.T) {
	var out bytes.Buffer
	c := &console{in: bufio.NewReader(strings.NewReader("xoxp-abc\r\ny\r\n")), out: &out}

	got, err := c.Secret("  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "xoxp-abc" {
		t.Fatalf("read %q", got)
	}
	answer, err := c.Line("Replace it? [y/N] ")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "y" {
		t.Fatalf("the next prompt read %q, want the answer that was actually typed", answer)
	}
}

// Only what has already arrived is taken. A prompt must never consume a
// keystroke that has not been typed yet.
func TestSecretDoesNotWaitForOrEatALaterKeystroke(t *testing.T) {
	var out bytes.Buffer
	c := &console{in: bufio.NewReader(strings.NewReader("xoxp-abc\n\n")), out: &out}

	if _, err := c.Secret("  "); err != nil {
		t.Fatal(err)
	}
	// The blank line that follows is an answer in its own right — "no" — and
	// eating it would turn a deliberate decline into a silent one.
	answer, err := c.Line("Replace it? [y/N] ")
	if err != nil && !errors.Is(err, errCancelled) {
		t.Fatal(err)
	}
	if answer != "" {
		t.Fatalf("the next prompt read %q, want the empty answer that was typed", answer)
	}
}

// swapExit stops the signal goroutine ending the test binary and reports the
// code it asked for, the same way the audit package tests its own guard.
func swapExit(t *testing.T) chan int {
	t.Helper()
	codes := make(chan int, 1)
	prev := exit
	exit = func(code int) {
		codes <- code
		// Park: the real os.Exit does not return, and letting the goroutine
		// fall through would run code the production path never reaches.
		select {}
	}
	t.Cleanup(func() { exit = prev })
	return codes
}

// A signal arriving while the terminal is not echoing must put it back. The
// window is short, but what it leaves behind is somebody's shell with echo off,
// long after the tool they would connect it to has gone.
func TestOnAbortRestoresTheTerminalAndExits(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{
		{syscall.SIGINT, 130},
		{syscall.SIGTERM, 143},
		{syscall.SIGHUP, 129},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			codes := swapExit(t)
			restored := make(chan struct{})
			stop := onAbort(func() { close(restored) })
			defer stop()

			if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-restored:
			case <-time.After(5 * time.Second):
				t.Fatal("the terminal was never restored")
			}
			select {
			case code := <-codes:
				if code != tc.code {
					t.Fatalf("exit code = %d, want %d", code, tc.code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the process never exited")
			}
		})
	}
}

// stop has to actually stop: a guard left armed after the masked prompt would
// turn every later signal into an exit through a restore that no longer
// applies. A signal is sent, because a test that sends none passes whether or
// not stop does anything.
func TestOnAbortStopsListening(t *testing.T) {
	swapExit(t)

	// With the guard stopped, nothing else would take the signal and the
	// default disposition would kill the test binary.
	sink := make(chan os.Signal, 1)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	fired := make(chan struct{}, 1)
	stop := onAbort(func() { fired <- struct{}{} })
	stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The runtime has forwarded the signal to everything still registered by
	// the time this returns, so a handler that was going to run has run.
	select {
	case <-sink:
	case <-time.After(5 * time.Second):
		t.Fatal("the signal was never delivered")
	}
	select {
	case <-fired:
		t.Fatal("the handler ran after stop")
	case <-time.After(50 * time.Millisecond):
	}
}

// The guard has to be wired to the restore, not merely armed. A handler that
// runs and restores nothing leaves exactly the terminal it exists to prevent,
// and an armed-but-empty guard is indistinguishable from a working one until
// somebody's shell stops echoing.
func TestSecretRestoresTheTerminalWhenASignalArrives(t *testing.T) {
	codes := swapExit(t)

	// Registered before anything is signalled, so a signal arriving before the
	// guard is armed is absorbed here rather than killing the test binary.
	sink := make(chan os.Signal, 8)
	signal.Notify(sink, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sink) })

	// A pipe nobody writes to: Secret blocks in the masked read, which is the
	// window the guard covers.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// raw holds the test inside the window the fix is about: the terminal has
	// been changed and the restore has not been handed back yet. A guard armed
	// after raw returns is not armed here at all.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	restored := make(chan struct{}, 4)
	c := &console{
		in:  bufio.NewReader(r),
		out: io.Discard,
		raw: func() (func(), error) {
			entered <- struct{}{}
			<-release
			return func() {
				select {
				case restored <- struct{}{}:
				default:
				}
			}, nil
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Secret("  ")
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the masked prompt never started")
	}

	// Sent once, and while raw is still in flight. Waiting on the sink is what
	// makes that deterministic: the runtime has then forwarded the signal to
	// every channel registered at this moment, so a guard armed after raw
	// returns has provably missed it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink:
	case <-time.After(5 * time.Second):
		t.Fatal("the signal was never delivered")
	}
	close(release)

	select {
	case <-restored:
	case <-time.After(5 * time.Second):
		t.Fatal("a signal during the masked prompt left the terminal unrestored")
	}

	// Waited for, not assumed: the handler reads the exit function after it
	// restores, and letting the test return first would have its cleanup swap
	// that variable out from under a goroutine still using it.
	select {
	case code := <-codes:
		if code != 143 {
			t.Errorf("exit code = %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the process never exited")
	}

	// Unblock the masked read so Secret returns and disarms the guard, rather
	// than leaving it registered for whatever runs next.
	w.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Secret never returned")
	}
}
