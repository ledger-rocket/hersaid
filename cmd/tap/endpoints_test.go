package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tap holds the write credential, so it cannot be confined the way hear is —
// the question is not whether it can reach Slack but which verbs it can reach.
// Reacting is the whole of its job; posting, editing and deleting are not, and
// the credential would carry them just as happily.
func TestBuiltBinaryReachesNoMessageEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "tap")
	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	forbidden := []string{"chat.postMessage", "chat.update", "chat.delete", "files.upload"}

	nm, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm: %v\n%s", err, nm)
	}
	for _, f := range forbidden {
		if strings.Contains(string(nm), f) {
			t.Errorf("symbol table names %q", f)
		}
	}

	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range forbidden {
		if bytes.Contains(raw, []byte(f)) {
			t.Errorf("binary contains the string %q", f)
		}
	}

	// The linker drops the string constant of any endpoint the binary never
	// calls, so absence on its own is weak evidence. reactions.add is tap's
	// reason to exist, so finding it shows the scan can see an endpoint that is
	// genuinely compiled in.
	if !bytes.Contains(raw, []byte("reactions.add")) {
		t.Error("the byte scan cannot see a reachable endpoint, so it proves nothing")
	}
}
