package main

import (
	"fmt"
	"testing"
)

// notCanvasError stands in for fmt.Errorf("%s: %s", title, reason); the text a
// user sees must not change.
func TestNotCanvasErrorKeepsTheOldText(t *testing.T) {
	t.Parallel()
	got := notCanvasError{title: "Plan", reason: "a PDF, not a canvas"}.Error()
	if want := fmt.Sprintf("%s: %s", "Plan", "a PDF, not a canvas"); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// The --max-size and --since refusals are built around the errMaxSize and
// errSince sentinels; their full text must read as it did before the sentinels.
func TestFlagRefusalsKeepTheOldText(t *testing.T) {
	t.Parallel()
	_, sizeErr := parseSize("big")
	_, sinceErr := parseSince("soon")
	for _, c := range []struct {
		err  error
		want string
	}{
		{sizeErr, "--max-size big: not a size — try 512K, 100M or 2G"},
		{sinceErr, "--since soon: not a duration — try 30m, 2h, 7d or 1w"},
	} {
		if c.err == nil || c.err.Error() != c.want {
			t.Errorf("error = %v, want %q", c.err, c.want)
		}
	}
}
