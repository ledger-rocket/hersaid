package cred

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noKeyring stands in for a machine with no keyring at all. go-keyring returns
// the raw D-Bus/exec error there, unwrapped, so a caller cannot tell it apart
// from a genuine miss with errors.Is — resolution must fall through on any
// error, and this asserts it does.
func noKeyring(t *testing.T) {
	t.Helper()
	swap(t, func(service, account string) (string, error) {
		return "", errors.New(`exec: "dbus-launch": executable file not found in $PATH`)
	})
}

// keyringWith serves only the named services; everything else fails the way a
// headless host does.
func keyringWith(t *testing.T, items map[string]string) {
	t.Helper()
	swap(t, func(service, account string) (string, error) {
		if v, ok := items[service]; ok {
			return v, nil
		}
		return "", errors.New(`exec: "dbus-launch": executable file not found in $PATH`)
	})
}

func swap(t *testing.T, f func(string, string) (string, error)) {
	t.Helper()
	prev := keyringGet
	keyringGet = f
	t.Cleanup(func() { keyringGet = prev })
}

// isolate points the file source at an empty temp dir so a real
// ~/.config/hersaid/*.token cannot influence a test.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

// unsetEnv removes a variable for the duration of the test. t.Setenv is called
// first purely for its cleanup, which restores the developer's real value.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func writeToken(t *testing.T, cfgDir, dir, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfgDir, dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An explicitly empty credential env var means absent, so the
// obvious way to rehearse the closed gate — blanking the variable — cannot
// silently fall through to a keyring item and send.
func TestEmptyEnvVarMeansAbsentAndStopsResolution(t *testing.T) {
	cfg := isolate(t)
	writeToken(t, cfg, "hersaid", "write.token", "xoxp-from-file")
	swap(t, func(service, account string) (string, error) {
		t.Errorf("keyring consulted after an explicitly empty %s", writeRole.envVar)
		return "xoxp-from-keyring", nil
	})

	t.Setenv("SLACK_WRITE_TOKEN", "")

	if _, err := ResolveWrite(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveWrite() error = %v, want ErrNotFound", err)
	}
}

// Whitespace-only is the same as empty: it is what an accidental `export
// SLACK_WRITE_TOKEN=$'\n'` produces.
func TestWhitespaceOnlyEnvVarMeansAbsent(t *testing.T) {
	isolate(t)
	noKeyring(t)
	t.Setenv("SLACK_WRITE_TOKEN", "  \n\t ")

	if _, err := ResolveWrite(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveWrite() error = %v, want ErrNotFound", err)
	}
}

func TestUnsetEnvVarFallsThroughToKeyring(t *testing.T) {
	isolate(t)
	keyringWith(t, map[string]string{"hersaid-write": "xoxp-from-keyring\n"})
	unsetEnv(t, "SLACK_WRITE_TOKEN")

	c, err := ResolveWrite()
	if err != nil {
		t.Fatalf("ResolveWrite() error = %v", err)
	}
	if c.token != "xoxp-from-keyring" {
		t.Fatalf("token = %q; want the keyring value with whitespace stripped", c.token)
	}
}

// Whitespace is stripped everywhere, not just at the ends: TrimSpace would
// leave an embedded newline in place, and the token then fails at Slack with a
// header error rather than obviously.
func TestInteriorWhitespaceIsStripped(t *testing.T) {
	isolate(t)
	noKeyring(t)
	t.Setenv("SLACK_READ_TOKEN", " xoxp-123\n456\t789 ")

	c, err := ResolveRead()
	if err != nil {
		t.Fatalf("ResolveRead() error = %v", err)
	}
	if c.token != "xoxp-123456789" {
		t.Fatalf("token = %q, want every whitespace character removed", c.token)
	}
}

// The file fallback is a first-class path, not a degraded one: it is what every
// headless Linux host, container and CI runner will use.
func TestFileFallbackWhenKeyringUnavailable(t *testing.T) {
	cfg := isolate(t)
	noKeyring(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	writeToken(t, cfg, "hersaid", "read.token", "xoxp-from-file\n")

	c, err := ResolveRead()
	if err != nil {
		t.Fatalf("ResolveRead() error = %v", err)
	}
	if c.token != "xoxp-from-file" {
		t.Fatalf("token = %q, want xoxp-from-file", c.token)
	}
}

// Precedence: the keyring wins over the file, so re-enrolling into the keyring
// is not silently overridden by a file left behind from a host that had none.
func TestSourcePrecedence(t *testing.T) {
	cfg := isolate(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	writeToken(t, cfg, "hersaid", "read.token", "the-file")

	for _, tc := range []struct {
		name  string
		items map[string]string
		want  string
	}{
		{"keychain wins over the file", map[string]string{"hersaid-read": "the-keychain"}, "the-keychain"},
		{"the file serves when the keychain has nothing", nil, "the-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyringWith(t, tc.items)
			c, err := ResolveRead()
			if err != nil {
				t.Fatalf("ResolveRead() error = %v", err)
			}
			if c.token != tc.want {
				t.Fatalf("token = %q, want %q", c.token, tc.want)
			}
		})
	}
}

// An empty keychain item must not shadow a populated file: a failed paste
// stores nothing, and the next source is the recovery.
func TestEmptyKeychainItemFallsThrough(t *testing.T) {
	cfg := isolate(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	keyringWith(t, map[string]string{"hersaid-read": "  \n"})
	writeToken(t, cfg, "hersaid", "read.token", "xoxp-from-file")

	c, err := ResolveRead()
	if err != nil {
		t.Fatalf("ResolveRead() error = %v", err)
	}
	if c.token != "xoxp-from-file" {
		t.Fatalf("token = %q, want the file to be reached", c.token)
	}
}

func TestNoSourceIsErrNotFound(t *testing.T) {
	isolate(t)
	noKeyring(t)
	unsetEnv(t, "SLACK_READ_TOKEN")

	if _, err := ResolveRead(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveRead() error = %v, want ErrNotFound", err)
	}
}

// The selectors are constants rather than environment-overridable. A test
// cannot prove the absence of a future env lookup, but it can pin the names the
// commands and the installer agree on.
func TestSelectorsAreFixed(t *testing.T) {
	if readRole.service != "hersaid-read" || writeRole.service != "hersaid-write" {
		t.Fatalf("keyring services = %q/%q, want hersaid-read/hersaid-write",
			readRole.service, writeRole.service)
	}
	t.Setenv("USER", "someone")
	if account() != "someone" {
		t.Fatalf("account() = %q, want $USER", account())
	}
}

// The read role must never consult the write credential's sources, or a read
// command could run holding a token that can post.
func TestRolesDoNotShareSources(t *testing.T) {
	isolate(t)
	unsetEnv(t, "SLACK_READ_TOKEN")
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-write-only")
	keyringWith(t, map[string]string{"hersaid-write": "xoxp-write-keychain"})

	if _, err := ResolveRead(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveRead() error = %v; a read must not see the write credential", err)
	}
}

// Only a recognised Slack token type is echoed. Which of xoxp/xoxb a token is
// is public knowledge and worth a diagnostic; anything else in this position is
// a secret of some other kind — the wrong password, an unrelated API key —
// pasted by mistake, and its first characters are as sensitive as the rest.
func TestFingerprintDiscloseNothingButTheSlackTokenType(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"xoxp-1234567890-secret-material", "xoxp-… (31 chars)"},
		{"xoxb-1234567890-secret-material", "xoxb-… (31 chars)"},
		{"sk-live-9f2b1c-not-a-slack-token", "(32 chars)"},
		{"hunter2", "(7 chars)"},
		{"xoxp", "(4 chars)"},
		{"", "(0 chars)"},
	} {
		if got := (Read{token: tc.token}).Fingerprint(); got != tc.want {
			t.Errorf("Fingerprint(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}

	// Nothing of a mis-pasted secret survives into the diagnostic.
	const secret = "sk-live-9f2b1c-not-a-slack-token"
	fp := (Write{token: secret}).Fingerprint()
	for _, leak := range []string{"sk", "live", "9f2b1c"} {
		if strings.Contains(fp, leak) {
			t.Errorf("Fingerprint() = %q, which echoes %q", fp, leak)
		}
	}
}
