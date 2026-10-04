// Package cred resolves the two Slack credentials, one per role.
//
// Read and Write are distinct types with an unexported token field, so no other
// package can mint a credential of either role from a string it chose, and
// slackwrite.New accepts only a Write. That prevents accidental coupling; it is
// not proof of capability separation — the real separation is Slack-side, where
// the read app's token carries no chat:write scope.
package cred

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/zalando/go-keyring"
)

// ErrNotFound is returned when no source holds credential material for a role.
// For the write role it is also the gate: resolution makes no network call, so
// a command that gets ErrNotFound has issued no HTTP request at all.
var ErrNotFound = errors.New("no credential found")

// A role names one credential's sources. The fields are constants, never read
// from the environment: an override would let a read command be pointed at the
// write credential, so it would run holding a token that can post.
type role struct {
	name string
	// envVar is tested for presence, not for a non-empty value.
	envVar string
	// service is the keyring service name.
	service  string
	fileName string
}

var (
	readRole = role{
		name:     "read",
		envVar:   "SLACK_READ_TOKEN",
		service:  "hersaid-read",
		fileName: "read.token",
	}
	writeRole = role{
		name:     "write",
		envVar:   WriteEnvVar,
		service:  "hersaid-write",
		fileName: "write.token",
	}
)

// configDir is the directory under XDG_CONFIG_HOME that holds the
// file fallback.
const configDir = "hersaid"

// WriteEnvVar is the variable that overrides the stored write credential.
// Exported because a caller that must withhold the write credential from a
// child process needs to name it, and a second literal elsewhere would drift
// from this one silently.
const WriteEnvVar = "SLACK_WRITE_TOKEN"

// Read authorises read-only Slack API calls.
type Read struct{ token string }

// Write authorises Slack API calls that act as the user.
type Write struct{ token string }

// Authorize sets the bearer header. The token has no accessor: it is never
// returned as a string, so it cannot be logged or passed to another package by
// accident.
func (c Read) Authorize(req *http.Request) { authorize(req, c.token) }

// Authorize sets the bearer header.
func (c Write) Authorize(req *http.Request) { authorize(req, c.token) }

// Fingerprint identifies a token in diagnostics without disclosing it.
func (c Read) Fingerprint() string { return fingerprint(c.token) }

// Key identifies which credential a cached fact was derived from, so a fact
// outliving a token switch can be ignored rather than trusted. It is a digest,
// not the display fingerprint: two user tokens share a prefix and a length, so
// the fingerprint cannot tell them apart, and a cached identity bound to it
// would follow the wrong account. Safe to store — a hash of high-entropy
// material discloses nothing about it.
func (c Read) Key() string { return KeyFor(c.token) }

// KeyFor is Key for a token not yet established to be a credential, which is
// the state enrolment is in when it needs one.
func KeyFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// Fingerprint identifies a token in diagnostics without disclosing it.
func (c Write) Fingerprint() string { return fingerprint(c.token) }

// IsZero reports that this credential holds no material. Read{} is
// constructible in any package, and ResolveRead returns one alongside its
// error, so an endpoint tests this before building a request rather than
// trusting every caller's control flow to have handled that error.
func (c Read) IsZero() bool { return c.token == "" }

// IsZero reports that this credential holds no material.
func (c Write) IsZero() bool { return c.token == "" }

func authorize(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
}

// tokenPrefix is the shape of a Slack token: xoxp-, xoxb-, xoxe- and so on.
var tokenPrefix = regexp.MustCompile(`^xox[a-z]-`)

// fingerprint discloses the leading bytes only when they are a Slack token
// type, which is public knowledge. Anything else in this position is a secret
// of some other kind — the wrong password, an API key — whose first characters
// are as sensitive as the rest, so only its length is reported.
func fingerprint(token string) string {
	if p := tokenPrefix.FindString(token); p != "" {
		return fmt.Sprintf("%s… (%d chars)", p, len(token))
	}
	return fmt.Sprintf("(%d chars)", len(token))
}

// ResolveRead returns the read credential, or ErrNotFound.
func ResolveRead() (Read, error) {
	t, err := resolve(readRole)
	return Read{token: t}, err
}

// ResolveWrite returns the write credential, or ErrNotFound. It is the gate,
// and the gate is a single call: a separate HasWrite predicate would mean a
// second keyring read, which on a locked keychain is a second prompt within one
// invocation. Material existing does not mean the token is valid or carries
// chat:write — only Slack can decide that.
func ResolveWrite() (Write, error) {
	t, err := resolve(writeRole)
	return Write{token: t}, err
}

// keyringGet is a variable so tests can simulate a machine with no keyring
// without touching the developer's real one.
var keyringGet = keyring.Get

// resolve walks the sources, first hit wins:
//
//  1. the role's env var
//  2. keyring service hersaid-<role>
//  3. ~/.config/hersaid/<role>.token
//
// The keyring is consulted before the file: a keychain is preferred over a file
// on disk, and the file exists for hosts that have no keyring at all.
func resolve(r role) (string, error) {
	// Presence, not non-emptiness: an explicitly exported SLACK_*_TOKEN="" means
	// "no credential" and stops here rather than falling through to the keyring.
	// Falling through would make the obvious way to rehearse the closed gate —
	// blanking the variable — silently send instead.
	if v, ok := os.LookupEnv(r.envVar); ok {
		return finish(strip(v), r)
	}
	// Any keyring error falls through to the next source, not only ErrNotFound:
	// on a headless Linux host go-keyring returns the raw D-Bus or exec failure
	// unwrapped, so a caller cannot tell it apart from a genuine miss, and
	// matching on ErrNotFound alone would strand every such user.
	acct := account()
	v, err := keyringGet(r.service, acct)
	if err == nil {
		if t := strip(v); t != "" {
			return t, nil
		}
	}
	b, err := os.ReadFile(tokenPath(configDir, r))
	if err == nil {
		if t := strip(string(b)); t != "" {
			return t, nil
		}
	}
	return "", fmt.Errorf("%w for role %s", ErrNotFound, r.name)
}

func finish(token string, r role) (string, error) {
	if token == "" {
		return "", fmt.Errorf("%w for role %s", ErrNotFound, r.name)
	}
	return token, nil
}

// strip removes every whitespace character — a token contains none, and a
// trailing newline from a
// heredoc or an editor is the common way one arrives with some. TrimSpace would
// leave an embedded newline in place, which is a silent behaviour change.
func strip(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func tokenPath(dir string, r role) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, dir, r.fileName)
}

// account is the keyring account key. $USER first, with a fallback: it is what
// hersaid-setup stores under, and the two have to agree or a stored credential
// is one nothing can find.
func account() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	u, err := user.Current()
	if err == nil {
		return u.Username
	}
	return ""
}
