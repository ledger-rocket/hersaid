package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// utc keeps the date column deterministic. time.Local cannot be used for this:
// it is resolved once per process, so a test that set TZ would pass or fail
// depending on what ran before it.
var utc = time.UTC

func opts() Options { return Options{Location: utc} }

// The transcript format is the interface people already read, down to the two
// spaces between its columns: date, author, text.
func TestTranscriptLineFormatIsStable(t *testing.T) {
	var b bytes.Buffer
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "stepan"}}})
	msgs := []slackread.Message{{TS: "1785708834.862009", User: "U1", Text: "ship it"}}

	err := WriteTranscript(&b, msgs, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "08-02 22:13  stepan  ship it\n"; b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// --ts prefixes the raw timestamp, which is the argument `sez --thread` and
// `tap` take. The text column does not move with it: the continuation indent
// is a constant, and a reader diffing two transcripts notices a column that
// shifted.
func TestShowTSPrefixesTheRawTimestamp(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{TS: "1785708834.862009", Username: "bot", Text: "a\nb"}}
	o := opts()
	o.ShowTS = true

	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	want := "1785708834.862009  08-02 22:13  bot  a\n" + strings.Repeat(" ", 20) + "b\n"
	if b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

// A multi-line message is indented to the text column, so its second line
// cannot be mistaken for a message of its own by a reader or by a parser
// splitting on newlines.
func TestMultiLineTextIsIndentedToTheTextColumn(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{TS: "1785708834.000000", Username: "bot", Text: "one\ntwo\nthree"}}

	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), b.String())
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, strings.Repeat(" ", 20)) {
			t.Errorf("continuation line %q is not indented to column 20", l)
		}
		if strings.HasPrefix(l, strings.Repeat(" ", 21)) {
			t.Errorf("continuation line %q is indented past column 20", l)
		}
	}
}

// The indent is a constant, not a computed alignment: 20 is where the text
// starts after a five-character author ("08-02 22:13" + 2 + 5 + 2), and every
// other author length is left slightly ragged, because the alternative — an
// indent that varies per message — makes a transcript harder to read, not
// easier.
func TestTheIndentIsAConstantRatherThanAnAlignment(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{
		{TS: "1785708834.000000", Username: "abcde", Text: "one\ntwo"},
		{TS: "1785708834.000000", Username: "a-much-longer-name", Text: "one\ntwo"},
	}
	err := WriteTranscript(&b, msgs, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if got := strings.Index(lines[0], "one"); got != 20 {
		t.Errorf("a five-character author puts the text at column %d, want 20", got)
	}
	for _, i := range []int{1, 3} {
		if !strings.HasPrefix(lines[i], strings.Repeat(" ", 20)+"two") {
			t.Errorf("continuation %q is not at column 20 regardless of author length", lines[i])
		}
	}
}

// A message with replies says so, and says how to read them: naming the address
// the reader used is what makes the hint a command rather than a template.
func TestReplyHintNamesTheThreadCommand(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{TS: "1785708834.862009", Username: "bot", Text: "topic", ReplyCount: 3}}
	o := opts()
	o.ThreadCommand = "hear thread #eng"

	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	want := "08-02 22:13  bot  topic\n" +
		strings.Repeat(" ", 20) + "⤷ 3 replies — hear thread #eng 1785708834.862009\n"
	if b.String() != want {
		t.Fatalf("transcript = %q, want %q", b.String(), want)
	}
}

func TestNoReplyHintWithoutReplies(t *testing.T) {
	var b bytes.Buffer
	msgs := []slackread.Message{{TS: "1785708834.862009", Username: "bot", Text: "topic"}}
	o := opts()
	o.ThreadCommand = "hear thread #eng"

	err := WriteTranscript(&b, msgs, Names{}, o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "⤷") {
		t.Fatalf("transcript = %q, want no reply hint on a message with none", b.String())
	}
}

// The author fallback, in order. Each step exists because
// the one before it is genuinely absent for some class of message: apps post
// with a username and no user, and some integrations post with neither.
func TestAuthorFallsBackThroughEveryStep(t *testing.T) {
	users := []cache.User{
		{ID: "UDISPLAY", Name: "handle", RealName: "Real Name", Profile: cache.Profile{DisplayName: "display"}},
		{ID: "UREAL", Name: "handle", RealName: "Real Name"},
		{ID: "UHANDLE", Name: "handle"},
		{ID: "UBARE"},
	}
	names := NamesOf(users)

	for _, tc := range []struct {
		name string
		msg  slackread.Message
		want string
	}{
		{"display name wins", slackread.Message{User: "UDISPLAY"}, "display"},
		{"then the real name", slackread.Message{User: "UREAL"}, "Real Name"},
		{"then the handle", slackread.Message{User: "UHANDLE"}, "handle"},
		{"then the id itself", slackread.Message{User: "UBARE"}, "UBARE"},
		{"an unknown user falls to username", slackread.Message{User: "UNOBODY", Username: "webhook"}, "webhook"},
		{"then to the bot id", slackread.Message{BotID: "B0123"}, "B0123"},
		{"and finally to a question mark", slackread.Message{}, "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := names.Author(tc.msg); got != tc.want {
				t.Fatalf("Author = %q, want %q", got, tc.want)
			}
		})
	}
}

// Untrusted text stays data, in the form Go makes available. A display name is
// chosen by its owner — including an external Slack Connect guest — and reaches
// this code as the replacement for a regular-expression match. Regexp.ReplaceAllString
// would expand "$1" and "${x}" in that replacement, so a guest could make a
// mention render as some other message's capture group.
func TestADisplayNameIsDataNotAReplacementTemplate(t *testing.T) {
	for _, display := range []string{"$1", "${x}", "$0$1", `\1`, "$name"} {
		t.Run(display, func(t *testing.T) {
			names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: display}}})
			got := names.Text("hello <@U1> bye")
			if want := "hello @" + display + " bye"; got != want {
				t.Fatalf("Text = %q, want %q — the name was expanded, not printed", got, want)
			}
		})
	}
}

// The same hazard in the message text rather than in the name: text is
// attacker-controlled too, and a message that says "<@U1>" twice must render
// twice, not consume the name as a template.
func TestEveryMentionInAMessageIsResolved(t *testing.T) {
	names := NamesOf([]cache.User{
		{ID: "U1", Profile: cache.Profile{DisplayName: "ann"}},
		{ID: "U2", Profile: cache.Profile{DisplayName: "bob"}},
	})
	got := names.Text("<@U1> and <@U2> and <@U1>")
	if want := "@ann and @bob and @ann"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

// A mention of somebody the user list does not have — an external guest, a
// deleted account, a cache written before they joined — keeps the id. It is
// the only thing left that identifies who was meant.
func TestAnUnknownMentionKeepsItsID(t *testing.T) {
	if got := (Names{}).Text("ping <@U0NOBODY>"); got != "ping @U0NOBODY" {
		t.Fatalf("Text = %q, want the id preserved", got)
	}
}

// The <@U123|label> form is left alone: it already carries the label a reader
// needs, and rewriting it would say something the message does not.
func TestTheLabelledMentionFormIsLeftAlone(t *testing.T) {
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "ann"}}})
	if got := names.Text("<@U1|ann>"); got != "<@U1|ann>" {
		t.Fatalf("Text = %q, want it untouched", got)
	}
}

// The date column is local time, which is the whole point of rendering it: a
// Slack timestamp is UTC seconds and nobody reads their day in UTC.
func TestTheDateColumnIsRenderedInTheGivenZone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no zoneinfo: %v", err)
	}
	const ts = "1785708834.862009"
	if got, want := Stamp(ts, utc), "08-02 22:13"; got != want {
		t.Fatalf("Stamp(UTC) = %q, want %q", got, want)
	}
	if got, want := Stamp(ts, tokyo), "08-03 07:13"; got != want {
		t.Fatalf("Stamp(Tokyo) = %q, want %q — the zone is being ignored", got, want)
	}
}

// A timestamp Slack never sends, but a cache or a hand-written test fixture
// might: it is returned rather than dropped, because it is the only handle the
// reader has on that message.
func TestAnUnparseableTimestampSurvives(t *testing.T) {
	if got := Stamp("not-a-ts", utc); got != "not-a-ts" {
		t.Fatalf("Stamp = %q, want the input back", got)
	}
	if got := rfc3339("not-a-ts"); got != "" {
		t.Fatalf("rfc3339 = %q, want empty for a timestamp that will not parse", got)
	}
}

// A search hit names its channel and carries no date column, and its author is
// the username Slack attached rather than a user-list lookup — search results
// carry no user id to look up.
func TestSearchLineNamesTheChannel(t *testing.T) {
	var b bytes.Buffer
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "ann"}}})
	m := slackread.SearchMatch{Message: slackread.Message{TS: "1785708834.862009", Username: "bob", Text: "hi <@U1>"}}
	m.Channel.Name = "eng"

	err := WriteSearch(&b, []slackread.SearchMatch{m}, names, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "#eng  bob  hi @ann\n"; b.String() != want {
		t.Fatalf("search line = %q, want %q", b.String(), want)
	}
}

func TestSearchLineFallsBackToQuestionMarks(t *testing.T) {
	var b bytes.Buffer
	err := WriteSearch(&b, []slackread.SearchMatch{{}}, Names{}, opts())
	if err != nil {
		t.Fatal(err)
	}
	if want := "#?  ?  \n"; b.String() != want {
		t.Fatalf("search line = %q, want %q", b.String(), want)
	}
}

// The structured form carries both texts: the raw one, which names the user
// ids a consumer may want to address, and the rendered one, which is what the
// transcript shows. Both are always present — a field that appears only when
// it differs is a field consumers get wrong.
func TestStructuredMessagesCarryRawAndRenderedText(t *testing.T) {
	names := NamesOf([]cache.User{{ID: "U1", Profile: cache.Profile{DisplayName: "ann"}}})
	msgs := []slackread.Message{{TS: "1785708834.862009", User: "U2", Text: "hi <@U1>", ReplyCount: 2}}

	got := Messages(msgs, names)
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	m := got[0]
	if m.Text != "hi <@U1>" {
		t.Errorf("Text = %q, want it exactly as Slack sent it", m.Text)
	}
	if m.TextRendered != "hi @ann" {
		t.Errorf("TextRendered = %q, want the mention resolved", m.TextRendered)
	}
	if m.Time != "2026-08-02T22:13:54Z" {
		t.Errorf("Time = %q", m.Time)
	}
	if m.User != "U2" || m.TS != "1785708834.862009" || m.ReplyCount != 2 {
		t.Errorf("message = %+v", m)
	}
}

func TestStructuredMessagesAreNeverNull(t *testing.T) {
	b, err := json.Marshal(Transcript{Kind: KindTranscript, Messages: Messages(nil, Names{})})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"messages":[]`)) {
		t.Fatalf("marshalled = %s, want an empty list rather than null", b)
	}
}

// The mention syntax a consumer has to read is <@U…>; writing it as an escape
// sequence would make every hand-inspection of the output harder for nothing.
func TestJSONLeavesSlackMarkupReadable(t *testing.T) {
	var b bytes.Buffer
	err := JSON(&b, Message{Text: "<@U1> & <https://example.com|link>"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "<@U1> & <https://example.com|link>") {
		t.Fatalf("JSON = %s, want the markup unescaped", b.String())
	}
	// Still valid JSON, which is the point of not hand-rolling the encoder.
	var back Message
	err = json.Unmarshal(b.Bytes(), &back)
	if err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	if back.Text != "<@U1> & <https://example.com|link>" {
		t.Fatalf("round trip = %q", back.Text)
	}
}

// inAZoneThatIsNotUTC points time.Local somewhere else for the duration of a
// test. Without it a machine already running in UTC could not tell a time
// rendered in UTC from one rendered in the local zone, and the assertion below
// would hold whether or not the code did.
func inAZoneThatIsNotUTC(t *testing.T) {
	t.Helper()
	prev := time.Local
	time.Local = time.FixedZone("TEST+05", 5*60*60)
	t.Cleanup(func() { time.Local = prev })
}

// The two outputs deliberately disagree about the zone. A transcript is read by
// a person where they are, so its date column is local; the structured document
// travels, and a consumer on another machine has no way to know which zone
// produced it, so its times are UTC.
func TestTheStructuredTimeIsUTCWhereTheTranscriptIsLocal(t *testing.T) {
	inAZoneThatIsNotUTC(t)
	const ts = "1785708834.862009"

	got := Messages([]slackread.Message{{TS: ts, Text: "hi"}}, Names{})
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if want := "2026-08-02T22:13:54Z"; got[0].Time != want {
		t.Errorf("structured time = %q, want %q — the local zone is leaking into it", got[0].Time, want)
	}
	// Options{} is what the commands pass for a transcript: no zone named,
	// meaning the reader's own.
	var b strings.Builder
	err := WriteTranscript(&b, []slackread.Message{{TS: ts, Text: "hi"}}, Names{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "08-03 03:13"; !strings.Contains(b.String(), want) {
		t.Errorf("transcript = %q, want the date column %q in the local zone", b.String(), want)
	}
}

// A search hit carries the same guarantee: hits are the output most likely to
// be collected from several places, and a mixture of zones in one list is
// unusable.
func TestASearchHitsTimeIsUTC(t *testing.T) {
	inAZoneThatIsNotUTC(t)

	m := slackread.SearchMatch{}
	m.TS, m.Text = "1785708834.862009", "hi"
	got := Hits([]slackread.SearchMatch{m}, Names{})
	if len(got) != 1 {
		t.Fatalf("got %d hits, want 1", len(got))
	}
	if want := "2026-08-02T22:13:54Z"; got[0].Time != want {
		t.Errorf("hit time = %q, want %q", got[0].Time, want)
	}
}
