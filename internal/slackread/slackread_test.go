package slackread

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// readCred mints a cred.Read for tests. The token field is unexported, so
// resolution is the only way to build one — which is the point: no package can
// turn a string it chose into a credential.
func readCred(t *testing.T, token string) cred.Read {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SLACK_READ_TOKEN", token)
	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func client(t *testing.T, s *slacktest.Server) *Client {
	t.Helper()
	return New(readCred(t, "xoxp-fake-read-token"), slackhttp.New(2*time.Second), s.URL)
}

func TestAuthTest(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0123ABCD"}`)

	got, err := client(t, s).AuthTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "stepan" || got.Team != "LedgerRocket" || got.UserID != "U0123ABCD" {
		t.Fatalf("response = %+v", got)
	}
	if auth := s.Last().Auth; auth != "Bearer xoxp-fake-read-token" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// A Slack reply with ok:false is an error, not an empty result — on every
// endpoint, since one that forgot would report a refusal as nothing found.
func TestAPIErrorIsSurfaced(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":false,"error":"invalid_auth"}`)

	_, err := client(t, s).AuthTest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("error = %v, want it to name invalid_auth", err)
	}
}

func TestMissingErrorFieldStillFails(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":false}`)

	_, err := client(t, s).AuthTest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown error") {
		t.Fatalf("error = %v, want it to report an unknown error", err)
	}
}

func TestMalformedResponseIsAnError(t *testing.T) {
	s := slacktest.New(t)
	s.ReplyFunc("auth.test", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>proxy interstitial</html>"))
	})

	_, err := client(t, s).AuthTest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed response") {
		t.Fatalf("error = %v, want a malformed-response error", err)
	}
}

// A single unpaginated call silently truncates a workspace larger than one
// page, which surfaces much later as a channel that "does not exist".
func TestConversationsListFollowsTheCursor(t *testing.T) {
	s := slacktest.New(t)
	s.Pages("conversations.list",
		`{"ok":true,"channels":[{"id":"C1","name":"a"}],"response_metadata":{"next_cursor":"page2"}}`,
		`{"ok":true,"channels":[{"id":"C2","name":"b"}],"response_metadata":{"next_cursor":""}}`,
	)

	got, err := client(t, s).ConversationsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "C1" || got[1].ID != "C2" {
		t.Fatalf("conversations = %+v, want both pages", got)
	}

	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2", len(reqs))
	}
	first, second := reqs[0].Query, reqs[1].Query
	if got := first.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200 — Slack rejects >= 1000 and recommends <= 200", got)
	}
	if got := first.Get("types"); got != "public_channel,private_channel,im,mpim" {
		t.Errorf("types = %q; without im and mpim no DM would resolve", got)
	}
	if got := first.Get("exclude_archived"); got != "true" {
		t.Errorf("exclude_archived = %q, want true", got)
	}
	if first.Has("cursor") {
		t.Error("the first page sent a cursor")
	}
	if got := second.Get("cursor"); got != "page2" {
		t.Errorf("second page cursor = %q, want page2", got)
	}
}

func TestUsersListFollowsTheCursor(t *testing.T) {
	s := slacktest.New(t)
	s.Pages("users.list",
		`{"ok":true,"members":[{"id":"U1","name":"a"}],"response_metadata":{"next_cursor":"p2"}}`,
		`{"ok":true,"members":[{"id":"U2","name":"b"}],"response_metadata":{}}`,
	)

	got, err := client(t, s).UsersList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("members = %+v, want both pages", got)
	}
	if got := s.Requests()[0].Query.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200", got)
	}
}

// The zero credential must issue no request either. A read token is a bearer
// credential like any other, and cred.Read{} is constructible in any package.
func TestZeroCredentialIssuesNoRequest(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true}`)
	s.Reply("conversations.list", `{"ok":true,"channels":[]}`)

	c := New(cred.Read{}, slackhttp.New(2*time.Second), s.URL)

	_, err := c.AuthTest(context.Background())
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("AuthTest error = %v, want ErrNoCredential", err)
	}
	_, err = c.ConversationsList(context.Background())
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("ConversationsList error = %v, want ErrNoCredential", err)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// A fetch that returns nothing must yield an empty list, not nil. The result is
// written to the cache verbatim, and `{"channels":null}` breaks every
// `jq '.channels[]'`, which the cache's readers rely on.
func TestAnEmptyFetchIsAnEmptyListNotNull(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.list", `{"ok":true,"channels":[]}`)
	s.Reply("users.list", `{"ok":true,"members":[]}`)

	c := client(t, s)
	convs, err := c.ConversationsList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if convs == nil {
		t.Fatal("ConversationsList returned a nil slice, which marshals as null")
	}
	users, err := c.UsersList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if users == nil {
		t.Fatal("UsersList returned a nil slice, which marshals as null")
	}

	// What the cache file would then hold.
	for _, tc := range []struct {
		body any
		want string
	}{
		{cache.Conversations{Channels: convs}, `{"channels":[]}`},
		{cache.Users{Members: users}, `{"members":[]}`},
	} {
		b, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("cache body = %s, want %s", b, tc.want)
		}
	}
}

// A server that keeps returning the same non-empty cursor — broken, hostile, or
// something in the middle — must not be followed forever.
func TestPaginationIsBounded(t *testing.T) {
	s := slacktest.New(t)
	// Pages repeats its last body, so this cursor never runs out.
	s.Pages("conversations.list",
		`{"ok":true,"channels":[{"id":"C1"}],"response_metadata":{"next_cursor":"always"}}`)
	s.Pages("users.list",
		`{"ok":true,"members":[{"id":"U1"}],"response_metadata":{"next_cursor":"always"}}`)

	c := client(t, s)
	_, err := c.ConversationsList(context.Background())
	if err == nil {
		t.Fatal("ConversationsList followed an endless cursor to completion")
	}
	_, err = c.UsersList(context.Background())
	if err == nil {
		t.Fatal("UsersList followed an endless cursor to completion")
	}
	if got, want := s.Hits(), 2*maxPages; got != want {
		t.Fatalf("Slack received %d requests, want %d — the cap is what stops it", got, want)
	}
}

func TestHistoryAndReplies(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history", `{"ok":true,"messages":[{"ts":"1.1","user":"U1","text":"hi","reply_count":2}]}`)
	s.Reply("conversations.replies", `{"ok":true,"messages":[{"ts":"1.1","text":"parent"},{"ts":"1.2","text":"reply"}]}`)

	c := client(t, s)
	page, err := c.History(context.Background(), "C0123ABCD", HistoryOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ReplyCount != 2 {
		t.Fatalf("history = %+v", page.Messages)
	}
	q := s.Last().Query
	if q.Get("channel") != "C0123ABCD" || q.Get("limit") != "20" {
		t.Fatalf("history query = %v", q)
	}

	replies, err := c.Replies(context.Background(), "C0123ABCD", "1.1", HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(replies.Messages) != 2 {
		t.Fatalf("replies = %+v", replies.Messages)
	}
	if got := s.Last().Query.Get("ts"); got != "1.1" {
		t.Fatalf("replies ts = %q", got)
	}
}

// A transcript longer than one page is reachable, and a caller can tell a full
// page from the end of the window.
//
// Slack caps a single call at 999 messages, so without this a Phase 2 verb —
// `hear '#alerts-dev' --since 24h` most of all — would silently report the
// newest page as the whole transcript. The cursor travels in both directions:
// out of the page that ended, and back in as the next request's cursor field.
func TestATranscriptIsPagedByTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		call     func(*Client, HistoryOptions) (HistoryPage, error)
		fields   []string
	}{
		{
			"history", "conversations.history",
			func(c *Client, o HistoryOptions) (HistoryPage, error) {
				return c.History(context.Background(), "C0123ABCD", o)
			},
			[]string{"channel", "cursor", "limit"},
		},
		{
			"replies", "conversations.replies",
			func(c *Client, o HistoryOptions) (HistoryPage, error) {
				return c.Replies(context.Background(), "C0123ABCD", "1.1", o)
			},
			[]string{"channel", "cursor", "limit", "ts"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := slacktest.New(t)
			s.Pages(tc.endpoint,
				`{"ok":true,"messages":[{"ts":"2.2","text":"newer"}],"response_metadata":{"next_cursor":"dXNlcjpVMDYxTkZUVDI="}}`,
				`{"ok":true,"messages":[{"ts":"1.1","text":"older"}],"response_metadata":{"next_cursor":""}}`,
			)
			c := client(t, s)

			first, err := tc.call(c, HistoryOptions{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if first.NextCursor != "dXNlcjpVMDYxTkZUVDI=" {
				t.Fatalf("NextCursor = %q; a full page that discards the cursor is a transcript that stops early", first.NextCursor)
			}
			if got := s.Last().QueryFields(); contains(got, "cursor") {
				t.Fatalf("first request carried %v; there was no cursor to continue from", got)
			}

			second, err := tc.call(c, HistoryOptions{Limit: 1, Cursor: first.NextCursor})
			if err != nil {
				t.Fatal(err)
			}
			req := s.Last()
			if got := req.Query.Get("cursor"); got != first.NextCursor {
				t.Fatalf("cursor = %q, want the page's own %q", got, first.NextCursor)
			}
			if got := req.QueryFields(); !equal(got, tc.fields) {
				t.Fatalf("query fields = %v, want exactly %v", got, tc.fields)
			}
			if len(second.Messages) != 1 || second.Messages[0].Text != "older" {
				t.Fatalf("second page = %+v", second.Messages)
			}
			if second.NextCursor != "" {
				t.Fatalf("NextCursor = %q at the end of the window, want empty", second.NextCursor)
			}
		})
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// oldest and latest bound the window by message timestamp. Empty means
// unbounded in that direction, and must not appear on the wire at all: an empty
// oldest= is not the same request as no oldest=.
func TestHistoryWindow(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history", `{"ok":true,"messages":[]}`)
	c := client(t, s)

	_, err := c.History(context.Background(), "C0123ABCD", HistoryOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Last().QueryFields(), []string{"channel", "limit"}; !equal(got, want) {
		t.Fatalf("unbounded query fields = %v, want exactly %v", got, want)
	}

	_, err = c.History(context.Background(), "C0123ABCD", HistoryOptions{
		Limit:  20,
		Oldest: "1785712000.000000",
		Latest: "1785715600.000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if got, want := req.QueryFields(), []string{"channel", "latest", "limit", "oldest"}; !equal(got, want) {
		t.Fatalf("bounded query fields = %v, want exactly %v", got, want)
	}
	if got := req.Query.Get("oldest"); got != "1785712000.000000" {
		t.Fatalf("oldest = %q", got)
	}
	if got := req.Query.Get("latest"); got != "1785715600.000000" {
		t.Fatalf("latest = %q", got)
	}
}

func TestSearch(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages",
		`{"ok":true,"messages":{"matches":[{"ts":"1.1","text":"found","username":"stepan","channel":{"id":"C1","name":"eng"}}]}}`)

	got, err := client(t, s).Search(context.Background(), "deploy failed", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Channel.Name != "eng" || got[0].Text != "found" {
		t.Fatalf("matches = %+v", got)
	}
	if q := s.Last().Query.Get("query"); q != "deploy failed" {
		t.Fatalf("query = %q", q)
	}
}

// No parameter smuggling on the read side: a value containing a parameter
// separator must arrive as one field, never as two.
func TestQueryValuesCannotIntroduceAField(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("search.messages", `{"ok":true,"messages":{"matches":[]}}`)

	_, err := client(t, s).Search(context.Background(), "a&count=1000&token=xoxp-stolen", 20)
	if err != nil {
		t.Fatal(err)
	}
	req := s.Last()
	if got, want := req.QueryFields(), []string{"count", "query"}; !equal(got, want) {
		t.Fatalf("query fields = %v, want exactly %v", got, want)
	}
	if got := req.Query.Get("count"); got != "20" {
		t.Fatalf("count = %q; the query string overrode a declared field", got)
	}
	if req.Query.Has("token") {
		t.Fatal("a token field was smuggled into the query string")
	}
}

// probe observes the outgoing request rather than the reply, because a
// deadline is a property of the client's context and is invisible at the server.
type probe struct {
	next     http.RoundTripper
	deadline time.Time
	hasIt    bool
}

func (p *probe) RoundTrip(r *http.Request) (*http.Response, error) {
	p.deadline, p.hasIt = r.Context().Deadline()
	return p.next.RoundTrip(r)
}

// Every request carries a deadline of its own, so a caller
// passing context.Background() cannot hang — and neither can one that supplied
// an http.Client with no Timeout of its own.
func TestEveryRequestCarriesADeadline(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true}`)

	for _, tc := range []struct {
		name string
		hc   *http.Client
		want time.Duration
	}{
		{
			"client with no timeout falls back to the package default",
			&http.Client{}, slackhttp.DefaultTimeout,
		},
		{
			"client timeout is honoured",
			&http.Client{Timeout: 3 * time.Second}, 3 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{next: http.DefaultTransport}
			tc.hc.Transport = p
			_, err := New(readCred(t, "xoxp-t"), tc.hc, s.URL).AuthTest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !p.hasIt {
				t.Fatal("the outgoing request had no deadline")
			}
			// Generous: the assertion is that the bound is the intended one, not
			// that no time passed between setting it and observing it.
			if d := time.Until(p.deadline); d > tc.want || d < tc.want-2*time.Second {
				t.Fatalf("deadline is %v away, want about %v", d, tc.want)
			}
		})
	}
}

// A deadline already on the caller's context is not extended.
func TestCallerDeadlineIsNotExtended(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true}`)

	p := &probe{next: http.DefaultTransport}
	hc := &http.Client{Timeout: time.Hour, Transport: p}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := New(readCred(t, "xoxp-t"), hc, s.URL).AuthTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(p.deadline); d > time.Second {
		t.Fatalf("deadline is %v away; the caller's 250ms bound was widened", d)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
