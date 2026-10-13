package cache

import (
	"errors"
	"testing"
)

// Identity is cached, never fetched on demand. The two lists beside it fill
// themselves from Slack when cold, but identity must not: a read's call count
// is a tested property — a transcript that fits in one page makes one call —
// and a lazily-filled identity would add a call to every read until something
// happened to warm it. The commands that already call auth.test write it
// through instead, so it costs nothing at all.
func TestIdentityRoundTripsThroughTheCache(t *testing.T) {
	isolate(t)
	want := Identity{UserID: "U1", User: "alice", Team: "Acme"}
	err := Write(IdentityFile, want)
	if err != nil {
		t.Fatal(err)
	}
	var got Identity
	err = Read(IdentityFile, &got)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

// Cold is the ordinary state until a command that authenticates has run once,
// and it is not an error condition — the caller renders without a marker.
func TestAnUnwarmedIdentityIsCold(t *testing.T) {
	isolate(t)
	var id Identity
	err := Read(IdentityFile, &id)
	if !errors.Is(err, ErrCold) {
		t.Fatalf("err = %v, want ErrCold", err)
	}
}

// Clear takes the identity with it — it is derived from the credential, and a
// stale one outliving a re-enrolment would label another account's reactions as
// the reader's own.
func TestClearRemovesTheIdentity(t *testing.T) {
	isolate(t)
	err := Write(IdentityFile, Identity{UserID: "U1"})
	if err != nil {
		t.Fatal(err)
	}
	err = Clear()
	if err != nil {
		t.Fatal(err)
	}
	var id Identity
	err = Read(IdentityFile, &id)
	if !errors.Is(err, ErrCold) {
		t.Fatalf("err = %v, want ErrCold after Clear", err)
	}
}
