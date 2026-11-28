package main

import (
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// sez reads options only before the target, so a flag after it used to become
// message text: `sez --send --md U0A4Z4BE3N3 --thread 1795765280.039559 -`
// posted the words "--thread 1795765280.039559 -" at top level, and the body on
// stdin was never sent. Every one of sez's own options, and the near misses of
// --thread that agents wrote, is refused after the target, with no request and
// no audit record.
func TestAFlagAfterTheTargetIsRefused(t *testing.T) {
	cases := map[string][]string{
		"--thread":            {"--send", "--md", "U0A4Z4BE3N3", "--thread", "1795765280.039559", "-"},
		"--thread-id":         {"--send", "#eng", "--thread-id", "1795765280.039559", "hi"},
		"--thread-ts":         {"--send", "#eng", "--thread-ts", "1795765280.039559", "hi"},
		"--thread=1795765280": {"--send", "#eng", "--thread=1795765280", "hi"},
		"--md":                {"--send", "#eng", "hi", "--md"},
		"--send":              {"#eng", "--send", "hi"},
		"--dry-run":           {"--send", "#eng", "hi", "--dry-run"},
		"-n":                  {"--send", "#eng", "-n", "hi"},
		"--file":              {"--send", "#eng", "--file", "./out.log"},
		"--file=./out.log":    {"--send", "#eng", "--file=./out.log"},
		"--edit":              {"--send", "U0A4Z4BE3N3", "--edit", "1795765280.039559", "new text"},
		"--delete":            {"--send", "U0A4Z4BE3N3", "--delete", "1795765280.039559"},
		"--at":                {"--send", "#eng", "hi", "--at", "09:00"},
		"--in":                {"--send", "#eng", "--in", "2h", "hi"},
		"--scheduled":         {"--send", "#eng", "hi", "--scheduled"},
		"--unschedule":        {"--send", "#eng", "--unschedule", "Q1298393284"},
		"--help":              {"--send", "#eng", "hi", "--help"},
		"-h":                  {"--send", "#eng", "hi", "-h"},
		"--version":           {"--send", "#eng", "hi", "--version"},
	}
	for flag, args := range cases {
		t.Run(flag, func(t *testing.T) {
			logPath := isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			delivers(s)

			code, stdout, stderr := sezStdin(t, s, "the real body", args...)
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
			if !strings.Contains(stderr, "sez: "+flag+" comes after the target") {
				t.Fatalf("stderr does not name %s as misplaced: %q", flag, stderr)
			}
			if !strings.Contains(stderr, "<target>") {
				t.Fatalf("stderr does not show the order with the target last: %q", stderr)
			}
		})
	}
}

// Text that only contains dashes, or holds a flag inside a longer argument, is
// still text: the guard compares whole arguments to sez's own option names.
func TestTextThatMerelyContainsDashesStillSends(t *testing.T) {
	cases := map[string][]string{
		"use -- to stop options":      {"--send", "#eng", "use", "--", "to", "stop", "options"},
		"a--b --threads -nope":        {"--send", "#eng", "a--b", "--threads", "-nope"},
		"pass --thread before the id": {"--send", "#eng", "pass --thread before the id"},
		"- is a dash":                 {"--send", "#eng", "- is a dash"},
	}
	for want, args := range cases {
		t.Run(want, func(t *testing.T) {
			isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			delivers(s)

			code, _, stderr := sez(t, s, args...)
			if strings.Contains(stderr, "comes after the target") {
				t.Fatalf("legitimate text was refused as a misplaced flag: %q", stderr)
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if got := sent(t, s); got != want {
				t.Fatalf("text on the wire = %q, want %q", got, want)
			}
		})
	}
}

// After "--" every argument is the target and text, as the usage says, so a
// message that starts with a word sez knows as a flag can still be typed.
func TestDoubleDashMakesAFlagWordText(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	code, _, stderr := sez(t, s, "--send", "--", "#eng", "--md", "is", "a", "flag")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "--md is a flag" {
		t.Fatalf("text on the wire = %q, want %q", got, "--md is a flag")
	}
}

// Stdin is never parsed for options: a body that starts with a flag word sends.
func TestAFlagWordOnStdinIsText(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	code, _, stderr := sezStdin(t, s, "--thread is how to reply\n", "--send", "#eng", "-")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "--thread is how to reply" {
		t.Fatalf("text on the wire = %q, want %q", got, "--thread is how to reply")
	}
}
