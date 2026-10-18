package render

import (
	"bytes"
	"strings"
	"testing"
)

func inboxMsg(ts, author, text string) InboxMessage {
	m := Message{TS: ts, Author: author, Text: text, TextRendered: text}
	return InboxMessage{Message: m}
}

// The text form is a header per conversation, its messages and threads in the
// order they happened, and each thread's replies under its parent. A parent is
// context, so it is one line however long it was.
func TestInboxTextFormatIsStable(t *testing.T) {
	t.Parallel()
	parent := inboxMsg("1699000000.000100", "alice", "the plan for the release,\nin full, with every step written out so that it runs well past one line")
	in := Inbox{
		Complete: true,
		Conversations: []InboxConversation{
			{
				Channel: ChannelRef{ID: "D0000000001"}, Kind: ConversationDM, User: "U0000000002", Name: "bob",
				Complete: true,
				Messages: []InboxMessage{inboxMsg("1700050000.000100", "bob", "two\nlines")},
				Threads: []InboxThread{{
					ThreadTS: "1699000000.000100", Parent: &parent, Complete: true,
					Replies: []InboxMessage{inboxMsg("1700060000.000100", "bob", "done")},
				}},
			},
			{
				Channel: ChannelRef{ID: "C0000000001", Name: "general"}, Kind: ConversationChannel,
				Complete: true,
				Threads: []InboxThread{{
					ThreadTS: "1699500000.000100", Complete: true,
					Replies: []InboxMessage{inboxMsg("1700070000.000100", "carol", "a reply")},
				}},
			},
			{
				Channel: ChannelRef{ID: "G0000000001", Name: "mpdm-alice--bob-1"}, Kind: ConversationGroupDM,
				Complete: true,
				Messages: []InboxMessage{inboxMsg("1700080000.000100", "alice", "hi all")},
			},
		},
	}
	var b bytes.Buffer
	err := WriteInbox(&b, in, opts())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"@bob  (dm)",
		"thread 11-03 08:26  alice  the plan for the release, in full, with every step written out so that it runs w…  (hear thread D0000000001 1699000000.000100)",
		"    11-15 14:53  bob  done",
		"11-15 12:06  bob  two",
		continuation + "lines",
		"#general",
		"thread 11-09 03:20 (parent not returned)  (hear thread C0000000001 1699500000.000100)",
		"    11-15 17:40  carol  a reply",
		"mpdm-alice--bob-1  (group dm)",
		"11-15 20:26  alice  hi all",
		"",
	}, "\n")
	if b.String() != want {
		t.Fatalf("inbox =\n%s\nwant\n%s", b.String(), want)
	}
}

// An incomplete inbox ends by naming every part that is not complete, so the
// text form can never pass for a whole answer when it is not one.
func TestInboxTextNamesWhatIsIncomplete(t *testing.T) {
	t.Parallel()
	in := Inbox{
		Complete: false,
		Sources: []InboxSource{
			{Name: "dms", Complete: true},
			{Name: "threads", Complete: false, Reason: "call budget"},
		},
		Conversations: []InboxConversation{{
			Channel: ChannelRef{ID: "D0000000001"}, Kind: ConversationDM, User: "U0000000002", Name: "bob",
			Complete: false, Reason: "rate limited",
			Threads: []InboxThread{{ThreadTS: "1699000000.000100", Complete: false, Reason: "call budget"}},
		}},
	}
	var b bytes.Buffer
	err := WriteInbox(&b, in, opts())
	if err != nil {
		t.Fatal(err)
	}
	wantTail := "incomplete:\n" +
		"  search threads: call budget\n" +
		"  @bob: rate limited\n" +
		"  thread @bob 1699000000.000100: call budget\n"
	if !strings.HasSuffix(b.String(), wantTail) {
		t.Fatalf("inbox =\n%s\nwant it to end with\n%s", b.String(), wantTail)
	}
}

// Text a stranger chose cannot drive the terminal: the inbox prints more of it
// in one place than any other command.
func TestInboxTextIsTerminalSafe(t *testing.T) {
	t.Parallel()
	m := inboxMsg("1700050000.000100", "eve\x1b[2J", "hi\x1b]0;owned\x07 there\u202e")
	in := Inbox{Complete: true, Conversations: []InboxConversation{
		{
			Channel: ChannelRef{ID: "C0000000001", Name: "gen\x1beral"}, Kind: ConversationChannel,
			Complete: true, Messages: []InboxMessage{m},
			Threads: []InboxThread{{ThreadTS: "1699000000.000100", Parent: &m, Complete: true}},
		},
		{
			Channel: ChannelRef{ID: "D0000000001"}, Kind: ConversationDM, User: "U0000000002", Name: "mal\x1b[2Jlory\u202e",
			Complete: true, Messages: []InboxMessage{m},
		},
	}}
	var b bytes.Buffer
	err := WriteInbox(&b, in, opts())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(b.String(), "\x1b\x07\u202e") {
		t.Fatalf("inbox carries a control or format character: %q", b.String())
	}
	if !strings.Contains(b.String(), "\n@mal[2Jlory  (dm)\n") {
		t.Fatalf("inbox = %q, want the DM header built from its name, made safe", b.String())
	}
}

// --ts puts the raw timestamp first, as a transcript does, so a line can be
// answered with sez --thread.
func TestInboxTextShowsTimestampsWhenAsked(t *testing.T) {
	t.Parallel()
	in := Inbox{Complete: true, Conversations: []InboxConversation{{
		Channel: ChannelRef{ID: "C0000000001", Name: "general"}, Kind: ConversationChannel, Complete: true,
		Messages: []InboxMessage{inboxMsg("1700050000.000100", "bob", "hi")},
	}}}
	var b bytes.Buffer
	o := opts()
	o.ShowTS = true
	err := WriteInbox(&b, in, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := "#general\n1700050000.000100  11-15 12:06  bob  hi\n"; b.String() != want {
		t.Fatalf("inbox = %q, want %q", b.String(), want)
	}
}

func TestTSLessComparesAsNumbers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"999999999.000100", "1000000000.000100", true},
		{"1700000000.000100", "1700000000.000200", true},
		{"1700000000.000200", "1700000000.000100", false},
		{"1700000000.000100", "1700000000.000100", false},
		{"1700000000.000100", "not-a-ts", true},
		{"not-a-ts", "1700000000.000100", false},
		{"-1.5", "1.5", false},
		{"1700000000.000100", "1700000000.1", true},
		{"1700000000.1", "1700000000.000100", false},
		{"1700000000.10", "1700000000.1", false},
		{"1700000000.000100", "1700000000.0001000000000000000000000001", true},
	} {
		if got := TSLess(c.a, c.b); got != c.want {
			t.Errorf("TSLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A thread's parent is one line of message text, and it keeps the joiners a
// message keeps. Its author is a name, and falls back when it strips to
// nothing.
func TestInboxParentKeepsTheJoinersAndAnAuthor(t *testing.T) {
	t.Parallel()
	family := "\U0001F468\u200d\U0001F467"
	parent := inboxMsg("1700050000.000100", "\x1b\u202e", family+" shipped")
	in := Inbox{Complete: true, Conversations: []InboxConversation{{
		Channel: ChannelRef{ID: "C0000000001", Name: "general"}, Kind: ConversationChannel, Complete: true,
		Threads: []InboxThread{{ThreadTS: "1700050000.000100", Parent: &parent, Complete: true}},
	}}}
	var b bytes.Buffer
	err := WriteInbox(&b, in, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), family+" shipped") {
		t.Fatalf("inbox lost the joiner: %q", b.String())
	}
	if !strings.Contains(b.String(), "  ?  "+family) {
		t.Fatalf("inbox = %q, want an author that strips to nothing to read ?", b.String())
	}
}

// An inbox author that strips to nothing falls back as a transcript's does,
// through the bot id to the user id; a header falls back to the id.
func TestInboxNamesThatStripToNothingFallBack(t *testing.T) {
	t.Parallel()
	m := inboxMsg("1700050000.000100", "\x1b\u202e", "hi")
	m.User = "U0000000009"
	in := Inbox{Complete: true, Conversations: []InboxConversation{{
		Channel: ChannelRef{ID: "D0000000001"}, Kind: ConversationDM, Name: "\u202e", Complete: true,
		Messages: []InboxMessage{m},
	}}}
	var b bytes.Buffer
	err := WriteInbox(&b, in, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "  U0000000009  hi") {
		t.Fatalf("inbox = %q, want the author to fall back to the user id", b.String())
	}
	if !strings.HasPrefix(b.String(), "D0000000001  (dm)\n") {
		t.Fatalf("inbox = %q, want the header to fall back to the id", b.String())
	}
}
