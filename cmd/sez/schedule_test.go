package main

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	// The daylight-saving test loads a named zone. The embedded database is
	// the fallback on a machine without zoneinfo, so the test never skips.
	_ "time/tzdata"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// The ids the scheduling tests address. None of them belongs to a workspace.
const (
	schedChannel = "C0000000000"
	schedID      = "Q0000000001"
)

// schedules stubs a successful chat.scheduleMessage. The reply names a DM, as
// it would for a user id given as the channel, so a test can see which of the
// two the record keeps.
func schedules(s *slacktest.Server) {
	s.Reply("chat.scheduleMessage",
		`{"ok":true,"channel":"D0000000000","scheduled_message_id":"`+schedID+`","post_at":"1785715200","message":{"text":"stored"}}`)
}

// waiting stubs the list with one scheduled message in schedChannel, due at at.
func waiting(s *slacktest.Server, at time.Time, text string) {
	s.Reply("chat.scheduledMessages.list",
		`{"ok":true,"scheduled_messages":[{"id":"`+schedID+`","channel_id":"`+schedChannel+`","post_at":`+
			strconv.FormatInt(at.Unix(), 10)+`,"date_created":1785712596,"text":"`+text+`"}],"response_metadata":{"next_cursor":""}}`)
}

// seedEng writes the conversation cache through the cache package, naming #eng
// as schedChannel, so a #name resolves the way it does after `hear channels`.
func seedEng(t *testing.T) {
	t.Helper()
	err := cache.Write(cache.ConversationsFile, cache.Conversations{
		Channels: []cache.Conversation{{ID: schedChannel, Name: "eng"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func endpoints(s *slacktest.Server) []string {
	reqs := s.Requests()
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Endpoint)
	}
	return out
}

// A schedule renders like a send, plus the time, and sends nothing: the time is
// part of what is being approved. Local time is what the person meant; UTC is
// what the record holds.
func TestAScheduleRendersTheTimeAndSendsNothing(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	schedules(s)

	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	code, stdout, stderr := sez(t, s, flagAt, at.Format(time.RFC3339), schedChannel, "see you then")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if hits := s.Hits(); hits != 0 {
		t.Fatalf("Slack received %d requests during a render, want 0", hits)
	}
	for _, want := range []string{
		"--- dry run: nothing sent ---",
		"post at : " + at.Local().Format("Mon 2006-01-02 15:04:05 MST"),
		"(" + at.Format("2006-01-02 15:04:05") + " UTC)",
		"see you then",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("render lacks %q:\n%s", want, stdout)
		}
	}

	recs := readLog(t, log)
	if len(recs) != 1 || recs[0]["phase"] != "dry-run" {
		t.Fatalf("records = %v, want one dry-run", recs)
	}
	if recs[0]["action"] != string(audit.Schedule) || recs[0]["post_at"] != at.Format("2006-01-02T15:04:05Z") {
		t.Fatalf("dry-run record = %v, want message.schedule at %s", recs[0], at)
	}
}

// Under --send a schedule goes to chat.scheduleMessage and never to
// chat.postMessage. The text reaches Slack exactly as a send's would, mention
// syntax included, and the record says when it posts and how to name it.
func TestAScheduleUnderSendSchedulesInsteadOfPosting(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	schedules(s)

	before := time.Now()
	code, stdout, stderr := sez(t, s, "--send", flagIn, "2h", schedChannel, "ping <@U0000000000>")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := endpoints(s); !reflect.DeepEqual(got, []string{"chat.scheduleMessage"}) {
		t.Fatalf("endpoints = %v, want only chat.scheduleMessage", got)
	}

	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "post_at", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want %v", got, want)
	}
	if got := req.Body.Get("text"); got != "ping <@U0000000000>" {
		t.Fatalf("text = %q, want it as given", got)
	}
	postAt, err := strconv.ParseInt(req.Body.Get("post_at"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := before.Add(2*time.Hour).Unix()-1, time.Now().Add(2*time.Hour).Unix()
	if postAt < lo || postAt > hi {
		t.Fatalf("post_at = %d, want two hours from the run, in [%d, %d]", postAt, lo, hi)
	}
	if !strings.Contains(stdout, "scheduled to "+schedChannel) || !strings.Contains(stdout, "id="+schedID) {
		t.Fatalf("stdout = %q, want the target and the id", stdout)
	}

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("phases = %v, want intent then outcome", got)
	}
	stamp := time.Unix(postAt, 0).UTC().Format("2006-01-02T15:04:05Z")
	for _, r := range recs {
		if r["action"] != string(audit.Schedule) || r["post_at"] != stamp || r["text"] != "ping <@U0000000000>" {
			t.Fatalf("%v record = %v, want message.schedule at %s with the text", r["phase"], r, stamp)
		}
	}
	if recs[0]["scheduled_message_id"] != nil || recs[0]["ok"] != nil {
		t.Fatalf("intent = %v, want no id and ok null: nothing has been asked yet", recs[0])
	}
	out := recs[1]
	if out["ok"] != true || out["scheduled_message_id"] != schedID || out["ts"] != nil {
		t.Fatalf("outcome = %v, want ok, the id, and no ts: nothing has posted", out)
	}
	if out["channel_id"] != "D0000000000" || out["slack_text"] != "stored" {
		t.Fatalf("outcome = %v, want the conversation and the text Slack reported", out)
	}
}

// A person is addressed as a send addresses one: Slack takes the user id and
// opens the DM. The reply names that DM, and the record keeps it, because a
// cancellation can only name a conversation id.
func TestAScheduleAddressesAPersonAsASendDoes(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	schedules(s)

	code, stdout, stderr := sez(t, s, "--send", flagIn, "1h", "U0000000000", "x")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := s.Last().Body.Get("channel"); got != "U0000000000" {
		t.Fatalf("channel = %q, want the user id as given", got)
	}
	if !strings.Contains(stdout, "channel=D0000000000") {
		t.Fatalf("stdout = %q, want the DM Slack resolved it to", stdout)
	}
	recs := readLog(t, log)
	if recs[0]["channel_id"] != "U0000000000" || recs[1]["channel_id"] != "D0000000000" {
		t.Fatalf("channel_id = %v then %v, want the user id, then the DM", recs[0]["channel_id"], recs[1]["channel_id"])
	}
}

// --thread and --md mean what they mean for a send.
func TestAScheduleKeepsTheThreadAndTheConversion(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	schedules(s)

	code, stdout, stderr := sez(t, s, "--send", "--md", "--thread", "1785712596.256909", flagIn, "1h",
		schedChannel, "**done**")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	req := s.Last()
	if got := req.Body.Get("thread_ts"); got != "1785712596.256909" {
		t.Fatalf("thread_ts = %q", got)
	}
	if got := req.Body.Get("text"); got != "*done*" {
		t.Fatalf("text = %q, want the mrkdwn conversion", got)
	}
	if !strings.Contains(stdout, "(thread 1785712596.256909)") {
		t.Fatalf("stdout = %q, want the thread named", stdout)
	}
}

// A time outside what Slack takes is refused before anything is opened, as an
// empty message is; one that does not parse, or a combination that names no one
// thing, is a usage error.
func TestScheduleTimesOutsideTheWindowAreRefused(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	tooFar := time.Now().Add(121 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"a time that has passed", []string{"--send", flagAt, past, schedChannel, "x"}, 1},
		{"more than 120 days ahead", []string{"--send", flagAt, tooFar, schedChannel, "x"}, 1},
		{"no duration at all", []string{"--send", flagIn, "0s", schedChannel, "x"}, 2},
		{"a negative duration", []string{"--send", flagIn, "-5m", schedChannel, "x"}, 2},
		{"under a second", []string{"--send", flagIn, "999ms", schedChannel, "x"}, 2},
		{"a time that does not parse", []string{"--send", flagAt, "tomorrow", schedChannel, "x"}, 2},
		{"both --at and --in", []string{flagAt, "09:00", flagIn, "1h", schedChannel, "x"}, 2},
		{"--at without a value", []string{flagAt}, 2},
		{"a schedule with files", []string{flagIn, "1h", "--file", "f.txt", schedChannel}, 2},
		{"a schedule and an edit", []string{flagIn, "1h", flagEdit, schedChannel, editTS, "x"}, 2},
		{"a schedule and a cancellation", []string{flagIn, "1h", flagUnschedule, schedChannel, schedID}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			schedules(s)

			code, _, stderr := sez(t, s, tc.args...)
			if code != tc.code {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tc.code, stderr)
			}
			if hits := s.Hits(); hits != 0 {
				t.Fatalf("Slack received %d requests", hits)
			}
			if recs := readLog(t, log); len(recs) != 0 {
				t.Fatalf("records = %v; a refusal before the attempt is not one", recs)
			}
		})
	}
}

// --in under a second is a usage error. Truncated to whole seconds it could
// land on now or before it, so it would pass or fail with the clock.
func TestInTakesAtLeastASecond(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 3, 10, 0, 0, 900_000_000, time.UTC)
	for _, in := range []string{"1ns", "500ms", "999ms"} {
		_, refusal := resolveWhen(flags{in: in}, now)
		if refusal != "--in needs a duration of at least 1s, e.g. 90m or 2h30m" {
			t.Errorf("resolveWhen(--in %s) refusal = %q, want the usage error", in, refusal)
		}
	}
	got, refusal := resolveWhen(flags{in: "1s"}, now)
	if refusal != "" || !got.Equal(now.Add(time.Second)) {
		t.Errorf("resolveWhen(--in 1s) = %s, %q; want a second from now", got, refusal)
	}
}

// The local forms are local time in now's zone. HH:MM is today, never moved to
// tomorrow: the caller refuses one that has passed.
func TestParseAtReadsEachForm(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("UTC+1", 3600)
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, zone)
	for _, tc := range []struct{ in, want string }{
		{"2026-08-04T09:00:00Z", "2026-08-04T09:00:00Z"},
		{"2026-08-04T09:00:00+02:00", "2026-08-04T07:00:00Z"},
		{"2026-08-04T09:00", "2026-08-04T08:00:00Z"},
		{"2026-08-04 09:00", "2026-08-04T08:00:00Z"},
		{"2026-08-04T09:00:30", "2026-08-04T08:00:30Z"},
		{"2026-08-04 09:00:30", "2026-08-04T08:00:30Z"},
		{"14:30", "2026-08-03T13:30:00Z"},
		{"09:00", "2026-08-03T08:00:00Z"},
	} {
		got, refusal := parseAt(tc.in, now)
		if refusal != "" {
			t.Errorf("parseAt(%q) refused: %s", tc.in, refusal)
			continue
		}
		if s := got.UTC().Format(time.RFC3339); s != tc.want {
			t.Errorf("parseAt(%q) = %s, want %s", tc.in, s, tc.want)
		}
	}
	for _, bad := range []string{"", "tomorrow", "25:00", "2026-08-04", "9am"} {
		if _, refusal := parseAt(bad, now); refusal == "" {
			t.Errorf("parseAt(%q) parsed; want a usage error", bad)
		}
	}
}

// At a daylight-saving change a local wall time can name no instant, when the
// clocks skip it, or two, when they repeat it. Either is refused rather than
// moved an hour, and a repeated one asks for the offset that picks one. The
// zone is a real one, so the test does not depend on the machine's zone.
func TestParseAtRefusesWallTimesAClockChangeSkipsOrRepeats(t *testing.T) {
	t.Parallel()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// In 2026 London's clocks go forward at 01:00 GMT on 29 March and back at
	// 02:00 BST on 25 October.
	spring := time.Date(2026, 3, 29, 0, 10, 0, 0, london)
	autumn := time.Date(2026, 10, 25, 0, 10, 0, 0, london)
	// time.Date reads a repeated London time as the later one, and a repeated
	// New York time as the earlier one, so New York checks the other order.
	// Its clocks go back at 02:00 EDT on 1 November 2026.
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	westAutumn := time.Date(2026, 11, 1, 0, 10, 0, 0, newYork)
	for _, tc := range []struct {
		name, in string
		now      time.Time
		refusal  string
	}{
		{"a dated time the clocks skip", "2026-03-29T01:30", spring, "the clocks skip it"},
		{"the first second they skip", "2026-03-29 01:00:00", spring, "the clocks skip it"},
		{"a clock time they skip today", "01:30", spring, "the clocks skip it"},
		{
			"a dated time they repeat", "2026-10-25T01:30", autumn,
			"give RFC 3339 with an explicit offset, 2026-10-25T01:30:00+01:00 or 2026-10-25T01:30:00Z",
		},
		{"the first second they repeat", "2026-10-25 01:00:00", autumn, "give RFC 3339 with an explicit offset"},
		{"a clock time they repeat today", "01:30", autumn, "give RFC 3339 with an explicit offset"},
		{
			"a time repeated west of UTC", "2026-11-01T01:30", westAutumn,
			"give RFC 3339 with an explicit offset, 2026-11-01T01:30:00-04:00 or 2026-11-01T01:30:00-05:00",
		},
	} {
		got, refusal := parseAt(tc.in, tc.now)
		if !strings.Contains(refusal, tc.refusal) {
			t.Errorf("%s: parseAt(%q) = %s, refusal %q; want a refusal saying %q", tc.name, tc.in, got, refusal, tc.refusal)
		}
	}

	// Either side of each change, and an explicit offset inside one, name one
	// instant each. From October 1968 London kept BST all winter: the zone
	// changed but the offset did not, so nothing repeats there.
	for _, tc := range []struct{ in, want string }{
		{"1969-06-01T12:00", "1969-06-01T11:00:00Z"},
		{"2026-03-29T00:59:59", "2026-03-29T00:59:59Z"},
		{"2026-03-29T02:00", "2026-03-29T01:00:00Z"},
		{"2026-10-25T00:59:59", "2026-10-24T23:59:59Z"},
		{"2026-10-25T02:00", "2026-10-25T02:00:00Z"},
		{"2026-03-29T01:30:00+01:00", "2026-03-29T00:30:00Z"},
		{"2026-10-25T01:30:00+01:00", "2026-10-25T00:30:00Z"},
		{"2026-10-25T01:30:00Z", "2026-10-25T01:30:00Z"},
	} {
		got, refusal := parseAt(tc.in, autumn)
		if refusal != "" {
			t.Errorf("parseAt(%q) refused: %s", tc.in, refusal)
			continue
		}
		if s := got.UTC().Format(time.RFC3339); s != tc.want {
			t.Errorf("parseAt(%q) = %s, want %s", tc.in, s, tc.want)
		}
	}
}

// The gate is the same for a schedule: no credential, no request.
func TestAScheduleWithNoCredentialIssuesNoRequest(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)
	schedules(s)

	code, _, stderr := sez(t, s, "--send", flagIn, "1h", schedChannel, "x")
	if code != 1 || s.Hits() != 0 {
		t.Fatalf("exit = %d, %d requests; want 1 and none (stderr %q)", code, s.Hits(), stderr)
	}
	recs := readLog(t, log)
	if len(recs) != 1 || recs[0]["phase"] != string(audit.Abort) || recs[0]["action"] != string(audit.Schedule) {
		t.Fatalf("records = %v, want one abort for the schedule", recs)
	}
}

// The allowlist covers a schedule as it covers a send.
func TestTheAllowlistCoversASchedule(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "C0000000009")
	s := slacktest.New(t)
	schedules(s)

	code, _, _ := sez(t, s, "--send", flagIn, "1h", schedChannel, "x")
	if code != 1 || s.Hits() != 0 {
		t.Fatalf("exit = %d, %d requests; want 1 and none", code, s.Hits())
	}
	if recs := readLog(t, log); len(recs) != 1 || recs[0]["phase"] != string(audit.Refused) {
		t.Fatalf("records = %v, want one refused", recs)
	}
}

// Slack refusing a schedule, for a time it calls past or a scope the app lacks,
// is a failure: nothing is waiting, so there is no id, and the one refusal a
// reader can act on names its scope.
func TestARefusedScheduleIsFailed(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduleMessage", `{"ok":false,"error":"missing_scope"}`)

	code, stdout, stderr := sez(t, s, "--send", flagIn, "1h", schedChannel, "x")
	if code != 1 || !strings.Contains(stderr, "chat:write") {
		t.Fatalf("exit = %d, stderr = %q; want 1 and the scope named", code, stderr)
	}
	if strings.Contains(stdout, "scheduled to") {
		t.Fatalf("stdout = %q; a refusal must not read as a schedule", stdout)
	}
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("phases = %v", got)
	}
	if out := recs[1]; out["ok"] != false || out["error"] == nil || out["scheduled_message_id"] != nil {
		t.Fatalf("outcome = %v, want ok false, the error, and no id", out)
	}
}

// The window's edges: exactly 120 days ahead is accepted, a second more is not,
// and neither is the present second.
func TestScheduleTimeEdges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		ok   bool
	}{
		{"one second ahead", now.Add(time.Second), true},
		{"exactly 120 days ahead", now.Add(scheduleHorizon), true},
		{"a second past 120 days", now.Add(scheduleHorizon + time.Second), false},
		{"now", now, false},
	} {
		var stderr strings.Builder
		got, code, done := scheduleTime(&stderr, flags{at: tc.at.Format(time.RFC3339)}, now)
		if done == tc.ok {
			t.Errorf("%s: done = %v (code %d, stderr %q), want accepted = %v", tc.name, done, code, stderr.String(), tc.ok)
			continue
		}
		if tc.ok && !got.Equal(tc.at) {
			t.Errorf("%s: post at %s, want %s", tc.name, got, tc.at)
		}
		if !tc.ok && code != 1 {
			t.Errorf("%s: exit = %d, want 1: a time out of range is a refusal, not a usage error", tc.name, code)
		}
	}
}

// A dropped reply may still have scheduled the message, so the outcome is
// unknown, never failed: a retry could schedule it twice. The record says to
// look with --scheduled first, since a waiting message does not show in its
// conversation.
func TestADroppedScheduleIsUnknown(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Drop("chat.scheduleMessage")

	code, _, stderr := sez(t, s, "--send", flagIn, "1h", schedChannel, "x")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "UNKNOWN") {
		t.Fatalf("stderr = %q, want delivery called unknown", stderr)
	}
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("phases = %v", got)
	}
	if recs[1]["ok"] != nil {
		t.Fatalf("outcome ok = %v, want null", recs[1]["ok"])
	}
	if msg, _ := recs[1]["error"].(string); !strings.HasSuffix(msg, "delivery unknown, run sez --scheduled before retrying") {
		t.Fatalf("outcome error = %q, want the advice to list what is waiting", msg)
	}
}

// --scheduled lists what is waiting, soonest first, and writes nothing: no
// request but the list, and no audit log at all.
func TestScheduledListsWhatIsWaitingAndRecordsNothing(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	later := time.Unix(1785718800, 0)
	sooner := time.Unix(1785715200, 0)
	s.Reply("chat.scheduledMessages.list",
		`{"ok":true,"scheduled_messages":[`+
			`{"id":"Q0000000002","channel_id":"C0000000000","post_at":`+strconv.FormatInt(later.Unix(), 10)+`,"text":"second"},`+
			`{"id":"Q0000000001","channel_id":"C0000000000","post_at":`+strconv.FormatInt(sooner.Unix(), 10)+`,"text":"first\nline two"}`+
			`],"response_metadata":{"next_cursor":""}}`)

	code, stdout, stderr := sez(t, s, flagScheduled)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	want := "Q0000000001  C0000000000  " + when(sooner) + "\n  first\n  line two\n" +
		"Q0000000002  C0000000000  " + when(later) + "\n  second\n"
	if stdout != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	if fields := s.Last().BodyFields(); !reflect.DeepEqual(fields, []string{"limit"}) {
		t.Fatalf("body fields = %v, want no channel for a list of all", fields)
	}
	_, err := os.Stat(log)
	if !os.IsNotExist(err) {
		t.Fatalf("the audit log exists after a list (err %v); a read is not an attempt", err)
	}
}

// hostileEntry is a list entry whose conversation id carries an ESC sequence
// and whose text carries one and a right-to-left override, as JSON escapes.
const hostileEntry = `{"id":"` + schedID + `","channel_id":"` + schedChannel + `\u001b[2J","post_at":1785715200,` +
	`"text":"a\u001b[31mred\u202eevil"}`

// terminalControls are the two runes hostileEntry smuggles in.
const terminalControls = "\x1b\u202e"

// Everything the list prints came from Slack, so none of it may drive the
// terminal: the conversation id and the text lose their ESC sequences and
// bidi overrides, and what is left still prints.
func TestScheduledStripsTerminalControls(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[`+hostileEntry+`]}`)

	code, stdout, stderr := sez(t, s, flagScheduled)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.ContainsAny(stdout, terminalControls) {
		t.Fatalf("stdout carries terminal controls: %q", stdout)
	}
	for _, want := range []string{schedID, schedChannel + "[2J", "a[31mredevil"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %q", want, stdout)
		}
	}
}

// The cancellation's render prints the text read from the list, so it is
// stripped the same way.
func TestUnscheduleRenderStripsTerminalControls(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[`+hostileEntry+`]}`)

	code, stdout, stderr := sez(t, s, flagUnschedule, schedChannel, schedID)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.ContainsAny(stdout, terminalControls) {
		t.Fatalf("stdout carries terminal controls: %q", stdout)
	}
	if !strings.Contains(stdout, "a[31mredevil") {
		t.Fatalf("render lacks the stripped text: %q", stdout)
	}
}

// Given a conversation, the list asks for that one.
func TestScheduledNarrowsToOneConversation(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)

	code, stdout, _ := sez(t, s, flagScheduled, schedChannel)
	if code != 0 || stdout != "no scheduled messages\n" {
		t.Fatalf("exit = %d, stdout = %q", code, stdout)
	}
	if got := s.Last().Body.Get("channel"); got != schedChannel {
		t.Fatalf("channel = %q, want %s", got, schedChannel)
	}
}

// A list Slack refuses is reported and exits 1, and still writes no record.
func TestAFailedListIsReportedAndNotRecorded(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":false,"error":"invalid_auth"}`)

	code, stdout, stderr := sez(t, s, flagScheduled)
	if code != 1 || !strings.Contains(stderr, "invalid_auth") || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_, err := os.Stat(log)
	if !os.IsNotExist(err) {
		t.Fatalf("the audit log exists after a list (err %v)", err)
	}
}

// The list endpoint needs no scope, so a missing_scope from it is reported as
// Slack sent it, without instructions to grant a scope that does not exist.
func TestAListRefusedForScopeNamesNoScopeToGrant(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":false,"error":"missing_scope"}`)

	code, _, stderr := sez(t, s, flagScheduled)
	if code != 1 || !strings.Contains(stderr, "missing_scope") {
		t.Fatalf("exit = %d, stderr = %q; want 1 and the error", code, stderr)
	}
	if strings.Contains(stderr, "User Token Scopes") {
		t.Fatalf("stderr = %q; want no scope to grant", stderr)
	}
}

// The list endpoint takes a conversation id only, so a #name resolves through
// the cache first, and a cold cache is refused rather than passed through.
func TestScheduledResolvesANameBeforeAsking(t *testing.T) {
	t.Run("warm cache", func(t *testing.T) {
		isolate(t)
		seedEng(t)
		t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
		s := slacktest.New(t)
		s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)

		code, _, stderr := sez(t, s, flagScheduled, "#eng")
		if code != 0 {
			t.Fatalf("exit = %d (stderr %q)", code, stderr)
		}
		if got := s.Last().Body.Get("channel"); got != schedChannel {
			t.Fatalf("channel = %q, want the id #eng resolves to", got)
		}
	})
	t.Run("cold cache", func(t *testing.T) {
		isolate(t)
		t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
		s := slacktest.New(t)
		s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)

		code, _, stderr := sez(t, s, flagScheduled, "#eng")
		if code != 1 || s.Hits() != 0 {
			t.Fatalf("exit = %d, %d requests; want 1 and none (stderr %q)", code, s.Hits(), stderr)
		}
		if !strings.Contains(stderr, "hear dms") {
			t.Fatalf("stderr = %q, want the command that fills the cache", stderr)
		}
	})
}

// --scheduled changes nothing, so a flag that shapes or delivers a message is
// meaningless beside it; and without a credential there is nothing to ask with.
func TestScheduledRefusesWhatItCannotDo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		args  []string
		code  int
	}{
		{"--send", "xoxp-fake-write-token", []string{"--send", flagScheduled}, 2},
		{"--md", "xoxp-fake-write-token", []string{"--md", flagScheduled}, 2},
		{"two targets", "xoxp-fake-write-token", []string{flagScheduled, schedChannel, "C0000000009"}, 2},
		{"no credential", "", []string{flagScheduled}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", tc.token)
			s := slacktest.New(t)
			s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)

			code, _, stderr := sez(t, s, tc.args...)
			if code != tc.code || s.Hits() != 0 {
				t.Fatalf("exit = %d, %d requests; want %d and none (stderr %q)", code, s.Hits(), tc.code, stderr)
			}
			if tc.token == "" && !strings.Contains(stderr, "security add-generic-password") {
				t.Fatalf("stderr = %q, want the way to enrol one", stderr)
			}
		})
	}
}

// A cancellation renders the message that will not now post, read from Slack,
// and cancels nothing without --send. The record keeps what it said and when.
func TestUnscheduleRendersTheMessageFirst(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	at := time.Unix(1785715200, 0)
	waiting(s, at, "see you then")
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, stdout, stderr := sez(t, s, flagUnschedule, schedChannel, schedID)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := endpoints(s); !reflect.DeepEqual(got, []string{"chat.scheduledMessages.list"}) {
		t.Fatalf("endpoints = %v, want the read only", got)
	}
	for _, want := range []string{"message : " + schedID, "post at : " + when(at), "see you then"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("render lacks %q:\n%s", want, stdout)
		}
	}
	recs := readLog(t, log)
	if len(recs) != 1 || recs[0]["phase"] != "dry-run" || recs[0]["action"] != "message.unschedule" {
		t.Fatalf("records = %v, want one message.unschedule dry-run", recs)
	}
	if recs[0]["text"] != "see you then" || recs[0]["scheduled_message_id"] != schedID {
		t.Fatalf("record = %v, want the text and the id", recs[0])
	}
}

// Under --send it cancels the one message named, with an intent line before the
// request that already names it.
func TestUnscheduleUnderSendCancels(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	at := time.Unix(1785715200, 0)
	waiting(s, at, "see you then")
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, stdout, stderr := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	req := s.Last()
	if req.Endpoint != "chat.deleteScheduledMessage" {
		t.Fatalf("last endpoint = %s", req.Endpoint)
	}
	if req.Body.Get("channel") != schedChannel || req.Body.Get("scheduled_message_id") != schedID {
		t.Fatalf("body = %v", req.Body)
	}
	if !strings.Contains(stdout, "unscheduled "+schedID) {
		t.Fatalf("stdout = %q", stdout)
	}
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("phases = %v", got)
	}
	stamp := at.UTC().Format("2006-01-02T15:04:05Z")
	for _, r := range recs {
		if r["scheduled_message_id"] != schedID || r["post_at"] != stamp || r["text"] != "see you then" {
			t.Fatalf("%v record = %v, want the id, the time and the text", r["phase"], r)
		}
	}
	if recs[1]["ok"] != true {
		t.Fatalf("outcome = %v", recs[1])
	}
}

// A cancellation names its conversation as a delete does, and a #name reaches
// both the read and the cancellation as the id it resolves to.
func TestUnscheduleResolvesANameForBothRequests(t *testing.T) {
	isolate(t)
	seedEng(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	waiting(s, time.Unix(1785715200, 0), "x")
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, _, stderr := sez(t, s, "--send", flagUnschedule, "#eng", schedID)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("endpoints = %v, want the read and the cancellation", endpoints(s))
	}
	for _, r := range reqs {
		if got := r.Body.Get("channel"); got != schedChannel {
			t.Fatalf("%s channel = %q, want the id #eng resolves to", r.Endpoint, got)
		}
	}
}

// Slack refusing a cancellation, as it does within 60 seconds of the time or
// once the message has posted, is a failure; a reply that never came back is
// unknown, since the message may or may not still be waiting.
func TestAFailedUnscheduleIsRecordedAsSlackLeftIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  string
		ok     any
		stderr string
	}{
		{"refused", `{"ok":false,"error":"invalid_scheduled_message_id"}`, false, "invalid_scheduled_message_id"},
		{"dropped", "", nil, "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			waiting(s, time.Unix(1785715200, 0), "x")
			if tc.reply == "" {
				s.Drop("chat.deleteScheduledMessage")
			} else {
				s.Reply("chat.deleteScheduledMessage", tc.reply)
			}

			code, _, stderr := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
			if code != 1 || !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("exit = %d, stderr = %q; want 1 and %q", code, stderr, tc.stderr)
			}
			recs := readLog(t, log)
			if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
				t.Fatalf("phases = %v", got)
			}
			if out := recs[1]; out["ok"] != tc.ok || out["scheduled_message_id"] != schedID {
				t.Fatalf("outcome = %v, want ok %v and the id", out, tc.ok)
			}
		})
	}
}

// The allowlist covers a cancellation before the read: a conversation outside
// it is not even listed.
func TestTheAllowlistCoversAnUnschedule(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "C0000000009")
	s := slacktest.New(t)
	waiting(s, time.Unix(1785715200, 0), "x")
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, _, _ := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
	if code != 1 || s.Hits() != 0 {
		t.Fatalf("exit = %d, %d requests; want 1 and none", code, s.Hits())
	}
	if recs := readLog(t, log); len(recs) != 1 || recs[0]["phase"] != string(audit.Refused) {
		t.Fatalf("records = %v, want one refused", recs)
	}
}

// The gate's usual advice, to render without --send, does not help a
// cancellation, whose render reads with the same credential. It says so
// instead.
func TestTheGateDoesNotSendACancellationToARenderThatAlsoFails(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)

	code, _, stderr := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
	if code != 1 || s.Hits() != 0 {
		t.Fatalf("exit = %d, %d requests; want 1 and none", code, s.Hits())
	}
	if strings.Contains(stderr, "omit --send") || !strings.Contains(stderr, "even to render") {
		t.Fatalf("stderr = %q, want the cancellation's own advice", stderr)
	}
}

// A message the list does not hold is refused, under --send too: it may have
// posted already, and a cancellation that cannot show what it cancels has not
// earned the request.
func TestUnscheduleRefusesAMessageItCannotFind(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":true,"scheduled_messages":[]}`)
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, _, stderr := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got := endpoints(s); !reflect.DeepEqual(got, []string{"chat.scheduledMessages.list"}) {
		t.Fatalf("endpoints = %v, want no cancellation", got)
	}
	if !strings.Contains(stderr, "may have posted already") {
		t.Fatalf("stderr = %q", stderr)
	}
	if recs := readLog(t, log); len(recs) != 1 || recs[0]["phase"] != string(audit.Abort) {
		t.Fatalf("records = %v, want one abort", recs)
	}
}

// A list read that fails is no read: under --send the cancellation is never
// asked for, and the record is one abort.
func TestUnscheduleAbortsWhenTheReadFails(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.scheduledMessages.list", `{"ok":false,"error":"invalid_auth"}`)
	s.Reply("chat.deleteScheduledMessage", `{"ok":true}`)

	code, _, stderr := sez(t, s, "--send", flagUnschedule, schedChannel, schedID)
	if code != 1 || !strings.Contains(stderr, "invalid_auth") {
		t.Fatalf("exit = %d, stderr = %q; want 1 and the error", code, stderr)
	}
	if got := endpoints(s); !reflect.DeepEqual(got, []string{"chat.scheduledMessages.list"}) {
		t.Fatalf("endpoints = %v, want the read only", got)
	}
	if recs := readLog(t, log); len(recs) != 1 || recs[0]["phase"] != string(audit.Abort) {
		t.Fatalf("records = %v, want one abort", recs)
	}
}

// The read needs the write credential, so without one a cancellation cannot
// even render, and says so rather than rendering nothing.
func TestUnscheduleWithNoCredentialCannotRender(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)
	waiting(s, time.Unix(1785715200, 0), "x")

	code, _, stderr := sez(t, s, flagUnschedule, schedChannel, schedID)
	if code != 1 || s.Hits() != 0 {
		t.Fatalf("exit = %d, %d requests; want 1 and none (stderr %q)", code, s.Hits(), stderr)
	}
	if !strings.Contains(stderr, "security add-generic-password") || strings.Contains(stderr, "omit --send") {
		t.Fatalf("stderr = %q, want the way to enrol one, and no advice to render", stderr)
	}
	if recs := readLog(t, log); len(recs) != 1 || recs[0]["phase"] != string(audit.Abort) {
		t.Fatalf("records = %v, want one abort", recs)
	}
}

// --unschedule takes a target and an id: no text, no thread, nothing to
// convert, and an id that could not be one is refused before any request.
func TestUnscheduleTakesATargetAndAnID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"no id", []string{flagUnschedule, schedChannel}, 2},
		{"text", []string{flagUnschedule, schedChannel, schedID, "words"}, 2},
		{"--thread", []string{"--thread", editTS, flagUnschedule, schedChannel, schedID}, 2},
		{"--md", []string{"--md", flagUnschedule, schedChannel, schedID}, 2},
		{"--delete", []string{"--delete", flagUnschedule, schedChannel, schedID}, 2},
		{"a suspicious id", []string{flagUnschedule, schedChannel, "Q1&channel=C9"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			waiting(s, time.Unix(1785715200, 0), "x")

			code, _, stderr := sez(t, s, tc.args...)
			if code != tc.code || s.Hits() != 0 {
				t.Fatalf("exit = %d, %d requests; want %d and none (stderr %q)", code, s.Hits(), tc.code, stderr)
			}
		})
	}
}
