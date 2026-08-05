// Package allowlist restricts which conversations a write command may touch.
//
// It is stronger than the credential gate, which is binary: a fully authorised
// write token still cannot reach a conversation outside the list. The check
// belongs after resolution — a #name and its C… id are the same conversation —
// and before any request, so a refusal costs no network call.
package allowlist

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/resolve"
)

// EnvVar holds a comma-separated list of ids and/or #names.
const EnvVar = "HERSAID_WRITE_CHANNELS"

// ErrRefused reports a target outside the allowlist.
var ErrRefused = errors.New("not in " + EnvVar)

var idPattern = regexp.MustCompile(`^[CDGU][A-Z0-9]{7,}$`)

// List is a set of permitted write targets.
//
// The zero List permits everything: the allowlist is opt-in, so an unset
// variable must not lock out a user who never asked for it. Once set, matching
// is exact and case-sensitive in both directions, so a near-miss is refused
// rather than approximated.
type List struct {
	active bool
	ids    map[string]bool
	names  map[string]bool
	raw    string
}

// FromEnv reads the allowlist from HERSAID_WRITE_CHANNELS. An unset or empty
// variable yields an inactive List.
func FromEnv() List { return Parse(os.Getenv(EnvVar)) }

// Parse builds a List from a comma-separated string. Entries matching a Slack
// id are matched against the resolved id; anything else is a channel name, with
// an optional leading "#".
func Parse(s string) List {
	l := List{ids: map[string]bool{}, names: map[string]bool{}, raw: s}
	for _, part := range strings.Split(s, ",") {
		e := strings.TrimSpace(part)
		if e == "" {
			continue
		}
		l.active = true
		if idPattern.MatchString(e) {
			l.ids[e] = true
			continue
		}
		l.names[strings.TrimPrefix(e, "#")] = true
	}
	return l
}

// Active reports whether any restriction applies.
func (l List) Active() bool { return l.active }

// String is the list as configured, for a refusal message. It names channels,
// never credentials, so it is safe to print.
func (l List) String() string { return l.raw }

// Permits reports whether t may be written to.
//
// A target named by id matches only an id entry: this package does not know
// that C0123ABCD is #eng, so allowlisting the name and then passing the raw id
// is refused. That is the fail-closed direction.
func (l List) Permits(t resolve.Target) bool {
	if !l.active {
		return true
	}
	if l.ids[t.ID] {
		return true
	}
	// A person allowlisted by user id stays allowed once a DM conversation
	// exists and resolution starts yielding the D… id instead.
	if t.UserID != "" && l.ids[t.UserID] {
		return true
	}
	// Covers both a resolved #name and one passed through unresolved, whose ID
	// is still "#name".
	return t.Name != "" && l.names[t.Name]
}

// Check returns nil when t is permitted, else an error wrapping ErrRefused.
func (l List) Check(t resolve.Target) error {
	if l.Permits(t) {
		return nil
	}
	return fmt.Errorf("refusing %s: %w (%s)", t.Input, ErrRefused, l.raw)
}
