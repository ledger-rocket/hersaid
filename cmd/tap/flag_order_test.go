package main

import (
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// tap reads options only before the target, so one written after it used to
// become an operand: `tap #eng 1785712596.256909 --remove` added a reaction
// named --remove. Each of tap's options is refused after the target, with no
// request and no audit record.
func TestAFlagAfterTheTargetIsRefused(t *testing.T) {
	cases := map[string][]string{
		"--remove":       {"#eng", "1785712596.256909", "--remove"},
		"-r":             {"#eng", "1785712596.256909", "-r"},
		"--dry-run":      {"#eng", "1785712596.256909", "--dry-run"},
		"-n":             {"#eng", "1785712596.256909", "-n"},
		"--help":         {"#eng", "1785712596.256909", "--help"},
		"-h":             {"#eng", "1785712596.256909", "-h"},
		"--version":      {"#eng", "1785712596.256909", "--version"},
		"--remove=yes":   {"#eng", "1785712596.256909", "--remove=yes"},
		"--reaction":     {"#eng", "1785712596.256909", "thumbsup", "--reaction"},
		"--remove as ts": {"#eng", "--remove", "1785712596.256909", "thumbsup"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			logPath := isolate(t)
			warmCache(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)

			code, stdout, stderr := tap(t, s, args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if got := s.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
			if recs := readLog(t, logPath); len(recs) != 0 {
				t.Fatalf("audit log has %d records, want 0", len(recs))
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing", stdout)
			}
			if lines := strings.Count(stderr, "\n"); lines != 1 {
				t.Fatalf("stderr has %d lines, want 1:\n%s", lines, stderr)
			}
			flag := strings.TrimSuffix(name, " as ts")
			if !strings.Contains(stderr, "tap: "+flag+" comes after the target") {
				t.Fatalf("stderr does not name %s as misplaced: %q", flag, stderr)
			}
			if !strings.Contains(stderr, "<target> <ts> <emoji>") {
				t.Fatalf("stderr does not show the order with the target first: %q", stderr)
			}
		})
	}
}

// An emoji name that only contains a dash is still an emoji.
func TestAnEmojiWithADashIsNotAFlag(t *testing.T) {
	isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := tap(t, s, "--dry-run", "#eng", "1785712596.256909", "white-check-mark")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, ":white-check-mark:") {
		t.Fatalf("stdout = %q, want the emoji shown", stdout)
	}
}

// After "--" every argument is an operand, as it is in sez.
func TestDoubleDashMakesAnOptionWordAnOperand(t *testing.T) {
	isolate(t)
	warmCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := tap(t, s, "--dry-run", "--", "#eng", "1785712596.256909", "-n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, ":-n:") {
		t.Fatalf("stdout = %q, want the emoji -n shown", stdout)
	}
}
