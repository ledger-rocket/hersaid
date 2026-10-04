package slackwrite

import (
	"context"
	"reflect"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// Same discipline as PostMessage: every argument carries a parameter separator,
// and none of them may split into a field Slack honours.
func TestUpdateSendsExactlyTheDeclaredFields(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1.1","message":{"text":"stored"}}`)

	_, err := client(t, s).Update(context.Background(),
		"C0123ABCD&blocks=[{\"type\":\"section\"}]",
		"1785712599.000100&channel=C9999EVIL",
		"new&blocks=[{}]&channel=C9999EVIL")
	if err != nil {
		t.Fatal(err)
	}

	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "text", "ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
	if got := req.Body.Get("text"); got != "new&blocks=[{}]&channel=C9999EVIL" {
		t.Fatalf("text = %q; the separators must not have split it", got)
	}
	if got := req.Body.Get("ts"); got != "1785712599.000100&channel=C9999EVIL" {
		t.Fatalf("ts = %q; the separators must not have split it", got)
	}
	if len(req.Query) != 0 {
		t.Fatalf("query string = %v, want empty", req.Query)
	}
}

// What Slack stored, not what was submitted — it sanitises and truncates, and
// an edit is worth auditing against the same difference a send is.
func TestUpdateReturnsWhatSlackStored(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1785712599.000100","message":{"text":"after &amp; then"}}`)

	got, err := client(t, s).Update(context.Background(), "C1", "1785712599.000100", "after & then")
	if err != nil {
		t.Fatal(err)
	}
	if got.TS != "1785712599.000100" {
		t.Fatalf("TS = %q", got.TS)
	}
	if got.Text != "after &amp; then" {
		t.Fatalf("Text = %q, want what Slack stored", got.Text)
	}
}

func TestDeleteSendsOnlyTheAddress(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true,"ts":"1.1"}`)

	err := client(t, s).Delete(context.Background(), "C1", "1785712599.000100")
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v — a delete carries no text", got, want)
	}
}

// message_not_found is NOT treated as an idempotent success, unlike no_reaction
// on reactions.remove. There it is unambiguous: the reaction is not there and
// was not wanted. Here it reads the same for a message already deleted and for
// a mistyped timestamp naming a message that still exists — and reporting a
// typo as a successful deletion is the worse failure of the two.
func TestADeleteThatFoundNothingIsNotSuccess(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":false,"error":"message_not_found"}`)

	err := client(t, s).Delete(context.Background(), "C1", "1785712599.000100")
	if err == nil {
		t.Fatal("err = nil, want a refusal")
	}
	if got := Classify(err); got != Rejected {
		t.Fatalf("Classify = %v, want Rejected", got)
	}
}
