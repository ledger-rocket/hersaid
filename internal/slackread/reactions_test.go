package slackread

import (
	"context"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// conversations.history already carries the reactions on every message it
// returns, under the history scopes the read credential holds. Reading them
// therefore costs no extra call and no extra scope — which is the whole reason
// reactions are read through hear rather than tap, where the same information
// would cost a reactions.get against the WRITE credential.
func TestHistoryCarriesReactions(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785804051.982839","text":"topic","user":"U1",`+
			`"reactions":[{"name":"white_check_mark","count":2,"users":["U1","U2"]},`+
			`{"name":"eyes","count":1,"users":["U2"]}]}]}`)

	page, err := client(t, s).History(context.Background(), "D1", HistoryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(page.Messages))
	}
	got := page.Messages[0].Reactions
	if len(got) != 2 {
		t.Fatalf("reactions = %d, want 2", len(got))
	}
	if got[0].Name != "white_check_mark" || got[0].Count != 2 {
		t.Errorf("first reaction = %+v, want white_check_mark x2", got[0])
	}
	if len(got[0].Users) != 2 || got[0].Users[0] != "U1" || got[0].Users[1] != "U2" {
		t.Errorf("first reaction users = %v, want [U1 U2]", got[0].Users)
	}
	if got[1].Name != "eyes" || got[1].Count != 1 {
		t.Errorf("second reaction = %+v, want eyes x1", got[1])
	}
}

// The overwhelming majority of messages carry no reactions at all, and the
// field is simply absent when they do not. That must decode to no reactions
// rather than failing the read.
func TestAMessageWithNoReactionsDecodesCleanly(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785804051.982839","text":"topic","user":"U1"}]}`)

	page, err := client(t, s).History(context.Background(), "D1", HistoryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(page.Messages[0].Reactions); n != 0 {
		t.Fatalf("reactions = %d, want 0", n)
	}
}

// Slack generates a rich_text block for ordinary messages, so a rule of "has
// blocks" would refuse nearly every real message and make the change verbs
// unusable. The question is whether the text is the content or a fallback.
func TestOnlyContentTextCannotStandForCounts(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  Message
		want bool
	}{
		{"plain text", Message{Text: "hi"}, false},
		{"rich_text only", Message{Text: "hi", Blocks: []byte(`[{"type":"rich_text"}]`)}, false},
		{"link unfurl", Message{Text: "hi", Attachments: []byte(`[{"title":"a page"}]`)}, false},
		{"section block", Message{Blocks: []byte(`[{"type":"section"}]`)}, true},
		{"image beside rich_text", Message{Blocks: []byte(`[{"type":"rich_text"},{"type":"image"}]`)}, true},
		{"a file", Message{Files: []byte(`[{"id":"F1"}]`)}, true},
		{"empty list", Message{Blocks: []byte(`[]`)}, false},
		{"undecodable", Message{Blocks: []byte(`{"unexpected":1}`)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.msg.HasUnrenderedContent(); got != c.want {
				t.Fatalf("HasUnrenderedContent = %v, want %v", got, c.want)
			}
		})
	}
}
