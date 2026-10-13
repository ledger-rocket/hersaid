package main

import (
	"errors"
	"fmt"
	"testing"
)

var errGetwdCause = errors.New("getwd: no such file or directory")

// localPathError stands in for fmt.Errorf("%s: %v", shown, err); the text must
// not change, and it must not start wrapping err.
func TestLocalPathErrorKeepsTheOldText(t *testing.T) {
	t.Parallel()
	e := localPathError{shown: "notes.txt", err: errGetwdCause}
	if want := fmt.Sprintf("%s: %v", "notes.txt", errGetwdCause); e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
	if errors.Is(e, errGetwdCause) {
		t.Fatal("localPathError wraps its cause; the old message did not")
	}
}
