package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slackread"
)

func reacted(count int, users ...string) []slackread.Message {
	return []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{Name: "tada", Count: count, Users: users}},
	}}
}

var five = Names{"U1": "A", "U2": "B", "U3": "C", "U4": "D", "SELF": "Stepan"}

// The reader is named "you" and placed first. Slack does not order the users
// array, so past three reactors the reader's own name would otherwise fall into
// the overflow — and the reactor names exist precisely to answer "is this mine
// to remove". A fixed position is also easier to scan for than a name.
func TestTheReaderIsNamedYouAndComesFirst(t *testing.T) {
	var b bytes.Buffer
	o := opts()
	o.SelfID = "SELF"

	if err := WriteTranscript(&b, reacted(5, "U1", "U2", "U3", "U4", "SELF"), five, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 5 (you, A, B +2)") {
		t.Fatalf("transcript = %q, want the reader promoted out of the overflow", b.String())
	}
}

// Promotion must not cost a slot: three names are still three names, and the
// overflow still reconciles with the count.
func TestPromotingTheReaderKeepsTheOverflowHonest(t *testing.T) {
	var b bytes.Buffer
	o := opts()
	o.SelfID = "SELF"

	if err := WriteTranscript(&b, reacted(50, "U1", "U2", "U3", "U4", "SELF"), five, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 50 (you, A, B +47)") {
		t.Fatalf("transcript = %q, want three names and +47", b.String())
	}
}

// Already first, and it stays one entry rather than being listed twice.
func TestTheReaderIsNotDuplicatedWhenAlreadyListed(t *testing.T) {
	var b bytes.Buffer
	o := opts()
	o.SelfID = "SELF"

	if err := WriteTranscript(&b, reacted(3, "SELF", "U1", "U2"), five, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 3 (you, A, B)") {
		t.Fatalf("transcript = %q, want the reader listed once", b.String())
	}
	if strings.Contains(b.String(), "Stepan") {
		t.Fatalf("transcript = %q, want the reader as 'you' rather than by name", b.String())
	}
}

// A reaction the reader has not given reads exactly as it did before.
func TestAReactionTheReaderHasNotGivenIsUnchanged(t *testing.T) {
	var b bytes.Buffer
	o := opts()
	o.SelfID = "SELF"

	if err := WriteTranscript(&b, reacted(2, "U1", "U2"), five, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 2 (A, B)") {
		t.Fatalf("transcript = %q, want no reader marker", b.String())
	}
}

// The identity is best-effort: a cold cache, a refused auth.test or an offline
// store all leave it empty, and the transcript must render exactly as it does
// today rather than degrading or failing.
func TestWithoutAnIdentityTheLineIsUnchanged(t *testing.T) {
	var b bytes.Buffer

	if err := WriteTranscript(&b, reacted(5, "U1", "U2", "U3", "U4", "SELF"), five, opts()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 5 (A, B, C +2)") {
		t.Fatalf("transcript = %q, want the unmarked line", b.String())
	}
	if strings.Contains(b.String(), "you") {
		t.Fatalf("transcript = %q, want no reader marker without an identity", b.String())
	}
}
