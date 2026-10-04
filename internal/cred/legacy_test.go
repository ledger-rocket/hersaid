package cred

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Credentials enrolled under the name this tool had before the rename are not
// read. A fallback that outlives its last user is a second place a credential
// can come from, and a resolution order nobody can hold in their head is how
// the wrong account ends up posting.
func TestACredentialUnderTheOldNameIsNotRead(t *testing.T) {
	isolate(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	keyringWith(t, map[string]string{"slack-read": "xoxp-an-old-token-0000"})

	_, err := ResolveRead()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — the old keychain item must not resolve", err)
	}
}

// The same for the file that name wrote on a host with no keyring.
func TestAFileUnderTheOldNameIsNotRead(t *testing.T) {
	cfg := isolate(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	keyringWith(t, nil)
	dir := filepath.Join(cfg, "slackr")
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, "read.token"), []byte("xoxp-an-old-file-token"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ResolveRead()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound — the old token file must not resolve", err)
	}
}
