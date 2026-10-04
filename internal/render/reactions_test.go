package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// Emoji are named, not rendered as unicode. A mapping table would cover the
// standard set and silently fail on the custom workspace emoji a team actually
// uses, which have no unicode form at all.
//
// The reactors are named because that is what makes a reaction actionable: it
// is how a reader sees whether one is theirs to `tap --remove`. The names come
// from the user list already loaded for author and mention resolution, so this
// costs no lookup of its own.
func TestReactionsAreNamedAndAttributed(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{
			{Name: "white_check_mark", Count: 2, Users: []string{"U1", "U2"}},
			{Name: "eyes", Count: 1, Users: []string{"U2"}},
		},
	}}
	names := Names{"U1": "Stepan", "U2": "Raj"}

	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	want := "08-02 22:13  bot  topic\n" +
		strings.Repeat(" ", 20) + ":white_check_mark: 2 (Stepan, Raj)  :eyes: 1 (Raj)\n"
	if b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// A message with no reactions gains no line. Reactions are the common case's
// absence, and a transcript that reserved a row for them would double in height
// for nothing.
func TestNoReactionLineWithoutReactions(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{TS: "1785708834.862009", Username: "bot", Text: "topic"}}

	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "08-02 22:13  bot  topic\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

// Reactions sit between the message and its thread, as they do in Slack: they
// belong to the message that was reacted to, where the reply hint points away
// from it.
func TestReactionsPrecedeTheReplyHint(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic", ReplyCount: 3,
		Reactions: []slackread.Reaction{{Name: "eyes", Count: 1, Users: []string{"U1"}}},
	}}
	o := opts()
	o.ThreadCommand = threadCmd

	err := WriteTranscript(&b, msgs, Names{"U1": "Stepan"}, o)
	if err != nil {
		t.Fatal(err)
	}
	reactions := strings.Index(b.String(), ":eyes:")
	replies := strings.Index(b.String(), "⤷")
	if reactions < 0 || replies < 0 {
		t.Fatalf("transcript = %q, want both a reaction line and a reply hint", b.String())
	}
	if reactions > replies {
		t.Fatalf("transcript = %q, want reactions before the reply hint", b.String())
	}
}

// A long reactor list is truncated rather than allowed to run off the line, so
// that names shown plus overflow always reconcile with the count beside them.
func TestALongReactorListIsTruncated(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{
			Name: "tada", Count: 5,
			Users: []string{"U1", "U2", "U3", "U4", "U5"},
		}},
	}}
	names := Names{"U1": "A", "U2": "B", "U3": "C", "U4": "D", "U5": "E"}

	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 5 (A, B, C +2)") {
		t.Fatalf("transcript = %q, want three names and a +2 overflow", b.String())
	}
}

// An id the user list does not cover keeps its id, exactly as an unresolved
// mention does. Dropping it would misreport who reacted; the count would still
// say two while only one name was shown.
func TestAnUnknownReactorKeepsItsID(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{
			{Name: "eyes", Count: 2, Users: []string{"U1", "UNKNOWN"}},
		},
	}}

	err := WriteTranscript(&b, msgs, Names{"U1": "Stepan"}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":eyes: 2 (Stepan, UNKNOWN)") {
		t.Fatalf("transcript = %q, want the unresolved id kept", b.String())
	}
}

// The structured form carries raw user ids, not display names, for the same
// reason the message carries both `user` and `author`: JSON is the wire form,
// and a consumer that has ids can resolve them while one given only names
// cannot go back.
func TestStructuredReactionsCarryUserIDs(t *testing.T) {
	msgs := Messages([]slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{
			{Name: "white_check_mark", Count: 2, Users: []string{"U1", "U2"}},
		},
	}}, Names{"U1": "Stepan", "U2": "Raj"})

	if len(msgs) != 1 || len(msgs[0].Reactions) != 1 {
		t.Fatalf("messages = %+v, want one message with one reaction", msgs)
	}
	r := msgs[0].Reactions[0]
	if r.Name != "white_check_mark" || r.Count != 2 {
		t.Errorf("reaction = %+v, want white_check_mark x2", r)
	}
	if len(r.Users) != 2 || r.Users[0] != "U1" || r.Users[1] != "U2" {
		t.Errorf("users = %v, want the raw ids [U1 U2]", r.Users)
	}
}

// Absent rather than empty, matching how a search hit omits a permalink it does
// not have: a field present as [] on every message is one every consumer has to
// test for emptiness.
func TestAMessageWithNoReactionsOmitsTheField(t *testing.T) {
	msgs := Messages([]slackread.Message{
		{TS: "1785708834.862009", Username: "bot", Text: "topic"},
	}, Names{})

	b, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reactions") {
		t.Fatalf("marshalled message = %s, want no reactions field", b)
	}
}

// Slack truncates the users array on a heavily-reacted message: it reports a
// count of 50 and returns a handful of ids. The overflow must therefore count
// from the count, not from the ids that came back, or the line contradicts
// itself — three names and a +2 under a total of 50.
func TestTheOverflowCountsFromTheTotalNotTheReturnedIDs(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{
			Name: "tada", Count: 50,
			Users: []string{"U1", "U2", "U3", "U4", "U5"},
		}},
	}}
	names := Names{"U1": "A", "U2": "B", "U3": "C", "U4": "D", "U5": "E"}

	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ":tada: 50 (A, B, C +47)") {
		t.Fatalf("transcript = %q, want the overflow to reconcile with the count", b.String())
	}
}

// Slack can report a count with no ids at all. Rendering the parentheses
// regardless leaves a dangling "()" that says nothing.
func TestAReactionWithNoNamedReactorsHasNoParentheses(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{Name: "tada", Count: 4}},
	}}

	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "()") {
		t.Fatalf("transcript = %q, want no empty parentheses", b.String())
	}
	if !strings.Contains(b.String(), ":tada: 4") {
		t.Fatalf("transcript = %q, want the reaction still counted", b.String())
	}
}

// Empty, not nil — the rule the rest of the structured output already follows,
// because a null in place of [] breaks every `jq '.reactions[].users[]'`. Slack
// omits the users array on some reactions, so this is the shape a consumer
// actually meets rather than a hypothetical one.
func TestAReactionWithNoReactorsCarriesAnEmptyUserList(t *testing.T) {
	msgs := Messages([]slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{Name: "tada", Count: 4}},
	}}, Names{})

	b, err := json.Marshal(msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"users":[]`) {
		t.Fatalf("marshalled message = %s, want an empty list rather than null", b)
	}
}

// The render struct must not alias the slackread slice it was built from: a
// consumer mutating one would otherwise reach through into the other.
func TestStructuredReactionsDoNotAliasTheSource(t *testing.T) {
	src := []slackread.Message{{
		TS: "1785708834.862009", Username: "bot", Text: "topic",
		Reactions: []slackread.Reaction{{Name: "eyes", Count: 1, Users: []string{"U1"}}},
	}}
	msgs := Messages(src, Names{})
	msgs[0].Reactions[0].Users[0] = "MUTATED"

	if src[0].Reactions[0].Users[0] != "U1" {
		t.Fatalf("source users = %v, want the render struct to hold its own copy", src[0].Reactions[0].Users)
	}
}
