package slackwrite

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// Leg 1 declares two things and must put exactly those on the wire. The
// assertion is on the field-name set, not on a value: a filename that split
// into a second field is invisible to a value check, and `channel_id` smuggled
// in here would name a conversation nobody chose.
func TestGetUploadURLSendsExactlyTheDeclaredFields(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.getUploadURLExternal",
		`{"ok":true,"file_id":"F0123ABCD","upload_url":"https://files.slack.example/u?t=1"}`)

	hostile := "trace.pdf&length=1&channel_id=C9999EVIL"
	got, err := client(t, s).GetUploadURL(context.Background(), hostile, 1258291)
	if err != nil {
		t.Fatal(err)
	}

	req := s.Last()
	if want := []string{"filename", "length"}; !reflect.DeepEqual(req.BodyFields(), want) {
		t.Fatalf("body fields = %v, want exactly %v", req.BodyFields(), want)
	}
	if v := req.Body.Get("filename"); v != hostile {
		t.Fatalf("filename = %q; the separators must not have split it", v)
	}
	if v := req.Body.Get("length"); v != "1258291" {
		t.Fatalf("length = %q, want the byte count", v)
	}
	if len(req.Query) != 0 {
		t.Fatalf("query = %v, want empty — every field goes in the body", req.Query)
	}
	if got.FileID != "F0123ABCD" || got.UploadURL != "https://files.slack.example/u?t=1" {
		t.Fatalf("ticket = %+v, want the id and url from the reply", got)
	}
}

// An ok:true carrying nothing usable satisfies the envelope and nothing else.
// Refused here, where the phase is still "staging nothing has happened yet",
// rather than two legs later where the failure would be about a URL that was
// never there.
func TestGetUploadURLRefusesAReplyMissingWhatItPromised(t *testing.T) {
	for _, c := range []struct {
		name, body string
	}{
		{"no file id", `{"ok":true,"file_id":"","upload_url":"https://files.slack.example/u"}`},
		{"no upload url", `{"ok":true,"file_id":"F0123ABCD","upload_url":""}`},
		{"neither", `{"ok":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := slacktest.New(t)
			s.Reply("files.getUploadURLExternal", c.body)
			_, err := client(t, s).GetUploadURL(context.Background(), "trace.pdf", 10)
			if err == nil {
				t.Fatal("GetUploadURL() = nil error, want a refusal")
			}
		})
	}
}

// Slack's own refusal, classified as every other endpoint's is.
func TestGetUploadURLReportsSlacksRefusal(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.getUploadURLExternal", `{"ok":false,"error":"file_uploads_disabled"}`)

	_, err := client(t, s).GetUploadURL(context.Background(), "trace.pdf", 10)
	if Code(err) != "file_uploads_disabled" {
		t.Fatalf("Code(err) = %q, want the code Slack sent (err = %v)", Code(err), err)
	}
	if Classify(err) != Rejected {
		t.Fatalf("Classify(err) = %v, want Rejected", Classify(err))
	}
}

// Leg 3 goes through the same envelope check as every other endpoint. Without
// it a refused share decodes to an empty success, and the classification that
// decides between "nothing was posted" and "check Slack before retrying" reads
// a code off an error that would never exist.
func TestCompleteUploadReportsSlacksRefusal(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", `{"ok":false,"error":"posting_to_channel_denied"}`)

	_, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
	})
	if Code(err) != "posting_to_channel_denied" {
		t.Fatalf("Code(err) = %q, want the code Slack sent (err = %v)", Code(err), err)
	}
	if Classify(err) != Rejected {
		t.Fatalf("Classify(err) = %v, want Rejected", Classify(err))
	}
}

// The scope table is keyed by method name, and the client stamps a method name
// on every error. Nothing but this ties the two together: an endpoint called
// through a literal while the constant held some other string satisfies the
// wire tests through the literal and the table test through the constant, and
// then names no scope at all for a real refusal — which is the whole failure
// the table exists to prevent.
func TestAMissingScopeRefusalNamesItsScopeEndToEnd(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		call     func(*Client) error
	}{
		{"files.getUploadURLExternal", func(c *Client) error {
			_, err := c.GetUploadURL(context.Background(), "trace.pdf", 10)
			return err
		}},
		{"files.completeUploadExternal", func(c *Client) error {
			_, err := c.CompleteUpload(context.Background(), CompleteRequest{
				Files: []CompleteFile{{ID: "F1", Title: "trace.pdf"}}, ChannelID: "C0123ABCD",
			})
			return err
		}},
	} {
		t.Run(c.endpoint, func(t *testing.T) {
			s := slacktest.New(t)
			s.Reply(c.endpoint, `{"ok":false,"error":"missing_scope"}`)
			err := c.call(client(t, s))
			if Code(err) != CodeMissingScope {
				t.Fatalf("Code(err) = %q, want missing_scope", Code(err))
			}
			if got := Scope(err); got != "files:write" {
				t.Fatalf("Scope(err) = %q, want files:write — the wire method must be the table's key", got)
			}
		})
	}
}

// Leg 3 is the only leg that shares anything, so its argument set is the one
// that decides where a file appears. thread_ts and initial_comment are present
// only when given: an empty initial_comment is a comment Slack would render.
func TestCompleteUploadSendsExactlyTheDeclaredFields(t *testing.T) {
	for _, c := range []struct {
		name string
		req  CompleteRequest
		want []string
	}{
		{
			"files and channel alone",
			CompleteRequest{
				Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
				ChannelID: "C0123ABCD",
			},
			[]string{"channel_id", "files"},
		},
		{
			"with a comment",
			CompleteRequest{
				Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
				ChannelID: "C0123ABCD",
				Comment:   "here's the repro",
			},
			[]string{"channel_id", "files", "initial_comment"},
		},
		{
			"in a thread",
			CompleteRequest{
				Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
				ChannelID: "C0123ABCD",
				ThreadTS:  "1785708834.862009",
			},
			[]string{"channel_id", "files", "thread_ts"},
		},
		{
			// The combination, which is a primary flow: a file, a message and a
			// thread. Without this case an implementation shaped as
			// "comment, else thread" passes every other row and drops the
			// thread whenever there is something to say.
			"in a thread with a comment",
			CompleteRequest{
				Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
				ChannelID: "C0123ABCD",
				ThreadTS:  "1785708834.862009",
				Comment:   "here's the repro",
			},
			[]string{"channel_id", "files", "initial_comment", "thread_ts"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := slacktest.New(t)
			s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1"}]}`)

			_, err := client(t, s).CompleteUpload(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			req := s.Last()
			if got := req.BodyFields(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("body fields = %v, want exactly %v", got, c.want)
			}
			// The value, not only the name. A field present with the wrong
			// contents puts the message outside the thread it was addressed to.
			if got := req.Body.Get("thread_ts"); got != c.req.ThreadTS {
				t.Fatalf("thread_ts = %q, want %q", got, c.req.ThreadTS)
			}
			if got := req.Body.Get("channel_id"); got != c.req.ChannelID {
				t.Fatalf("channel_id = %q, want %q", got, c.req.ChannelID)
			}
			if got := req.Body.Get("initial_comment"); got != c.req.Comment {
				t.Fatalf("initial_comment = %q, want %q", got, c.req.Comment)
			}
		})
	}
}

// The files argument is JSON, and its order is the order the caller gave: a
// message shows its attachments in the order they were named.
func TestCompleteUploadEncodesTheFilesArgumentAsJSON(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1"},{"id":"F2"}]}`)

	_, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files: []CompleteFile{
			{ID: "F1", Title: "first.pdf"},
			{ID: "F2", Title: "second.log"},
		},
		ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The exact bytes, not a decode of them. json.Unmarshal matches keys
	// case-insensitively, so a struct marshalled without tags produces
	// {"ID":…,"Title":…} — which Slack does not read — and decoding it back
	// into a tagged struct succeeds and hides that entirely.
	raw := s.Last().Body.Get("files")
	const want = `[{"id":"F1","title":"first.pdf"},{"id":"F2","title":"second.log"}]`
	if raw != want {
		t.Fatalf("files = %s\nwant           %s", raw, want)
	}

	// Decoded too, so the failure above says which half is wrong.
	var got []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	err = json.Unmarshal([]byte(raw), &got)
	if err != nil {
		t.Fatalf("files = %q, which is not JSON: %v", raw, err)
	}
	if len(got) != 2 {
		t.Fatalf("files = %q, want two entries", raw)
	}
	if got[0].ID != "F1" || got[1].ID != "F2" {
		t.Errorf("files = %q, want F1 then F2 in the order given", raw)
	}
}

// A comment carrying parameter separators is one field, not three. This is the
// same guarantee PostMessage has, on the argument that carries what a person
// wrote.
func TestCompleteUploadCannotBeSteeredByItsComment(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1"}]}`)

	hostile := "look&channel_id=C9999EVIL&files=[{\"id\":\"F9\"}]"
	_, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
		Comment:   hostile,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if want := []string{"channel_id", "files", "initial_comment"}; !reflect.DeepEqual(req.BodyFields(), want) {
		t.Fatalf("body fields = %v, want exactly %v", req.BodyFields(), want)
	}
	if v := req.Body.Get("initial_comment"); v != hostile {
		t.Fatalf("initial_comment = %q, want it whole", v)
	}
	if v := req.Body.Get("channel_id"); v != "C0123ABCD" {
		t.Fatalf("channel_id = %q, want the one the caller chose", v)
	}
}

// Slack files a share under public or private according to the channel, not
// according to anything this code chose. Reading only one map would lose the
// timestamp for every private channel and DM, which is most of what this tool
// posts to.
func TestCompletionReadsSharesFromBothMaps(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{
			"public",
			`{"ok":true,"files":[{"id":"F1","shares":{"public":{"C0123ABCD":[{"ts":"1785708834.862009"}]}}}]}`,
			"1785708834.862009",
		},
		{
			"private",
			`{"ok":true,"files":[{"id":"F1","shares":{"private":{"C0123ABCD":[{"ts":"1785708834.862009"}]}}}]}`,
			"1785708834.862009",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := slacktest.New(t)
			s.Reply("files.completeUploadExternal", c.body)

			got, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
				Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
				ChannelID: "C0123ABCD",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Files) != 1 {
				t.Fatalf("files = %+v, want one", got.Files)
			}
			ts := got.Files[0].Shares.TimestampsFor("C0123ABCD")
			if !reflect.DeepEqual(ts, []string{c.want}) {
				t.Fatalf("TimestampsFor = %v, want [%s]", ts, c.want)
			}
		})
	}
}

// A share for somebody else's channel is not this message's timestamp.
func TestTimestampsForNamesOnlyTheChannelAsked(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal",
		`{"ok":true,"files":[{"id":"F1","shares":{"public":{"C0000ELSE":[{"ts":"1.1"}]},`+
			`"private":{"C0123ABCD":[{"ts":"2.2"}]}}}]}`)

	got, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ts := got.Files[0].Shares.TimestampsFor("C0123ABCD"); !reflect.DeepEqual(ts, []string{"2.2"}) {
		t.Fatalf("TimestampsFor(C0123ABCD) = %v, want [2.2]", ts)
	}
	if ts := got.Files[0].Shares.TimestampsFor("C0000NONE"); len(ts) != 0 {
		t.Fatalf("TimestampsFor(C0000NONE) = %v, want none", ts)
	}
}

// Every timestamp, not the first one found. The consensus rule downstream —
// all timestamps for the target channel must agree or the completion is
// ambiguous — can only fire on a list that is actually complete, so a
// TimestampsFor that stopped at the first match would silently disable it.
func TestTimestampsForReturnsEveryShareItFinds(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal",
		`{"ok":true,"files":[{"id":"F1","shares":{`+
			`"public":{"C0123ABCD":[{"ts":"1.1"},{"ts":"2.2"}]},`+
			`"private":{"C0123ABCD":[{"ts":"3.3"}]}}}]}`)

	got, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := got.Files[0].Shares.TimestampsFor("C0123ABCD")
	sort.Strings(ts)
	if want := []string{"1.1", "2.2", "3.3"}; !reflect.DeepEqual(ts, want) {
		t.Fatalf("TimestampsFor = %v, want every share across both maps: %v", ts, want)
	}
}

// An empty ts is absent, never a value to agree with. Returned as one, it would
// satisfy the consensus rule downstream by being the only "value" present, and
// a message would be recorded against a timestamp of "".
func TestTimestampsForTreatsAnEmptyTimestampAsAbsent(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal",
		`{"ok":true,"files":[{"id":"F1","shares":{"public":{"C0123ABCD":[{"ts":""}]}}}]}`)

	got, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ts := got.Files[0].Shares.TimestampsFor("C0123ABCD"); len(ts) != 0 {
		t.Fatalf("TimestampsFor = %q, want an empty ts dropped rather than returned", ts)
	}
}

// The documented minimal reply: ids and nothing else. It is a success with no
// timestamp, not a failure — the file ids are the anchor in that case.
func TestCompletionSurvivesAReplyWithNoShares(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1","title":"trace.pdf"}]}`)

	got, err := client(t, s).CompleteUpload(context.Background(), CompleteRequest{
		Files:     []CompleteFile{{ID: "F1", Title: "trace.pdf"}},
		ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatalf("CompleteUpload() error = %v; a reply with no shares is a success", err)
	}
	if len(got.Files) != 1 || got.Files[0].ID != "F1" {
		t.Fatalf("files = %+v, want the one id", got.Files)
	}
	if ts := got.Files[0].Shares.TimestampsFor("C0123ABCD"); len(ts) != 0 {
		t.Fatalf("TimestampsFor = %v, want none", ts)
	}
}

// The scope a failing endpoint needs, from a table rather than from Slack's
// `needed` field: a value out of a response must not be interpolated into
// instructions telling somebody what to grant.
func TestScopeNamesWhatTheFailingEndpointNeeds(t *testing.T) {
	for _, c := range []struct {
		method, want string
	}{
		{methodPostMessage, "chat:write"},
		{methodUpdate, "chat:write"},
		{methodDelete, "chat:write"},
		{methodAddReaction, "reactions:write"},
		{methodRemoveReaction, "reactions:write"},
		{methodGetUploadURL, "files:write"},
		{methodCompleteUpload, "files:write"},
	} {
		err := error(&APIError{Method: c.method, Code: CodeMissingScope})
		if got := Scope(err); got != c.want {
			t.Errorf("Scope(%s) = %q, want %q", c.method, got, c.want)
		}
	}
	if got := Scope(nil); got != "" {
		t.Errorf("Scope(nil) = %q, want empty", got)
	}
	if got := Scope(errors.New("not a Slack error")); got != "" {
		t.Errorf("Scope(non-API error) = %q, want empty", got)
	}
}

// Structural, as for every other endpoint: a zero credential issues no request
// at all, so a filename and a channel cannot reach anyone's wire.
func TestUploadEndpointsIssueNoRequestWithoutACredential(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.getUploadURLExternal", `{"ok":true,"file_id":"F1","upload_url":"https://x/u"}`)
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1"}]}`)

	c := New(cred.Write{}, slackhttp.New(2*time.Second), s.URL)
	_, err := c.GetUploadURL(context.Background(), "trace.pdf", 10)
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("GetUploadURL error = %v, want ErrNoCredential", err)
	}
	_, err = c.CompleteUpload(context.Background(), CompleteRequest{
		Files: []CompleteFile{{ID: "F1"}}, ChannelID: "C0123ABCD",
	})
	if !errors.Is(err, ErrNoCredential) {
		t.Errorf("CompleteUpload error = %v, want ErrNoCredential", err)
	}
	if s.Hits() != 0 {
		t.Fatalf("Slack received %d requests, want none", s.Hits())
	}
}

// The bearer reaches the API on both legs. Leg 2 is the one that must not have
// it, and that is slackupload's business, not this package's.
func TestUploadEndpointsCarryTheBearer(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("files.getUploadURLExternal", `{"ok":true,"file_id":"F1","upload_url":"https://x/u"}`)
	s.Reply("files.completeUploadExternal", `{"ok":true,"files":[{"id":"F1"}]}`)
	c := client(t, s)

	_, err := c.GetUploadURL(context.Background(), "trace.pdf", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Last().Auth; !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("getUploadURL Authorization = %q, want a bearer", got)
	}

	_, err = c.CompleteUpload(context.Background(), CompleteRequest{
		Files: []CompleteFile{{ID: "F1", Title: "trace.pdf"}}, ChannelID: "C0123ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Last().Auth; !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("completeUpload Authorization = %q, want a bearer", got)
	}
}
