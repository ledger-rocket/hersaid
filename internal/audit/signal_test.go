package audit

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// swapExit stops the signal goroutine ending the test binary and reports the
// code it asked for.
func swapExit(t *testing.T) chan int {
	t.Helper()
	codes := make(chan int, 1)
	prev := exit
	exit = func(code int) {
		codes <- code
		// Park: the real os.Exit does not return, and letting the goroutine fall
		// through would run code the production path never reaches.
		select {}
	}
	t.Cleanup(func() { exit = prev })
	return codes
}

// A catchable signal during the send window must still leave a record, and the
// process must exit with the conventional 128+n code.
func TestOnAbortRecordsAndExits(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{
		{syscall.SIGINT, 130},
		{syscall.SIGTERM, 143},
		{syscall.SIGHUP, 129},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			l, path := openTemp(t)
			codes := swapExit(t)

			got := make(chan os.Signal, 1)
			stop := OnAbort(func(sig os.Signal) {
				if err := l.Post(Post{Channel: "#eng", Phase: Abort, OK: Unknown,
					Error: "interrupted by " + sig.String()}); err != nil {
					t.Error(err)
				}
				got <- sig
			})
			defer stop()

			if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}

			select {
			case sig := <-got:
				if sig != tc.sig {
					t.Fatalf("handler saw %v, want %v", sig, tc.sig)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the signal handler never ran")
			}

			select {
			case code := <-codes:
				if code != tc.code {
					t.Fatalf("exit code = %d, want %d", code, tc.code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the process never exited")
			}

			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"phase":"abort"`) {
				t.Fatalf("log = %q, want an abort record", b)
			}
		})
	}
}

// Once the write is done and recorded, a later signal is no longer this
// package's business: stop must unregister the handler.
func TestStopUnregistersTheHandler(t *testing.T) {
	swapExit(t)
	ran := make(chan os.Signal, 1)
	stop := OnAbort(func(sig os.Signal) { ran <- sig })
	stop()

	// Re-notify so the default action (killing the test binary) stays suppressed
	// while proving the OnAbort handler no longer fires.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-guard:
	case <-time.After(5 * time.Second):
		t.Fatal("the signal was never delivered")
	}
	select {
	case sig := <-ran:
		t.Fatalf("the stopped handler ran for %v", sig)
	default:
	}
}
