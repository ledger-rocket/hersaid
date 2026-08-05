package slacktest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var nonLetters = regexp.MustCompile(`[^a-z]`)

// Comments describe the code as it is. A reference to what some earlier
// implementation did is archaeology a reader cannot check — the files are not
// here — and it ages into a claim about the present that was never true of it.
// git holds the history; the source holds the invariants.
//
// The constraint a comment names is welcome. Its provenance is not.
func TestNoCommentCitesAnEarlierImplementation(t *testing.T) {
	banned := []string{
		"zshversion", "zshone", "zshtools", "zshinstaller", "zshera",
		"shellversion", "shellinstaller", "shellformat", "shellshape",
		"shellwritten", "shellgrepped", "shellchmod",
		"binslackpost", "binslackreact", "binslackr", "slackrchannels",
	}
	root := moduleRoot(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// This file names them in order to forbid them.
		if strings.HasSuffix(path, "history_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Squashed to letters before matching: a wrapped comment splits a
		// phrase across "\n// ", and a test name spells it as one word, so a
		// plain substring search is blind to exactly what this looks for.
		squashed := nonLetters.ReplaceAllString(strings.ToLower(string(src)), "")
		for _, phrase := range banned {
			if strings.Contains(squashed, phrase) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s mentions %q; say what the code does now, not what something else did", rel, phrase)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
