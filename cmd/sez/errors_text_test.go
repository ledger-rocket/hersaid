package main

import (
	"errors"
	"fmt"
	"testing"
)

var errGetwdCause = errors.New("getwd: no such file or directory")

// PRO-7722 replaced fmt.Errorf("%s: %v", shown, err) with localPathError; the
// text must not change, and it must not start wrapping err.
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
