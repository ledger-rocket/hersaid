package slackwrite

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// Same discipline as PostMessage: every argument carries a parameter separator,
// and none of them may split into a field Slack honours.
func TestScheduleMessageSendsExactlyTheDeclaredFields(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"channel":"C0000000000","scheduled_message_id":"Q0000000001","post_at":"1785715200","message":{"text":"stored"}}`)

	postAt := time.Unix(1785715200, 0)
	_, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel:  "C0000000000&blocks=[{}]",
		Text:     "later&blocks=[{}]&post_at=1",
		ThreadTS: "1785712596.256909&channel=C9999EVIL",
		PostAt:   postAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "post_at", "text", "thread_ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
	if got := req.Body.Get("text"); got != "later&blocks=[{}]&post_at=1" {
		t.Fatalf("text = %q; the separators must not have split it", got)
	}
	if got := req.Body.Get("post_at"); got != "1785715200" {
		t.Fatalf("post_at = %q, want whole Unix seconds", got)
	}
	if len(req.Query) != 0 {
		t.Fatalf("query string = %v, want empty", req.Query)
	}
}

// No thread, no thread_ts: an empty field would be a thread Slack has to
// reject rather than none.
func TestScheduleMessageOmitsAnEmptyThread(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"channel":"C0000000000","scheduled_message_id":"Q0000000001","message":{"text":"x"}}`)

	_, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel: "C0000000000", Text: "x", PostAt: time.Unix(1785715200, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Last().BodyFields(), []string{"channel", "post_at", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
}

// The handle, the conversation Slack resolved the target to, and what Slack
// stored are what the caller records.
func TestScheduleMessageReturnsTheHandle(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"channel":"D0000000000","scheduled_message_id":"Q0000000001","post_at":"1785715200","message":{"text":"later &amp; on"}}`)

	got, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel: "U0000000000", Text: "later & on", PostAt: time.Unix(1785715200, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ScheduleResponse{ID: "Q0000000001", Channel: "D0000000000", Text: "later &amp; on"}
	if *got != want {
		t.Fatalf("reply = %+v, want %+v", *got, want)
	}
}

// ok:true with no handle may still mean a message is waiting, so it is
// ambiguous rather than a refusal: a retry could schedule it twice.
func TestAScheduleWithNoHandleIsAmbiguous(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"channel":"C0000000000"}`)

	_, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel: "C0000000000", Text: "x", PostAt: time.Unix(1785715200, 0),
	})
	if got := Classify(err); got != Ambiguous {
		t.Fatalf("Classify(%v) = %v, want Ambiguous", err, got)
	}
}

// ok:true with a handle but no conversation is ambiguous too. The message may
// be waiting, but a target given as a #name or a user id leaves no
// conversation id to cancel it by.
func TestAScheduleWithNoConversationIsAmbiguous(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"scheduled_message_id":"Q0000000001","message":{"text":"x"}}`)

	got, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel: "U0000000000", Text: "x", PostAt: time.Unix(1785715200, 0),
	})
	if got != nil || Code(err) != CodeMalformed {
		t.Fatalf("reply = %+v, err = %v; want no reply and %s", got, err, CodeMalformed)
	}
	if c := Classify(err); c != Ambiguous {
		t.Fatalf("Classify(%v) = %v, want Ambiguous", err, c)
	}
}

// Slack's reference shows the id and post_at as strings in one reply and as
// numbers in another; both decode, and the cursor is followed to the end.
func TestScheduledMessagesFollowsTheCursor(t *testing.T) {
	s := slacktest.New(t)
	s.Pages("chat.scheduledMessages.list",
		`{"ok":true,"scheduled_messages":[{"id":"Q0000000001","channel_id":"C0000000000","post_at":1785715200,"date_created":1785712596,"text":"first"}],"response_metadata":{"next_cursor":"page2"}}`,
		`{"ok":true,"scheduled_messages":[{"id":1000000002,"channel_id":"C0000000000","post_at":"1785718800","date_created":1785712596,"text":"second"}],"response_metadata":{"next_cursor":""}}`,
	)

	got, err := client(t, s).ScheduledMessages(context.Background(), "C0000000000")
	if err != nil {
		t.Fatal(err)
	}
	want := []ScheduledMessage{
		{ID: "Q0000000001", Channel: "C0000000000", PostAt: time.Unix(1785715200, 0), Text: "first"},
		{ID: "1000000002", Channel: "C0000000000", PostAt: time.Unix(1785718800, 0), Text: "second"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %+v\nwant %+v", got, want)
	}

	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if got, want := reqs[0].BodyFields(), []string{"channel", "limit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first page fields = %v, want %v", got, want)
	}
	if got := reqs[1].Body.Get("cursor"); got != "page2" {
		t.Fatalf("second page cursor = %q, want the one the first page gave", got)
	}
}

// Without a channel the list covers every conversation, so no channel field
// goes on the wire.
func TestScheduledMessagesWithoutAChannelAsksForAll(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)

	got, err := client(t, s).ScheduledMessages(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("list = %+v, want empty", got)
	}
	if fields := s.Last().BodyFields(); !reflect.DeepEqual(fields, []string{"limit"}) {
		t.Fatalf("body fields = %v, want only limit", fields)
	}
}

// A cursor that never empties is refused at the cap rather than walked forever,
// and the pages already read are not returned as if they were the whole list.
func TestScheduledMessagesStopsAtThePageCap(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[],"response_metadata":{"next_cursor":"again"}}`)

	got, err := client(t, s).ScheduledMessages(context.Background(), "")
	if !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("err = %v, want ErrTooManyPages", err)
	}
	if got != nil {
		t.Fatalf("list = %+v, want nothing", got)
	}
	if hits := s.Hits(); hits != maxListPages {
		t.Fatalf("%d requests, want %d", hits, maxListPages)
	}
}

// A post_at that is neither a number nor a string of digits is a reply that
// did not parse, and so is an entry without the id or the conversation id that
// cancel it. One bad entry spoils the page, rather than listing a message that
// --unschedule cannot name.
func TestScheduledMessagesRefusesAnUnusableEntry(t *testing.T) {
	for _, tc := range []struct{ name, entry string }{
		{"an unreadable time", `{"id":"Q0000000001","channel_id":"C0000000000","post_at":"soon","text":"x"}`},
		{"no id", `{"channel_id":"C0000000000","post_at":1785715200,"text":"x"}`},
		{"an empty id", `{"id":"","channel_id":"C0000000000","post_at":1785715200,"text":"x"}`},
		{"no conversation id", `{"id":"Q0000000001","post_at":1785715200,"text":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := slacktest.New(t)
			s.Reply("chat.scheduledMessages.list",
				`{"ok":true,"scheduled_messages":[{"id":"Q0000000002","channel_id":"C0000000000","post_at":1785715200,"text":"fine"},`+
					tc.entry+`]}`)

			got, err := client(t, s).ScheduledMessages(context.Background(), "")
			if got != nil || Code(err) != CodeMalformed {
				t.Fatalf("list = %+v, err = %v; want nothing and %s", got, err, CodeMalformed)
			}
		})
	}
}

func TestDeleteScheduledMessageSendsOnlyTheAddress(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	err := client(t, s).DeleteScheduledMessage(context.Background(), "C0000000000", "Q0000000001&channel=C9999EVIL")
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "scheduled_message_id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
	if got := req.Body.Get("scheduled_message_id"); got != "Q0000000001&channel=C9999EVIL" {
		t.Fatalf("scheduled_message_id = %q; the separators must not have split it", got)
	}
}

// Both writes need chat:write, so a missing_scope names it rather than the
// generic fallback.
func TestScheduleEndpointsNameTheirScope(t *testing.T) {
	for _, method := range []string{methodSchedule, methodUnschedule} {
		if got := Scope(&APIError{Method: method, Code: CodeMissingScope}); got != "chat:write" {
			t.Errorf("Scope(%s) = %q, want chat:write", method, got)
		}
	}
}

// The seconds Slack is sent are the instant asked for, whatever zone it was
// given in.
func TestScheduleMessageSendsTheInstantNotTheWallClock(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":true,"channel":"C0000000000","scheduled_message_id":"Q0000000001","message":{"text":"x"}}`)

	at := time.Date(2026, 8, 4, 9, 0, 0, 0, time.FixedZone("UTC+1", 3600))
	_, err := client(t, s).ScheduleMessage(context.Background(), ScheduleRequest{
		Channel: "C0000000000", Text: "x", PostAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.FormatInt(time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC).Unix(), 10)
	if got := s.Last().Body.Get("post_at"); got != want {
		t.Fatalf("post_at = %q, want %q", got, want)
	}
}
