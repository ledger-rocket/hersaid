package slackread

import (
	"context"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// search.messages already returns a permalink for every match. It is the one
// field that makes a hit addressable — `hear read <permalink>` resolves it —
// and obtaining it any other way costs a chat.getPermalink call per hit against
// the WRITE credential, which the read path does not and must not hold.
func TestSearchCarriesThePermalink(t *testing.T) {
	s := slacktest.New(t)
	const link = "https://example.slack.test/archives/C1/p1782660796826639"
	s.Reply("search.messages",
		`{"ok":true,"messages":{"matches":[{"ts":"1782660796.826639","text":"found",`+
			`"username":"alice","channel":{"id":"C1","name":"eng"},`+
			`"permalink":"`+link+`"}]}}`)

	got, err := client(t, s).Search(context.Background(), "deploy failed", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("matches = %d, want 1", len(got))
	}
	if got[0].Permalink != link {
		t.Fatalf("Permalink = %q, want %q", got[0].Permalink, link)
	}
}

// A match Slack sends without a permalink must decode to an empty string rather
// than failing the whole search: the field is a convenience, not a contract.
func TestSearchToleratesAMatchWithNoPermalink(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages",
		`{"ok":true,"messages":{"matches":[{"ts":"1.1","text":"found",`+
			`"username":"alice","channel":{"id":"C1","name":"eng"}}]}}`)

	got, err := client(t, s).Search(context.Background(), "q", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Permalink != "" {
		t.Fatalf("Permalink = %q, want empty", got[0].Permalink)
	}
}
