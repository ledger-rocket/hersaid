package cred

import (
	"strings"
	"testing"
)

// The key identifies which credential a cached fact came from. It must not be
// the token, and must not collide the way the display fingerprint does: two
// user tokens share a prefix and a length, so the fingerprint cannot tell them
// apart, and a cached identity bound to it would follow the wrong account.
func TestKeySeparatesTokensThatTheFingerprintCannot(t *testing.T) {
	t.Parallel()
	a := Read{token: "xoxp-1111111111-1111111111-1111111111-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b := Read{token: "xoxp-2222222222-2222222222-2222222222-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}

	if a.Fingerprint() != b.Fingerprint() {
		t.Skip("fingerprints already differ; this test guards the case where they do not")
	}
	if a.Key() == b.Key() {
		t.Fatalf("Key() = %q for both tokens, want distinct keys", a.Key())
	}
}

// A key is written to disk, so it must not carry the secret it identifies.
func TestKeyDoesNotContainTheToken(t *testing.T) {
	t.Parallel()
	const token = "xoxp-secret-material-9876543210"
	k := Read{token: token}.Key()

	if strings.Contains(k, token) || strings.Contains(k, "secret-material") {
		t.Fatalf("Key() = %q, want nothing of the token in it", k)
	}
	if k == "" {
		t.Fatal("Key() is empty, want a stable digest")
	}
	if k != (Read{token: token}).Key() {
		t.Fatal("Key() is not stable for the same token")
	}
}
