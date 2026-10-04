package cli

import (
	"bytes"
	"strings"
	"testing"
)

const usage = "usage: sez [--send] <#chan> [text]\n"

// Both flags were asked for, so they are output: stdout, ExitOK. A usage error
// is the opposite case and is Usage's business.
func TestHelpIsOutputNotADiagnostic(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if code := Help(&stdout, usage); code != ExitOK {
		t.Errorf("Help() = %d, want ExitOK — it was asked for, not misused", code)
	}
	if stdout.String() != usage {
		t.Errorf("Help() printed %q, want the usage text", stdout.String())
	}
}

func TestInterceptAnswersALeadingFlag(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--help", "whoami"}} {
		var stdout bytes.Buffer
		code, handled := Intercept(args, "hear", usage, &stdout)
		if !handled {
			t.Errorf("Intercept(%v) did not handle the request", args)
			continue
		}
		if code != ExitOK {
			t.Errorf("Intercept(%v) = %d, want ExitOK", args, code)
		}
		if stdout.String() != usage {
			t.Errorf("Intercept(%v) printed %q, want the usage text", args, stdout.String())
		}
	}
}

func TestVersionNamesTheCommand(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	code, handled := Intercept([]string{"--version"}, "tap", usage, &stdout)
	if !handled || code != ExitOK {
		t.Fatalf("Intercept(--version) = %d, %v", code, handled)
	}
	if !strings.HasPrefix(stdout.String(), "tap ") {
		t.Fatalf("stdout = %q, want it to start with the command name", stdout.String())
	}
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Fatalf("stdout = %q, want one line", stdout.String())
	}
	if strings.Contains(stdout.String(), "usage") {
		t.Fatalf("stdout = %q, want the version alone", stdout.String())
	}
}

// Only the first argument is examined. Anywhere else, a command's own parser
// decides: it is the only thing that knows which options take a value, and that
// "--help" after "--thread" is a timestamp rather than a request for help.
func TestInterceptLooksAtTheFirstArgumentOnly(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--thread", "--help"},
		{"--send", "--help"},
		{"#eng", "--help"},
		{"#eng", "run it with --help and see"},
		{"--", "--help"},
		{"whoami", "--version"},
		{},
	} {
		var stdout bytes.Buffer
		if _, handled := Intercept(args, "sez", usage, &stdout); handled {
			t.Errorf("Intercept(%v) answered for an argument that is not the first", args)
		}
		if stdout.Len() != 0 {
			t.Errorf("Intercept(%v) wrote %q", args, stdout.String())
		}
	}
}

// A usage error is a diagnostic: stderr, ExitUsage. Help is not — separating
// the two is what lets a caller tell "you asked me for help" from "you got it
// wrong".
func TestUsageIsADiagnostic(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := Usage(&stderr, "sez", usage, "unknown flag: --sned"); code != ExitUsage {
		t.Fatalf("Usage() = %d, want ExitUsage", code)
	}
	if !strings.Contains(stderr.String(), "sez: unknown flag: --sned") {
		t.Fatalf("stderr = %q, want it to name what was wrong", stderr.String())
	}
	if !strings.Contains(stderr.String(), usage) {
		t.Fatalf("stderr = %q, want the usage text too", stderr.String())
	}
}
