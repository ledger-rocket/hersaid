package slackhosts

import "testing"

// The suffix match is on label boundaries, not on the characters: an attacker
// who can register "notslack.com" or "slack.com.evil.test" must not thereby be
// somewhere the bearer may be sent.
func TestPermitsMatchesOnLabelBoundaries(t *testing.T) {
	h := Allow("slack.com")
	for _, c := range []struct {
		host string
		want bool
	}{
		{"slack.com", true},
		{"files.slack.com", true},
		{"a.b.slack.com", true},
		{"notslack.com", false},
		{"slack.com.evil.test", false},
		{"xslack.com", false},
		{"", false},
	} {
		if got := h.PermitsBearer(c.host); got != c.want {
			t.Errorf("PermitsBearer(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// The redirect tier is additive and one-way. A host reachable only by redirect
// is never a host the bearer may be sent to, which is the property the whole
// split exists to provide.
func TestTheRedirectTierNeverWidensWhereTheBearerMayGo(t *testing.T) {
	h := Allow("slack.com").AndViaRedirect("storage.test")
	if h.PermitsBearer("storage.test") {
		t.Error("PermitsBearer(storage.test) = true; a redirect-only host may not hold the bearer")
	}
	if !h.PermitsRedirect("storage.test") {
		t.Error("PermitsRedirect(storage.test) = false; the redirect tier must permit it")
	}
	if !h.PermitsRedirect("slack.com") {
		t.Error("PermitsRedirect(slack.com) = false; everywhere the bearer may go, a redirect may go")
	}
	if h.PermitsRedirect("elsewhere.test") {
		t.Error("PermitsRedirect(elsewhere.test) = true; an unlisted host is permitted by neither tier")
	}
}

// AndViaRedirect returns a value rather than mutating the receiver: a Hosts
// handed to two clients must not gain a host because one of them extended it.
func TestAndViaRedirectDoesNotMutateTheReceiver(t *testing.T) {
	base := Allow("slack.com")
	_ = base.AndViaRedirect("storage.test")
	if base.PermitsRedirect("storage.test") {
		t.Error("the receiver gained a redirect host; AndViaRedirect must copy")
	}
}

// The zero value permits nothing. A Hosts is constructible in any package, so a
// client built from one nobody filled in must refuse rather than allow.
func TestTheZeroValuePermitsNothing(t *testing.T) {
	var h Hosts
	if h.PermitsBearer("slack.com") || h.PermitsRedirect("slack.com") {
		t.Error("the zero Hosts permitted a host; it must permit none")
	}
}
