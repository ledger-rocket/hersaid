package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestStorePrefersTheKeyring(t *testing.T) {
	isolate(t)

	where, shadowed, err := store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(shadowed) != 0 {
		t.Errorf("reported %v as outranked where nothing else exists", shadowed)
	}
	if !strings.Contains(where, "hersaid-read") {
		t.Errorf("reported %q, want the keychain service", where)
	}
	got, err := keyring.Get("hersaid-read", "tester")
	if err != nil {
		t.Fatalf("not in the keyring: %v", err)
	}
	if got != readToken {
		t.Errorf("keyring holds a different token")
	}
	_, err = os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token"))
	if err == nil {
		t.Error("a file was written as well as the keyring; a credential lives in one place")
	}
}

// A host with no Secret Service — every CI runner, most containers, an SSH
// session — falls through to the file. Any keyring error counts, not only "not
// found": go-keyring returns the raw D-Bus or exec failure there, unwrapped.
func TestStoreFallsBackToAFileWhenThereIsNoKeyring(t *testing.T) {
	isolate(t)
	noKeyring(t)

	where, _, err := store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token")
	if !strings.Contains(where, path) {
		t.Errorf("reported %q, want the path it wrote", where)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != readToken {
		t.Error("the file holds a different token")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
	fi, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", got)
	}
}

// Replacing a token never inherits the mode of the file it replaced. Writing in
// place would: os.WriteFile applies its own mode only when it creates the file,
// so a token going into a once-0644 file would be world-readable on disk for as
// long as it took to chmod it afterwards.
func TestStoreDoesNotInheritTheModeOfTheFileItReplaces(t *testing.T) {
	isolate(t)
	noKeyring(t)

	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token")
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(path, []byte("old"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
}

// A host that gained a keyring after an earlier file enrolment: the new token
// goes to the keyring, cred reads the keyring first, and the old file is now
// unreachable — but it is still a working credential on disk, so it is named.
func TestStoreNamesATokenFileTheKeyringNowShadows(t *testing.T) {
	isolate(t)
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token")
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(path, []byte("xoxp-an-older-enrolment-0000"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, shadowed, err := store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(shadowed) != 1 || shadowed[0] != path {
		t.Fatalf("outranked = %v, want [%s]", shadowed, path)
	}
	// Named, not deleted: removing a file nobody asked about is the worse
	// surprise, and the old token may be the one somebody still wants to revoke
	// deliberately.
	_, err = os.Stat(path)
	if err != nil {
		t.Errorf("the file was removed: %v", err)
	}
}

// MkdirAll applies its mode only to directories it creates, so the chmod is
// unconditional. Dropping it would leave a directory somebody made
// group-readable years ago exactly as it
// was, with a fresh token inside it.
func TestStoreNarrowsAConfigDirectoryThatWasAlreadyTooOpen(t *testing.T) {
	isolate(t)
	noKeyring(t)
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid")
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chmod(dir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %04o, want 0700", got)
	}
}

// The write is atomic, so a process that dies partway through leaves the
// previous token intact rather than half of the new one, and no temp file
// behind.
func TestStoreLeavesNoTemporaryFileBehind(t *testing.T) {
	isolate(t)
	noKeyring(t)

	_, _, err := store(readRole, readToken)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "read.token" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want read.token alone", names)
	}
}

// A keyring that declines one write is not a keyring that is absent. cred reads
// every keyring tier before any file, so a file written here would never be
// seen — and an enrolment that reports success while the commands keep using
// the old token is worse than one that refuses.
func TestStoreRefusesTheFileFallbackWhileTheKeyringStillHoldsACredential(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
	}{
		{name: "the current service", service: "hersaid-read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			err := keyring.Set(tc.service, "tester", "xoxp-an-older-enrolment-0000")
			if err != nil {
				t.Fatal(err)
			}
			// Reads keep working; only the write fails.
			prev := keyringSet
			keyringSet = func(string, string, string) error { return errors.New("keychain denied the write") }
			t.Cleanup(func() { keyringSet = prev })

			_, _, err = store(readRole, readToken)
			if err == nil {
				t.Fatal("stored a token the commands would never read")
			}
			if !strings.Contains(err.Error(), tc.service) {
				t.Errorf("error %q does not name the item in the way", err)
			}
			_, err = os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid", "read.token"))
			if err == nil {
				t.Error("a file was written that nothing would read")
			}
		})
	}
}

func TestLookupFindsWhatStoreWrote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyring bool
	}{
		{name: "in the keyring", keyring: true},
		{name: "in a file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if !tc.keyring {
				noKeyring(t)
			}
			if _, found := lookup(readRole); found {
				t.Fatal("found a credential before one was stored")
			}
			_, _, err := store(readRole, readToken)
			if err != nil {
				t.Fatal(err)
			}
			got, found := lookup(readRole)
			if !found {
				t.Fatal("did not find the credential it had just written")
			}
			if got.described != describe(readToken) {
				t.Errorf("described it as %q", got.described)
			}
			// The write role is a different credential; finding one must not
			// answer for the other.
			if _, found := lookup(writeRole); found {
				t.Error("a read credential was reported as a write credential")
			}
		})
	}
}

func TestDescribeDoesNotDiscloseTheToken(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{token: readToken, want: "xoxp-… (47 chars)"},
		{token: botToken, want: "xoxb-… (46 chars)"},
		// Not a Slack token at all: the leading bytes of some other secret are
		// as sensitive as the rest, so only its length is reported.
		{token: "hunter2-and-then-some", want: "(21 chars)"},
	}
	for _, tc := range tests {
		got := describe(tc.token)
		if got != tc.want {
			t.Errorf("describe(%d chars) = %q, want %q", len(tc.token), got, tc.want)
		}
		if strings.Contains(got, tc.token) {
			t.Errorf("describe echoed the token: %q", got)
		}
	}
}

// The account key is $USER, which is what cred looks under. A different key
// would store a credential nothing can find.
func TestKeyringAccountIsTheUserName(t *testing.T) {
	t.Setenv("USER", "someone")
	if got := keyringAccount(); got != "someone" {
		t.Fatalf("keyringAccount() = %q, want %q", got, "someone")
	}
}

func TestTokenPathHonoursXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got, err := tokenPath(configDir, writeRole)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tmp/xdg/hersaid/write.token"; got != want {
		t.Fatalf("tokenPath = %q, want %q", got, want)
	}
}

func TestTokenPathFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := tokenPath(configDir, readRole)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "hersaid", "read.token"); got != want {
		t.Fatalf("tokenPath = %q, want %q", got, want)
	}
}
