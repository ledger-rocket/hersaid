package render

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/slackread"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

// hostile is what a workspace member can put in a message or a display name:
// an OSC 8 hyperlink, a clear-screen, the C1 control-sequence introducer both
// as a rune and as a bare byte, a carriage return that overprints the line, and
// a right-to-left override.
const hostile = "\x1b]8;;https://example.com\x07\x1b[2J\u009b31m\x9b31m\r\u202e"

// assertTerminalSafe fails on any rune that could drive or rearrange a
// terminal. A newline and a tab are the layout these writers produce
// themselves, and the two joiners shape what is written; those are all that is
// allowed through. The output must
// also be valid UTF-8: a bare 0x9b byte is read as the C1 introducer by a
// terminal in an 8-bit mode, and ranging over it here would see only U+FFFD.
func assertTerminalSafe(t *testing.T, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	for _, r := range out {
		if r == '\n' || r == '\t' || r == '\u200c' || r == '\u200d' {
			continue
		}
		if termsafe.IsHostile(r) {
			t.Fatalf("output carries %U, which drives a terminal: %q", r, out)
		}
	}
}

// A transcript prints text and names that any workspace member chooses,
// including an external Slack Connect guest. None of it may reach the terminal
// as a control sequence: the author, the text, a mentioned name, a reactor or
// an emoji name.
func TestTranscriptStripsTerminalControls(t *testing.T) {
	t.Parallel()
	names := NamesOf([]cache.User{
		{ID: "U1", Profile: cache.Profile{DisplayName: "ann" + hostile}},
		{ID: "U2", Profile: cache.Profile{DisplayName: "bob" + hostile}},
	})
	msgs := []slackread.Message{
		{
			TS: "1785708834.000000", User: "U1",
			Text:      "hi <@U2>" + hostile + "\nsecond" + hostile,
			Reactions: []slackread.Reaction{{Name: "eyes" + hostile, Count: 1, Users: []string{"U2"}}},
		},
		{TS: "1785708835.000000", Username: "hook" + hostile, Text: "from a webhook"},
		{TS: "1785708836.000000", BotID: "B1" + hostile, Text: "from a bot"},
	}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
	for _, want := range []string{"ann", "hi @bob", "second", ":eyes", "(bob", "hook", "from a webhook", "B1", "from a bot"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("transcript lost the visible text %q:\n%s", want, b.String())
		}
	}
}

// A display name is a mention's label, so a line break inside one would land in
// the text. It is stripped there like any other control, so a name cannot start
// a line that looks like a message of its own.
func TestAMentionedNameCannotBreakTheLine(t *testing.T) {
	t.Parallel()
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "ann\n08-02 22:14  boss  approved"}}})
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "bot", Text: "ask <@U1>"}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "08-02 22:13  bot  ask @ann08-02 22:14  boss  approved\n"; b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// A name made only of characters that are stripped would print as nothing. The
// author then falls back as an unknown one does, and a mention or a reactor
// keeps its id, so the line still says who.
func TestANameThatStripsToNothingFallsBack(t *testing.T) {
	t.Parallel()
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "\u202e\x1b"}}})
	msgs := []slackread.Message{{
		TS: "1785708834.000000", User: "U1", Username: "webhook", Text: "hi <@U1>",
		Reactions: []slackread.Reaction{{Name: "eyes", Count: 1, Users: []string{"U1"}}},
	}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	want := "08-02 22:13  webhook  hi @U1\n" + continuation + ":eyes: 1 (U1)\n"
	if b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// The reply hint is a command to run, and part of it is the address the reader
// typed. It is held to the same rule as the rest of the line.
func TestTheReplyHintIsTerminalSafe(t *testing.T) {
	t.Parallel()
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "bot", Text: "q", ReplyCount: 2}}
	o := opts()
	o.ThreadCommand = "hear thread #eng" + hostile

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
}

// Message text keeps the joiners that build an emoji, which is what the
// transcript printed before it was made terminal safe.
func TestMessageTextKeepsTheZeroWidthJoiner(t *testing.T) {
	t.Parallel()
	family := "\U0001F468\u200d\U0001F467"
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "bot", Text: family + " deployed"}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
	if !strings.Contains(b.String(), family) {
		t.Fatalf("transcript lost the joiner: %q", b.String())
	}
}

// A name keeps its joiners too: a display name can hold an emoji built from
// several, and it prints whole as an author, a mention and a reactor.
func TestANameKeepsTheZeroWidthJoiner(t *testing.T) {
	t.Parallel()
	coder := "Sam \U0001F469\u200d\U0001F4BB"
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: coder}}})
	msgs := []slackread.Message{{
		TS: "1785708834.000000", User: "U1", Text: "cc <@U1>",
		Reactions: []slackread.Reaction{{Name: "eyes", Count: 1, Users: []string{"U1"}}},
	}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	want := "08-02 22:13  " + coder + "  cc @" + coder + "\n" + continuation + ":eyes: 1 (" + coder + ")\n"
	if b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// Each step of the author fallback is made safe before it is chosen, so a
// username that strips to nothing gives way to the bot id rather than to "?".
func TestAnAuthorThatStripsToNothingFallsThroughToTheBotID(t *testing.T) {
	t.Parallel()
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "\x1b\u202e", BotID: "B0123ABCD", Text: "hi"}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "08-02 22:13  B0123ABCD  hi\n"; b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// A timestamp is printed as a column of its own and, when it will not parse,
// in place of the date. It is held to the same rule as everything else.
func TestATimestampIsTerminalSafe(t *testing.T) {
	t.Parallel()
	o := opts()
	o.ShowTS = true
	msgs := []slackread.Message{{TS: "x\x1b[2J", Username: "bot", Text: "hi"}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
}

// A search hit prints a channel name, a username and the text, and the same
// rule holds for each.
func TestSearchStripsTerminalControls(t *testing.T) {
	t.Parallel()
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "ann" + hostile}}})
	m := slackread.SearchMatch{Username: "bob" + hostile, Text: "hi <@U1>" + hostile}
	m.Channel.Name = "eng" + hostile

	var b bytes.Buffer
	err := WriteSearch(&b, []slackread.SearchMatch{m}, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
	for _, want := range []string{"#eng", "bob", "hi @ann"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("search line lost the visible text %q: %q", want, b.String())
		}
	}
}

// A search hit starts with "#channel  author", so a hit whose text starts a new
// line with the same shape would read as a second hit from someone else. Its
// later lines are indented, as a transcript's are.
func TestASearchHitCannotForgeAnotherHit(t *testing.T) {
	t.Parallel()
	m := slackread.SearchMatch{Username: "bob", Text: "ok\n#general  ceo  wire the money today"}
	m.Channel.Name = "eng"

	var b bytes.Buffer
	err := WriteSearch(&b, []slackread.SearchMatch{m}, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	want := "#eng  bob  ok\n" + continuation + "#general  ceo  wire the money today\n"
	if b.String() != want {
		t.Fatalf("search output = %q, want %q", b.String(), want)
	}
}

// A file's line names it and, when it was not written, says why. The name is
// the uploader's choice, and a reason can quote what Slack sent back.
func TestFilesLinesStripTerminalControls(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	err := WriteFiles(&b, []FileResult{
		{Status: "written", Path: "eng-1/a" + hostile + ".pdf", Bytes: 1},
		{Status: "skipped", Name: "b" + hostile + ".pdf", Reason: "external" + hostile},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, b.String())
}

// A known user whose display name strips to nothing, posting with no username,
// is named by their id, as their reaction and a mention of them are.
func TestAnAuthorWithNothingElseFallsBackToTheUserID(t *testing.T) {
	t.Parallel()
	names := NamesOf([]cache.User{{ID: "U0123ABCD", Profile: cache.Profile{DisplayName: "\u200b"}}})
	msgs := []slackread.Message{{TS: "1785708834.000000", User: "U0123ABCD", Text: "hi"}}

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "08-02 22:13  U0123ABCD  hi\n"; b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// The hint is checked against the rule names follow, so it is printed by that
// rule too: an address with a joiner in it prints whole, and resolves.
func TestTheReplyHintKeepsAJoiner(t *testing.T) {
	t.Parallel()
	coder := "@Sam \U0001F469\u200d\U0001F4BB"
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "bot", Text: "q", ReplyCount: 2}}
	o := opts()
	o.ThreadCommand = "hear thread " + coder

	var b bytes.Buffer
	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "hear thread "+coder+" 1785708834.000000") {
		t.Fatalf("hint lost the joiner: %q", b.String())
	}
}
