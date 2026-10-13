package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackread"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// The inbox fixtures are a small invented workspace: the reader, two other
// people, a 1:1 DM, a group DM and a channel. Every id is fake.
const (
	inboxMe    = "U0000000001"
	inboxBob   = "U0000000002"
	inboxCarol = "U0000000003"
	dmBob      = "D0000000001"
	dmApp      = "D0000000002"
	groupDM    = "G0000000001"
	general    = "C0000000001"
	random     = "C0000000002"
)

// inboxNow is when every inbox test runs. With the default 24h window, since is
// 1700000000.000000 and until is 1700086400.000000.
var inboxNow = time.Unix(1700086400, 0)

const (
	sinceTS = "1700000000.000000"
	untilTS = "1700086400.000000"
	// oldTS is long before the window: the parent of an old thread.
	oldTS = "1699000000.000100"
	// newParentTS is a parent posted inside the window.
	newParentTS = "1700020000.000100"
	// lookbackTS is a thread inside the mention lookback, before the window.
	lookbackTS = "1699700000.000100"
	// olderLookbackTS is a second thread inside the lookback.
	olderLookbackTS = "1699600000.000100"
)

var inboxUsers = []cache.User{
	{ID: inboxMe, Name: "me", Profile: cache.Profile{DisplayName: "me"}},
	{ID: inboxBob, Name: "bob", Profile: cache.Profile{DisplayName: "bob"}},
	{ID: inboxCarol, Name: "carol", Profile: cache.Profile{DisplayName: "carol"}},
}

// fakeClock is the inbox's time: fixed, and a sleep that records the wait and
// returns at once.
type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	return ctx.Err()
}

func newClock() *fakeClock { return &fakeClock{now: inboxNow} }

func (e *env) inboxAt(clk *fakeClock, args ...string) (string, string, int) {
	e.t.Helper()
	var o, s bytes.Buffer
	code := runWith(append([]string{"inbox"}, args...), &o, &s, testEndpoints(e.srv.URL), timing{clock: clk, setup: inboxDeadline, deadline: inboxDeadline})
	return o.String(), s.String(), code
}

// inbox runs `hear inbox --json` at inboxNow and decodes what it printed.
func (e *env) inbox(args ...string) (render.Inbox, string, int) {
	e.t.Helper()
	stdout, stderr, code := e.inboxAt(newClock(), append([]string{"--json"}, args...)...)
	var got render.Inbox
	if code == 0 {
		decode(e.t, stdout, &got)
	}
	return got, stderr, code
}

// warmInbox fills the user list and the identity, bound to the credential the
// test env sets, so the run makes no request it does not need to.
func (e *env) warmInbox() {
	e.t.Helper()
	err := cache.Write(cache.UsersFile, cache.Users{Members: inboxUsers})
	if err != nil {
		e.t.Fatal(err)
	}
	err = cache.Write(cache.IdentityFile, cache.Identity{
		UserID: inboxMe, User: "me", Team: "example",
		CredentialKey: cred.KeyFor("xoxp-fake-read-token"),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// hit is one search.messages match, in the shape the real API answers with.
type hit struct {
	kind, ch, chName, ts, thread, user, text string
	attachments                              string
	// link replaces the permalink the other fields imply; "-" leaves it out.
	link string
}

func (h hit) json() string {
	flags := `"is_channel":true`
	switch h.kind {
	case render.ConversationDM:
		flags = `"is_im":true`
	case render.ConversationGroupDM:
		flags = `"is_mpim":true,"is_private":true`
	case "none":
		flags = `"is_channel":false`
	}
	link := h.link
	if link == "" {
		link = fmt.Sprintf("https://example.slack.test/archives/%s/p%s", h.ch, strings.ReplaceAll(h.ts, ".", ""))
		if h.thread != "" {
			link += fmt.Sprintf("?thread_ts=%s&cid=%s", h.thread, h.ch)
		}
	}
	permalink := `,"permalink":` + strconv.Quote(link)
	if link == "-" {
		permalink = ""
	}
	attachments := ""
	if h.attachments != "" {
		attachments = `,"attachments":` + h.attachments
	}
	return fmt.Sprintf(`{"iid":"x","type":"message","ts":%q,"user":%q,"username":"u","text":%q,"team":"T0000000000",`+
		`"channel":{"id":%q,"name":%q,%s}%s%s,"blocks":[]}`,
		h.ts, h.user, h.text, h.ch, h.chName, flags, permalink, attachments)
}

// msg is one message as conversations.replies returns it.
type msg struct {
	ts, thread, user, text, subtype string
}

func (m msg) json() string {
	extra := ""
	if m.thread != "" {
		extra += fmt.Sprintf(`,"thread_ts":%q`, m.thread)
	}
	if m.subtype != "" {
		extra += fmt.Sprintf(`,"subtype":%q`, m.subtype)
	}
	return fmt.Sprintf(`{"type":"message","ts":%q,"user":%q,"text":%q%s}`, m.ts, m.user, m.text, extra)
}

func repliesBody(msgs ...msg) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.json())
	}
	return `{"ok":true,"has_more":false,"messages":[` + strings.Join(parts, ",") + `]}`
}

// slackFake is the inbox's Slack: the three searches, routed on their query,
// each a list of pages, and conversations.replies routed on the thread ts.
type slackFake struct {
	dms, mentions, threads, mentionThreads [][]hit
	replies                                map[string]http.HandlerFunc
}

func sourceOf(query string) string {
	switch {
	case strings.HasPrefix(query, "is:dm "):
		return render.SearchDMs
	case strings.HasPrefix(query, "<@") && strings.Contains(query, " is:thread "):
		return render.SearchMentionThreads
	case strings.HasPrefix(query, "<@"):
		return render.SearchMentions
	case strings.HasPrefix(query, "is:thread with:me "):
		return render.SearchThreads
	}
	return ""
}

func searchPage(pages [][]hit, page int) string {
	if len(pages) == 0 {
		return `{"ok":true,"messages":{"matches":[],"paging":{"count":100,"total":0,"page":1,"pages":0}}}`
	}
	hits := pages[min(page, len(pages))-1]
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, h.json())
	}
	return fmt.Sprintf(`{"ok":true,"messages":{"matches":[%s],"paging":{"count":100,"total":%d,"page":%d,"pages":%d}}}`,
		strings.Join(parts, ","), len(pages)*100, page, len(pages))
}

func (e *env) serve(f slackFake) {
	e.srv.ReplyFunc("search.messages", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		var pages [][]hit
		switch sourceOf(r.URL.Query().Get("query")) {
		case render.SearchDMs:
			pages = f.dms
		case render.SearchMentions:
			pages = f.mentions
		case render.SearchThreads:
			pages = f.threads
		case render.SearchMentionThreads:
			pages = f.mentionThreads
		default:
			e.t.Errorf("unexpected search query %q", r.URL.Query().Get("query"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, searchPage(pages, page))
	})
	e.srv.ReplyFunc(methodReplies, func(w http.ResponseWriter, r *http.Request) {
		h, ok := f.replies[r.URL.Query().Get("ts")]
		if !ok {
			e.t.Errorf("unexpected replies read of %s", r.URL.Query().Get("ts"))
			_, _ = io.WriteString(w, `{"ok":false,"error":"thread_not_found"}`)
			return
		}
		h(w, r)
	})
}

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s)
	}
}

// endpoints is every method the fake saw, in order.
func (e *env) endpoints() []string {
	reqs := e.srv.Requests()
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Endpoint)
	}
	return out
}

func (e *env) requestsTo(endpoint string) []slacktest.Request {
	var out []slacktest.Request
	for _, r := range e.srv.Requests() {
		if r.Endpoint == endpoint {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) count(endpoint string) int {
	n := 0
	for _, r := range e.srv.Requests() {
		if r.Endpoint == endpoint {
			n++
		}
	}
	return n
}

// checkInbox holds for every run: only the four read methods phase 1 uses are
// called, and calls.used is exactly what reached Slack, less the cache fills,
// which calls.cache counts.
func (e *env) checkInbox(got render.Inbox, maxCalls int) {
	e.t.Helper()
	allowed := map[string]bool{
		"auth.test": true, "users.list": true, "search.messages": true, methodReplies: true,
	}
	for _, ep := range e.endpoints() {
		if !allowed[ep] {
			e.t.Errorf("inbox called %s", ep)
		}
	}
	fills := e.count("users.list")
	if got.Calls.Used != e.srv.Hits()-fills || got.Calls.Cache != fills {
		e.t.Errorf("calls = %+v, Slack saw %d requests of which %d were cache fills", got.Calls, e.srv.Hits(), fills)
	}
	if got.Calls.Max != maxCalls || got.Calls.Used > maxCalls {
		e.t.Errorf("calls = %+v, want at most %d", got.Calls, maxCalls)
	}
}

func onlyConversation(t *testing.T, got render.Inbox) render.InboxConversation {
	t.Helper()
	if len(got.Conversations) != 1 {
		t.Fatalf("conversations = %+v, want exactly one", got.Conversations)
	}
	return got.Conversations[0]
}

func tsList(msgs []render.InboxMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.TS)
	}
	return out
}

// The case the inbox exists for: someone answers, today, in a thread I joined
// last month. History over the window never returns that parent; the threads
// search finds the reply, and one replies read brings the parent and every new
// reply with it.
func TestInboxFindsANewReplyUnderAnOldThread(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000200", thread: oldTS, user: inboxBob, text: "answered"}}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(
			msg{ts: oldTS, thread: oldTS, user: inboxMe, text: "a question from last month"},
			msg{ts: "1700040000.000100", thread: oldTS, user: inboxCarol, text: "first"},
			msg{ts: "1700050000.000200", thread: oldTS, user: inboxBob, text: "answered"},
		))},
	})

	got, stderr, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	e.checkInbox(got, 60)
	if !got.Complete {
		t.Fatalf("complete = false: %+v", got)
	}
	c := onlyConversation(t, got)
	if c.Channel.ID != general || c.Channel.Name != "general" || c.Kind != render.ConversationChannel {
		t.Fatalf("conversation = %+v", c)
	}
	if len(c.Messages) != 0 || len(c.Threads) != 1 {
		t.Fatalf("messages = %v, threads = %d, want none and one", tsList(c.Messages), len(c.Threads))
	}
	th := c.Threads[0]
	if th.ThreadTS != oldTS || th.Parent == nil || th.Parent.TS != oldTS || th.Source != render.SourceReplies || !th.Complete {
		t.Fatalf("thread = %+v", th)
	}
	if want := []string{"1700040000.000100", "1700050000.000200"}; !slices.Equal(tsList(th.Replies), want) {
		t.Fatalf("replies = %v, want %v", tsList(th.Replies), want)
	}
	reads := e.requestsTo(methodReplies)
	if len(reads) != 1 {
		t.Fatalf("replies requests = %d, want 1", len(reads))
	}
	q := reads[0].Query
	if q.Get("channel") != general || q.Get("ts") != oldTS || q.Get("oldest") != sinceTS || q.Get("latest") != untilTS {
		t.Fatalf("replies request = %v", q)
	}
}

// Each search asks for one window, newest first, a hundred at a time.
func TestInboxSearchQueries(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	want := []string{
		"is:dm after:2023-11-12", "<@" + inboxMe + "> after:2023-11-12", "is:thread with:me after:2023-11-12",
		"<@" + inboxMe + "> is:thread after:2023-11-05 before:2023-11-16",
	}
	reqs := e.srv.Requests()
	queries := make([]string, 0, len(reqs))
	for _, r := range reqs {
		q := r.Query
		queries = append(queries, q.Get("query"))
		if q.Get("count") != "100" || q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" || q.Get("page") != "1" {
			t.Errorf("search request = %v", q)
		}
	}
	if !slices.Equal(queries, want) {
		t.Fatalf("queries = %q, want %q", queries, want)
	}
	if len(got.Sources) != 4 || got.Sources[0].Query != want[0] || !got.Sources[3].Complete {
		t.Fatalf("sources = %+v", got.Sources)
	}
	if got.Since != "2023-11-14T22:13:20Z" || got.Until != "2023-11-15T22:13:20Z" || got.UserID != inboxMe {
		t.Fatalf("since/until/user = %s %s %s", got.Since, got.Until, got.UserID)
	}
	if got.Conversations == nil || len(got.Conversations) != 0 {
		t.Fatalf("conversations = %#v, want an empty list", got.Conversations)
	}
}

// after: is exclusive and read in the user's zone, so the date is the UTC date
// of since less two days, whichever side of midnight since falls on.
func TestInboxAfterDateAroundMidnight(t *testing.T) {
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2023, 11, 15, 0, 0, 30, 0, time.UTC), "after:2023-11-12"},
		{time.Date(2023, 11, 14, 23, 59, 30, 0, time.UTC), "after:2023-11-11"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			e.serve(slackFake{})
			_, _, code := e.inboxAt(&fakeClock{now: tc.now}, "--json")
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			if q := e.query(0).Get("query"); q != "is:dm "+tc.want {
				t.Fatalf("query = %q, want is:dm %s", q, tc.want)
			}
		})
	}
}

// A DM nobody has opened since the cache was filled is still found: discovery
// is a search, and neither the conversation list nor a history read is used.
func TestInboxFindsADMWithoutTheConversationCache(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{dms: [][]hit{{
		{kind: render.ConversationDM, ch: dmBob, chName: inboxBob, ts: "1700045000.000100", user: inboxMe, text: "pong"},
		{kind: render.ConversationDM, ch: dmBob, chName: inboxBob, ts: "1700040000.000100", user: inboxBob, text: "ping"},
	}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	c := onlyConversation(t, got)
	if c.Channel.ID != dmBob || c.Kind != render.ConversationDM || c.User != inboxBob || c.Source != render.SourceSearch || !c.Complete {
		t.Fatalf("conversation = %+v", c)
	}
	if want := []string{"1700040000.000100", "1700045000.000100"}; !slices.Equal(tsList(c.Messages), want) {
		t.Fatalf("messages = %v, want %v, oldest first", tsList(c.Messages), want)
	}
	if c.Messages[0].Author != "bob" || c.Messages[0].Text != "ping" {
		t.Fatalf("message = %+v", c.Messages[0])
	}
}

// What only I wrote is not news, and costs no read.
func TestInboxDropsWhatOnlyIWrote(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		dms:     [][]hit{{{kind: render.ConversationDM, ch: dmBob, ts: "1700045000.000100", user: inboxMe, text: "note to self"}}},
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700046000.000100", thread: oldTS, user: inboxMe, text: "bump"}}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	if len(got.Conversations) != 0 {
		t.Fatalf("conversations = %+v, want none", got.Conversations)
	}
	if n := e.count(methodReplies); n != 0 {
		t.Fatalf("replies reads = %d, want 0", n)
	}
}

// A thread read can show that everything new in it is mine, though search said
// otherwise; the rule runs again on what was read.
func TestInboxDropsAThreadWhoseReadIsAllMine(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700046000.000100", thread: oldTS, user: inboxBob, text: "edited away"}}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(
			msg{ts: oldTS, thread: oldTS, user: inboxBob, text: "old"},
			msg{ts: "1700047000.000100", thread: oldTS, user: inboxMe, text: "mine"},
		))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(got.Conversations) != 0 {
		t.Fatalf("conversations = %+v, want none", got.Conversations)
	}
}

// A mention outside any thread is shown as search found it: there is nothing
// more to read.
func TestInboxShowsAMentionWithoutAnotherCall(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{{
		{kind: "channel", ch: random, chName: "random", ts: "1700030000.000100", user: inboxBob, text: "<@" + inboxMe + "> look"},
	}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	c := onlyConversation(t, got)
	if c.Kind != render.ConversationChannel || c.Source != render.SourceSearch || !c.Complete || len(c.Messages) != 1 {
		t.Fatalf("conversation = %+v", c)
	}
	m := c.Messages[0]
	if !m.MentionsMe || m.TextRendered != "@me look" {
		t.Fatalf("message = %+v", m)
	}
	if want := []string{"search.messages", "search.messages", "search.messages", "search.messages"}; !slices.Equal(e.endpoints(), want) {
		t.Fatalf("requests = %v, want %v", e.endpoints(), want)
	}
}

// mentions_me names the reader exactly: in the text, in an attachment, or in a
// user element of a block, and nowhere else.
func TestInboxMentionsMe(t *testing.T) {
	longer := inboxMe + "9"
	for _, tc := range []struct {
		name, text, attachments, blocks string
		want                            bool
	}{
		{"text", "<@" + inboxMe + "> hi", "", "", true},
		{"text with label", "<@" + inboxMe + "|me> hi", "", "", true},
		{"attachment text", "build", `[{"text":"owner <@` + inboxMe + `>"}]`, "", true},
		{"attachment pretext", "build", `[{"pretext":"<@` + inboxMe + `>"}]`, "", true},
		{"attachment fallback", "build", `[{"fallback":"<@` + inboxMe + `>"}]`, "", true},
		{"attachment title", "build", `[{"title":"<@` + inboxMe + `>"}]`, "", true},
		{"attachment field", "build", `[{"fields":[{"title":"t","value":"<@` + inboxMe + `>"}]}]`, "", true},
		{"block", "hi", "", `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"` + inboxMe + `"}]}]}]`, true},
		{"another user", "<@" + inboxBob + "> hi", "", "", false},
		{"a longer id", "<@" + longer + "> hi", "", "", false},
		{"id in unrelated json", "hi", `[{"id":"` + inboxMe + `","footer":"` + inboxMe + `"}]`, `[{"type":"section","block_id":"` + inboxMe + `"}]`, false},
		{"blocks that are a string", "hi", "", `"<@` + inboxMe + `>"`, false},
		{"a user element of the wrong shape", "hi", "", `[{"type":"user","user_id":["` + inboxMe + `"]}]`, false},
		{"bare id in text", "ask " + inboxMe, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			h := hit{kind: "channel", ch: random, chName: "random", ts: "1700030000.000100", user: inboxBob, text: tc.text, attachments: tc.attachments}
			raw := h.json()
			if tc.blocks != "" {
				raw = strings.Replace(raw, `"blocks":[]`, `"blocks":`+tc.blocks, 1)
			}
			e.srv.ReplyFunc("search.messages", func(w http.ResponseWriter, r *http.Request) {
				matches := ""
				if sourceOf(r.URL.Query().Get("query")) == render.SearchDMs {
					matches = raw
				}
				_, _ = io.WriteString(w, `{"ok":true,"messages":{"matches":[`+matches+`],"paging":{"page":1,"pages":1}}}`)
			})
			got, _, code := e.inbox()
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			c := onlyConversation(t, got)
			if got := c.Messages[0].MentionsMe; got != tc.want {
				t.Fatalf("mentions_me = %v, want %v", got, tc.want)
			}
		})
	}
}

// The same message found by two searches is listed once, and a thread found by
// two searches is read once.
func TestInboxListsWhatTwoSourcesFoundOnce(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	mention := hit{kind: render.ConversationDM, ch: dmBob, chName: inboxBob, ts: "1700030000.000100", user: inboxBob, text: "<@" + inboxMe + "> hi"}
	reply := hit{kind: "channel", ch: general, chName: "general", ts: "1700031000.000100", thread: oldTS, user: inboxBob, text: "<@" + inboxMe + "> in a thread"}
	e.serve(slackFake{
		dms:      [][]hit{{mention}},
		mentions: [][]hit{{reply, mention}},
		threads:  [][]hit{{reply}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "parent"},
			msg{ts: "1700031000.000100", thread: oldTS, user: inboxBob, text: "<@" + inboxMe + "> in a thread"},
		))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	if len(got.Conversations) != 2 {
		t.Fatalf("conversations = %d, want 2", len(got.Conversations))
	}
	total := 0
	for _, c := range got.Conversations {
		total += len(c.Messages)
		for _, th := range c.Threads {
			total += len(th.Replies)
		}
	}
	if total != 2 {
		t.Fatalf("listed %d messages, want 2", total)
	}
	if n := e.count(methodReplies); n != 1 {
		t.Fatalf("replies reads = %d, want 1", n)
	}
}

// A conversation's kind comes from the hit's own flags, so it needs no list.
func TestInboxToldConversationKindsApart(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		dms: [][]hit{{
			{kind: render.ConversationGroupDM, ch: groupDM, chName: "mpdm-me--bob--carol-1", ts: "1700060000.000100", user: inboxCarol, text: "group"},
			{kind: render.ConversationDM, ch: dmBob, chName: inboxBob, ts: "1700080000.000100", user: inboxBob, text: "direct"},
		}},
		mentions: [][]hit{{
			{kind: "channel", ch: general, chName: "general", ts: "1700070000.000100", user: inboxCarol, text: "<@" + inboxMe + ">"},
			{kind: "none", ch: random, chName: "random", ts: "1700050000.000100", user: inboxCarol, text: "<@" + inboxMe + ">"},
		}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	order := make([]string, 0, len(got.Conversations))
	kinds := make([]string, 0, len(got.Conversations))
	for _, c := range got.Conversations {
		order = append(order, c.Channel.ID)
		kinds = append(kinds, c.Kind)
	}
	// Newest activity first.
	if want := []string{dmBob, general, groupDM, random}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if want := []string{"dm", "channel", "group_dm", "channel"}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if g := got.Conversations[2].Channel; g.Name != "mpdm-me--bob--carol-1" {
		t.Fatalf("group dm channel = %+v", g)
	}
	if d := got.Conversations[0]; d.Channel.Name != "" || d.User != inboxBob {
		t.Fatalf("dm = %+v; a DM has no name, and its user is the other person", d)
	}
}

// A DM where the only other writer is an app has no person to name.
func TestInboxDMWithAnAppHasNoUser(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{dms: [][]hit{{{kind: render.ConversationDM, ch: dmApp, ts: "1700040000.000100", user: "", text: "build passed"}}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if c := onlyConversation(t, got); c.User != "" {
		t.Fatalf("user = %q, want none", c.User)
	}
	stdout, _, _ := e.inboxAt(newClock())
	if !strings.HasPrefix(stdout, dmApp+"  (dm)\n") {
		t.Fatalf("stdout = %q, want the channel id as the header", stdout)
	}
}

// A 1:1 DM names the other person under "name", as `hear dms --json` does,
// with the label their messages' author and the text header carry. A person
// the user list does not know has an id and no name: the key is left out, not
// set empty.
func TestInboxDMNamesTheOtherPerson(t *testing.T) {
	const stranger, dmStranger = "U0000000009", "D0000000009"
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{dms: [][]hit{{
		{kind: render.ConversationDM, ch: dmBob, ts: "1700080000.000100", user: inboxBob, text: "known"},
		{kind: render.ConversationDM, ch: dmStranger, ts: "1700070000.000100", user: stranger, text: "unknown"},
	}}})

	stdout, _, code := e.inboxAt(newClock(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got render.Inbox
	decode(t, stdout, &got)
	if len(got.Conversations) != 2 {
		t.Fatalf("conversations = %+v, want two", got.Conversations)
	}
	known, unknown := got.Conversations[0], got.Conversations[1]
	if known.User != inboxBob || known.Name != "bob" || len(known.Messages) != 1 || known.Messages[0].Author != known.Name {
		t.Fatalf("known dm = %+v, want user %s named bob, as its author is", known, inboxBob)
	}
	if unknown.User != stranger {
		t.Fatalf("unknown dm user = %q, want %s", unknown.User, stranger)
	}
	var raw struct {
		Conversations []map[string]any `json:"conversations"`
	}
	decode(t, stdout, &raw)
	if name, ok := raw.Conversations[1]["name"]; ok {
		t.Fatalf("unknown dm name = %#v, want the key left out", name)
	}

	text, _, _ := e.inboxAt(newClock())
	if !strings.HasPrefix(text, "@bob  (dm)\n") || !strings.Contains(text, "\n@"+stranger+"  (dm)\n") {
		t.Fatalf("text = %q, want the same label in the headers", text)
	}
}

// Inside a conversation, messages and threads are oldest first, and a parent
// posted inside the window is in its thread and nowhere else.
func TestInboxOrdersInsideAConversation(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	parentTS := newParentTS
	e.serve(slackFake{
		dms: [][]hit{{
			{kind: render.ConversationDM, ch: dmBob, ts: "1700070000.000100", user: inboxBob, text: "later"},
			{kind: render.ConversationDM, ch: dmBob, ts: "1700025000.000100", thread: parentTS, user: inboxBob, text: "reply"},
			{kind: render.ConversationDM, ch: dmBob, ts: parentTS, thread: parentTS, user: inboxBob, text: "parent"},
			{kind: render.ConversationDM, ch: dmBob, ts: "1700010000.000100", user: inboxBob, text: "earlier"},
		}},
		replies: map[string]http.HandlerFunc{parentTS: body(repliesBody(
			msg{ts: parentTS, thread: parentTS, user: inboxBob, text: "parent"},
			msg{ts: "1700025000.000100", thread: parentTS, user: inboxBob, text: "reply"},
		))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	c := onlyConversation(t, got)
	if want := []string{"1700010000.000100", "1700070000.000100"}; !slices.Equal(tsList(c.Messages), want) {
		t.Fatalf("messages = %v, want %v", tsList(c.Messages), want)
	}
	if len(c.Threads) != 1 || c.Threads[0].Parent == nil || c.Threads[0].Parent.TS != parentTS ||
		!slices.Equal(tsList(c.Threads[0].Replies), []string{"1700025000.000100"}) {
		t.Fatalf("threads = %+v", c.Threads)
	}
	stdout, _, _ := e.inboxAt(newClock())
	lines := strings.Split(stdout, "\n")
	if len(lines) < 5 || !strings.Contains(lines[1], "earlier") || !strings.HasPrefix(lines[2], "thread ") ||
		!strings.Contains(lines[3], "reply") || !strings.Contains(lines[4], "later") {
		t.Fatalf("text order wrong:\n%s", stdout)
	}
}

// The window is since < ts < until, compared as timestamps.
func TestInboxWindowBoundaries(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	at := func(ts string) hit {
		return hit{kind: "channel", ch: random, chName: "random", ts: ts, user: inboxBob, text: "<@" + inboxMe + ">"}
	}
	e.serve(slackFake{mentions: [][]hit{{
		at(untilTS), at("1700086399.999999"), at("1700000000.000001"), at(sinceTS),
	}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	c := onlyConversation(t, got)
	if want := []string{"1700000000.000001", "1700086399.999999"}; !slices.Equal(tsList(c.Messages), want) {
		t.Fatalf("messages = %v, want %v", tsList(c.Messages), want)
	}
}

// A search pages until it reaches the start of the window, and no further.
func TestInboxSearchPagesToTheStartOfTheWindow(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	m := func(ts string) hit {
		return hit{kind: "channel", ch: random, chName: "random", ts: ts, user: inboxBob, text: "<@" + inboxMe + ">"}
	}
	e.serve(slackFake{mentions: [][]hit{
		{m("1700080000.000100"), m("1700070000.000100")},
		{m("1700060000.000100"), m("1699999000.000100")},
		{m("1699990000.000100")},
	}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	var pages []string
	for _, r := range e.srv.Requests() {
		if sourceOf(r.Query.Get("query")) == render.SearchMentions {
			pages = append(pages, r.Query.Get("page"))
		}
	}
	if !slices.Equal(pages, []string{"1", "2"}) {
		t.Fatalf("mention pages read = %v, want 1 and 2", pages)
	}
	if s := got.Sources[1]; !s.Complete || s.Hits != 3 {
		t.Fatalf("mentions source = %+v", s)
	}
}

// A search that would page on past ten pages stops and says so.
func TestInboxSearchStopsAtThePageLimit(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	pages := make([][]hit, 20)
	for i := range pages {
		pages[i] = []hit{{kind: "channel", ch: random, chName: "random", ts: fmt.Sprintf("17000800%02d.000100", i), user: inboxMe, text: "<@" + inboxMe + ">"}}
	}
	e.serve(slackFake{mentions: pages})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	if s := got.Sources[1]; s.Complete || s.Reason != "page limit" {
		t.Fatalf("mentions source = %+v", s)
	}
	if got.Complete {
		t.Fatal("complete = true with a source cut short")
	}
	if n := e.count("search.messages"); n != 13 {
		t.Fatalf("search requests = %d, want 10 for mentions and 1 each for the others", n)
	}
}

// A thread hit whose permalink names no thread cannot be read as one: it is
// kept as a message, and the source says it could not place it.
func TestInboxThreadHitWithoutAThread(t *testing.T) {
	for _, link := range []string{"-", "https://example.slack.test/archives/" + general + "/p1700040000000100?thread_ts=bad", "not a link"} {
		t.Run(link, func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			e.serve(slackFake{threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700040000.000100", user: inboxBob, text: "x", link: link}}}})

			got, _, code := e.inbox()
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			c := onlyConversation(t, got)
			if len(c.Messages) != 1 || len(c.Threads) != 0 {
				t.Fatalf("conversation = %+v", c)
			}
			if s := got.Sources[2]; s.Complete || s.Reason != "hit without a thread" {
				t.Fatalf("threads source = %+v", s)
			}
		})
	}
}

// A thread read keeps the parent once however often Slack sends it, routes a
// broadcast to its thread, and pages.
func TestInboxThreadReadPagesAndPlacesEveryMessage(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	pages := []string{
		`{"ok":true,"has_more":true,"response_metadata":{"next_cursor":"c2"},"messages":[` +
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "parent"}.json() + `,` +
			msg{ts: "1700010000.000100", thread: oldTS, user: inboxBob, text: "one", subtype: "thread_broadcast"}.json() + `]}`,
		`{"ok":true,"has_more":false,"messages":[` +
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "parent"}.json() + `,` +
			msg{ts: "1700020000.000100", thread: oldTS, user: inboxBob, text: "two"}.json() + `]}`,
	}
	n := 0
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700020000.000100", thread: oldTS, user: inboxBob, text: "two"}}},
		replies: map[string]http.HandlerFunc{oldTS: func(w http.ResponseWriter, r *http.Request) {
			if n == 1 && r.URL.Query().Get("cursor") != "c2" {
				t.Errorf("page 2 cursor = %q", r.URL.Query().Get("cursor"))
			}
			_, _ = io.WriteString(w, pages[min(n, 1)])
			n++
		}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	c := onlyConversation(t, got)
	if len(c.Messages) != 0 || len(c.Threads) != 1 {
		t.Fatalf("conversation = %+v", c)
	}
	th := c.Threads[0]
	if th.Parent == nil || th.Parent.TS != oldTS || !th.Complete ||
		!slices.Equal(tsList(th.Replies), []string{"1700010000.000100", "1700020000.000100"}) {
		t.Fatalf("thread = %+v", th)
	}
}

// Slack may leave the parent out; the thread is still shown, saying so.
func TestInboxThreadWithoutItsParent(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700020000.000100", thread: oldTS, user: inboxBob, text: "two"}}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(msg{ts: "1700020000.000100", thread: oldTS, user: inboxBob, text: "two"}))},
	})

	stdout, _, code := e.inboxAt(newClock(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, `"parent": null`) {
		t.Fatalf("stdout does not carry a null parent:\n%s", stdout)
	}
	text, _, _ := e.inboxAt(newClock())
	if !strings.Contains(text, "(parent not returned)") {
		t.Fatalf("text = %q", text)
	}
}

// A read that fails part way keeps what it got and the search hits, and says
// why it stopped.
func TestInboxThreadReadFailingOnPage2KeepsPage1(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	n := 0
	e.serve(slackFake{
		threads: [][]hit{{
			{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "from search"},
		}},
		replies: map[string]http.HandlerFunc{oldTS: func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"response_metadata":{"next_cursor":"c2"},"messages":[`+
					msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "parent"}.json()+`,`+
					msg{ts: "1700010000.000100", thread: oldTS, user: inboxBob, text: "from page 1"}.json()+`]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":false,"error":"internal_error"}`)
		}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	th := onlyConversation(t, got).Threads[0]
	if th.Complete || !strings.Contains(th.Reason, "internal_error") || th.Parent == nil {
		t.Fatalf("thread = %+v", th)
	}
	if want := []string{"1700010000.000100", "1700050000.000100"}; !slices.Equal(tsList(th.Replies), want) {
		t.Fatalf("replies = %v, want %v", tsList(th.Replies), want)
	}
	if got.Complete {
		t.Fatal("complete = true with a thread cut short")
	}
}

// Identity is read from the cache when it belongs to this credential, and
// fetched and written through when it does not.
func TestInboxIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached *cache.Identity
		fetch  bool
	}{
		{"warm", &cache.Identity{UserID: inboxMe, CredentialKey: cred.KeyFor("xoxp-fake-read-token")}, false},
		{"cold", nil, true},
		{"another credential", &cache.Identity{UserID: inboxBob, CredentialKey: "0000000000000000"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			_ = cache.Clear()
			err := cache.Write(cache.UsersFile, cache.Users{Members: inboxUsers})
			if err != nil {
				t.Fatal(err)
			}
			if tc.cached != nil {
				err = cache.Write(cache.IdentityFile, *tc.cached)
				if err != nil {
					t.Fatal(err)
				}
			}
			e.srv.Reply("auth.test", `{"ok":true,"user":"me","team":"example","user_id":"`+inboxMe+`"}`)
			e.serve(slackFake{})

			got, stderr, code := e.inbox()
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			e.checkInbox(got, 60)
			if fetched := e.count("auth.test") == 1; fetched != tc.fetch {
				t.Fatalf("auth.test called: %v, want %v", fetched, tc.fetch)
			}
			if got.UserID != inboxMe {
				t.Fatalf("user_id = %q", got.UserID)
			}
			var id cache.Identity
			err = cache.Read(cache.IdentityFile, &id)
			if err != nil || id.UserID != inboxMe || id.CredentialKey != cred.KeyFor("xoxp-fake-read-token") {
				t.Fatalf("identity on disk = %+v (%v)", id, err)
			}
		})
	}
}

// Without an identity no query can be written, so a failed auth.test ends the
// run with nothing on stdout.
func TestInboxFailsWithoutAnIdentity(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.UsersFile, cache.Users{Members: inboxUsers})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Reply("auth.test", `{"ok":false,"error":"invalid_auth"}`)

	stdout, stderr, code := e.inboxAt(newClock(), "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid_auth") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

// Cache fills are counted apart from the budget: a cold user list fills even
// under the smallest budget, and the run goes on.
func TestInboxCacheFillsAreNotCharged(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	err := cache.Write(cache.UsersFile, cache.Users{})
	if err != nil {
		t.Fatal(err)
	}
	_ = cache.Clear()
	e.warmIdentityOnly()
	e.srv.Pages("users.list",
		`{"ok":true,"members":[{"id":"`+inboxMe+`","name":"me"}],"response_metadata":{"next_cursor":"p2"}}`,
		`{"ok":true,"members":[{"id":"`+inboxBob+`","name":"bob"}]}`)
	e.serve(slackFake{})

	got, stderr, code := e.inbox("--max-calls", "1")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	e.checkInbox(got, 1)
	if got.Calls.Cache != 2 || got.Calls.Used != 1 {
		t.Fatalf("calls = %+v, want 2 cache and 1 used", got.Calls)
	}
	if s := got.Sources; s[0].Complete != true || s[1].Reason != reasonBudget || s[2].Reason != reasonBudget {
		t.Fatalf("sources = %+v", s)
	}
}

func (e *env) warmIdentityOnly() {
	e.t.Helper()
	err := cache.Write(cache.IdentityFile, cache.Identity{UserID: inboxMe, CredentialKey: cred.KeyFor("xoxp-fake-read-token")})
	if err != nil {
		e.t.Fatal(err)
	}
}

// When the budget runs out the threads it could not read stay as search hits,
// named as incomplete; nothing found is dropped.
func TestInboxBudgetLeavesUnreadThreadsAsSearchHits(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	threads := []string{"1699000000.000100", "1699000001.000100", "1699000002.000100"}
	hits := make([]hit, 0, len(threads))
	replies := map[string]http.HandlerFunc{}
	for i, ts := range threads {
		reply := fmt.Sprintf("17000%d0000.000100", i+1)
		hits = append(hits, hit{kind: "channel", ch: general, chName: "general", ts: reply, thread: ts, user: inboxBob, text: "r"})
		replies[ts] = body(repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}, msg{ts: reply, thread: ts, user: inboxBob, text: "r"}))
	}
	e.serve(slackFake{threads: [][]hit{hits}, replies: replies})

	got, _, code := e.inbox("--max-calls", "5")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 5)
	if got.Complete {
		t.Fatal("complete = true with threads unread")
	}
	c := onlyConversation(t, got)
	var read, unread int
	for _, th := range c.Threads {
		switch {
		case th.Complete && th.Source == render.SourceReplies:
			read++
		case !th.Complete && th.Source == render.SourceSearch && th.Reason == reasonBudget && len(th.Replies) == 1:
			unread++
		default:
			t.Errorf("thread = %+v", th)
		}
	}
	if read != 1 || unread != 2 {
		t.Fatalf("read %d, unread %d, want 1 and 2", read, unread)
	}
	// The newest thread is read first.
	if !c.Threads[2].Complete {
		t.Fatalf("the newest thread was not the one read: %+v", c.Threads)
	}
	text, _, _ := e.inboxAt(newClock(), "--max-calls", "5")
	if !strings.Contains(text, "incomplete:\n") || !strings.Contains(text, "thread #general "+threads[0]+": call budget") {
		t.Fatalf("text does not name what is incomplete:\n%s", text)
	}
}

// limitedOnce answers 429 for the first n requests, then with ok.
func limitedThen(retryAfter string, n int, ok string) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= n {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		_, _ = io.WriteString(w, ok)
	}
}

func threeOldThreads() ([]hit, []string) {
	threads := []string{"1699000000.000100", "1699000001.000100", "1699000002.000100"}
	hits := make([]hit, 0, len(threads))
	for i, ts := range threads {
		hits = append(hits, hit{kind: "channel", ch: general, chName: "general", ts: fmt.Sprintf("17000%d0000.000100", 3-i), thread: ts, user: inboxBob, text: "r"})
	}
	return hits, threads
}

// A 429 is waited out once, for as long as Slack asked, and the same request is
// sent again.
func TestInboxWaitsOnceForRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}}},
		replies: map[string]http.HandlerFunc{oldTS: limitedThen("2", 1, repliesBody(
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "p"},
			msg{ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"},
		))},
	})
	clk := newClock()
	stdout, _, code := e.inboxAt(clk, "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got render.Inbox
	decode(t, stdout, &got)
	e.checkInbox(got, 60)
	if !slices.Equal(clk.slept, []time.Duration{2 * time.Second}) {
		t.Fatalf("slept %v, want one 2s wait", clk.slept)
	}
	if !got.Complete || e.count(methodReplies) != 2 {
		t.Fatalf("complete = %v after %d replies requests", got.Complete, e.count(methodReplies))
	}
	reads := e.requestsTo(methodReplies)
	if a, b := reads[0].Query.Encode(), reads[1].Query.Encode(); a != b {
		t.Fatalf("the retry was not the same request: %s vs %s", a, b)
	}
}

// A second 429 blocks the method: nothing more is asked of it this run.
func TestInboxSecond429BlocksTheMethod(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	hits, threads := threeOldThreads()
	always := limitedThen("1", 1000, "")
	e.serve(slackFake{threads: [][]hit{hits}, replies: map[string]http.HandlerFunc{threads[0]: always, threads[1]: always, threads[2]: always}})
	clk := newClock()
	stdout, _, code := e.inboxAt(clk, "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got render.Inbox
	decode(t, stdout, &got)
	if n := e.count(methodReplies); n != 2 {
		t.Fatalf("replies requests = %d, want 2", n)
	}
	for _, th := range onlyConversation(t, got).Threads {
		if th.Complete || th.Reason != "rate limited" {
			t.Fatalf("thread = %+v", th)
		}
	}
	if len(clk.slept) != 1 {
		t.Fatalf("slept %v, want one wait", clk.slept)
	}
}

// A wait longer than the cap is not waited for; the method is blocked at once.
func TestInboxDoesNotWaitForALongRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	hits, threads := threeOldThreads()
	h := limitedThen("120", 1000, "")
	e.serve(slackFake{threads: [][]hit{hits[:1]}, replies: map[string]http.HandlerFunc{threads[0]: h}})
	clk := newClock()
	_, _, code := e.inboxAt(clk, "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(clk.slept) != 0 || e.count(methodReplies) != 1 {
		t.Fatalf("slept %v after %d requests, want no wait and one request", clk.slept, e.count(methodReplies))
	}
}

// The total wait in a run is capped: two waits of 30s fit, a third does not.
func TestInboxTotalWaitIsCapped(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	hits, threads := threeOldThreads()
	replies := map[string]http.HandlerFunc{}
	for _, ts := range threads {
		replies[ts] = limitedThen("30", 1, repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}))
	}
	e.serve(slackFake{threads: [][]hit{hits}, replies: replies})
	clk := newClock()
	stdout, _, code := e.inboxAt(clk, "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got render.Inbox
	decode(t, stdout, &got)
	if !slices.Equal(clk.slept, []time.Duration{30 * time.Second, 30 * time.Second}) {
		t.Fatalf("slept %v, want two 30s waits", clk.slept)
	}
	limited := 0
	for _, th := range onlyConversation(t, got).Threads {
		if th.Reason == "rate limited" {
			limited++
		}
	}
	if limited != 1 {
		t.Fatalf("rate-limited threads = %d, want 1", limited)
	}
}

// A 429 while filling the user list is not retried: it fails the run, as it
// fails every other command.
func TestInbox429OnACacheFillIsFatal(t *testing.T) {
	e := newEnv(t)
	e.warmIdentityOnly()
	e.srv.ReplyFunc("users.list", limitedThen("1", 1000, ""))

	stdout, stderr, code := e.inboxAt(newClock(), "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "ratelimited") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

// An auth error means the credential is no good for anything that follows, so
// it ends the run wherever it comes from.
func TestInboxAuthErrorsAreFatal(t *testing.T) {
	for _, code := range []string{"invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive"} {
		for _, where := range []string{"search", "replies"} {
			t.Run(code+" from "+where, func(t *testing.T) {
				e := newEnv(t)
				e.warmInbox()
				refusal := `{"ok":false,"error":"` + code + `"}`
				e.serve(slackFake{
					threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}}},
					replies: map[string]http.HandlerFunc{oldTS: body(refusal)},
				})
				if where == "search" {
					e.srv.Reply("search.messages", refusal)
				}
				stdout, stderr, exit := e.inboxAt(newClock(), "--json")
				if exit != 1 || stdout != "" || !strings.Contains(stderr, code) {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q", exit, stdout, stderr)
				}
			})
		}
	}
}

// Any other refusal is confined to what it refused.
func TestInboxOtherErrorsAreConfined(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	hits, threads := threeOldThreads()
	f := slackFake{threads: [][]hit{hits[:2]}, replies: map[string]http.HandlerFunc{
		threads[0]: body(`{"ok":false,"error":"channel_not_found"}`),
		threads[1]: body(repliesBody(msg{ts: threads[1], thread: threads[1], user: inboxCarol, text: "p"}, msg{ts: hits[1].ts, thread: threads[1], user: inboxBob, text: "r"})),
	}}
	e.serve(f)
	e.srv.ReplyFunc("search.messages", func(w http.ResponseWriter, r *http.Request) {
		if sourceOf(r.URL.Query().Get("query")) == render.SearchMentions {
			_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
			return
		}
		pages := f.threads
		if src := sourceOf(r.URL.Query().Get("query")); src == render.SearchDMs || src == render.SearchMentionThreads {
			pages = nil
		}
		_, _ = io.WriteString(w, searchPage(pages, 1))
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources; !s[0].Complete || s[1].Complete || !strings.Contains(s[1].Reason, "missing_scope") || !s[2].Complete {
		t.Fatalf("sources = %+v", s)
	}
	byTS := map[string]render.InboxThread{}
	for _, th := range onlyConversation(t, got).Threads {
		byTS[th.ThreadTS] = th
	}
	if th := byTS[threads[0]]; th.Complete || !strings.Contains(th.Reason, "channel_not_found") {
		t.Fatalf("refused thread = %+v", th)
	}
	if th := byTS[threads[1]]; !th.Complete {
		t.Fatalf("readable thread = %+v", th)
	}
}

// The inbox is printed whether or not it is complete, and exits 0 either way:
// complete says which. Only a run that printed nothing exits 1.
func TestInboxExitsZeroWhenIncomplete(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.srv.Reply("search.messages", `{"ok":false,"error":"missing_scope"}`)

	got, _, code := e.inbox()
	if code != 0 || got.Complete {
		t.Fatalf("exit = %d, complete = %v", code, got.Complete)
	}
}

// Text from Slack cannot drive the terminal, here as anywhere the inbox prints.
func TestInboxTextIsTerminalSafe(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{{
		{kind: "channel", ch: random, chName: "ran\x1bdom", ts: "1700030000.000100", user: inboxBob, text: "<@" + inboxMe + "> \x1b[2Jhi\u202e\x07"},
	}}})

	stdout, _, code := e.inboxAt(newClock())
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(stdout, "\x1b\x07\u202e") {
		t.Fatalf("stdout carries a control character: %q", stdout)
	}
}

// --max-calls is the inbox's alone, and is a positive whole number of at most
// a thousand; inbox takes no argument.
func TestInboxFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"read", "#eng", "--max-calls", "5"}, "--max-calls applies to inbox"},
		{[]string{"search", "x", "--since", "2h"}, "--since applies to read and inbox"},
		{[]string{"inbox", "extra"}, "inbox takes no arguments"},
		{[]string{"inbox", "--max-calls", "0"}, "--max-calls"},
		{[]string{"inbox", "--max-calls", "-1"}, "--max-calls"},
		{[]string{"inbox", "--max-calls", "lots"}, "--max-calls"},
		{[]string{"inbox", "--max-calls", "1001"}, "--max-calls"},
		{[]string{"inbox", "--max-calls"}, "--max-calls needs a number"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			_, stderr, code := e.run(tc.args...)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit = %d, stderr = %q, want 2 and %q", code, stderr, tc.want)
			}
			if e.srv.Hits() != 0 {
				t.Fatalf("Slack received %d requests", e.srv.Hits())
			}
		})
	}
}

// --since sets the window for inbox as for read.
func TestInboxSinceSetsTheWindow(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{})

	got, _, code := e.inbox("--since", "2h")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got.Since != "2023-11-15T20:13:20Z" {
		t.Fatalf("since = %s", got.Since)
	}
	if q := e.query(0).Get("query"); q != "is:dm after:2023-11-13" {
		t.Fatalf("query = %q", q)
	}
}

// The help names the verb and its flag.
func TestInboxIsInTheHelp(t *testing.T) {
	for _, want := range []string{"inbox", "--max-calls"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}

// json.Marshal of the inbox keeps empty lists as lists, so jq never meets null
// where it expects an array.
func TestInboxEmptyListsAreLists(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "p"}, msg{ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}))},
	})

	stdout, _, code := e.inboxAt(newClock(), "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var raw map[string]any
	err := json.Unmarshal([]byte(stdout), &raw)
	if err != nil {
		t.Fatal(err)
	}
	convs, ok := raw["conversations"].([]any)
	if !ok || len(convs) != 1 {
		t.Fatalf("conversations = %#v", raw["conversations"])
	}
	conv, ok := convs[0].(map[string]any)
	if !ok {
		t.Fatalf("conversation = %#v", convs[0])
	}
	if _, ok := conv["messages"].([]any); !ok {
		t.Fatalf("messages = %#v, want []", conv["messages"])
	}
}

// mentionHit is a hit of the mention lookback: a message naming the reader in
// a thread, before the window.
func mentionHit(ts, thread string) hit {
	return hit{kind: "channel", ch: general, chName: "general", ts: ts, thread: thread, user: inboxBob, text: "<@" + inboxMe + "> before"}
}

// with:me does not match a thread the reader was only mentioned in, so the
// week before the window is searched for those mentions and each thread is read
// to see whether anything came after. One with a new reply is shown; one
// without is not.
func TestInboxReadsThreadsIWasMentionedInBeforeTheWindow(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	quiet, busy := olderLookbackTS, lookbackTS
	e.serve(slackFake{
		mentionThreads: [][]hit{{mentionHit("1699710000.000100", busy), mentionHit("1699610000.000100", quiet), mentionHit("1699300000.000100", oldTS)}},
		replies: map[string]http.HandlerFunc{
			busy:  body(repliesBody(msg{ts: busy, thread: busy, user: inboxCarol, text: "p"}, msg{ts: "1700020000.000100", thread: busy, user: inboxCarol, text: "new"})),
			quiet: body(repliesBody(msg{ts: quiet, thread: quiet, user: inboxCarol, text: "p"})),
		},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 60)
	c := onlyConversation(t, got)
	if len(c.Threads) != 1 || c.Threads[0].ThreadTS != busy || !slices.Equal(tsList(c.Threads[0].Replies), []string{"1700020000.000100"}) {
		t.Fatalf("threads = %+v", c.Threads)
	}
	s := got.Sources[3]
	if s.Name != render.SearchMentionThreads || !s.Complete || s.Hits != 2 {
		t.Fatalf("mention_threads source = %+v; the hit before the lookback is outside it", s)
	}
	if n := e.count(methodReplies); n != 2 {
		t.Fatalf("replies reads = %d, want 2", n)
	}
	if !got.Complete {
		t.Fatal("complete = false")
	}
}

// Those reads are the last call on the budget and take at most a quarter of
// it; the threads left unread are counted, so the source is not complete.
func TestInboxMentionLookbackHasAShareOfTheBudget(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	threads := []string{lookbackTS, olderLookbackTS, "1699500000.000100"}
	replies := map[string]http.HandlerFunc{}
	hits := make([]hit, 0, len(threads))
	for _, ts := range threads {
		hits = append(hits, mentionHit(strings.Replace(ts, ".000100", ".000200", 1), ts))
		replies[ts] = body(repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}))
	}
	e.serve(slackFake{mentionThreads: [][]hit{hits}, replies: replies})

	got, _, code := e.inbox("--max-calls", "8")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 8)
	if n := e.count(methodReplies); n != 2 {
		t.Fatalf("replies reads = %d, want 2, a quarter of 8", n)
	}
	if s := got.Sources[3]; s.Complete || s.Reason != "1 of 3 threads not read: share of the call budget" {
		t.Fatalf("mention_threads source = %+v", s)
	}
	if got.Complete {
		t.Fatal("complete = true with a thread unread")
	}
}

// A thread another search already found is not read a second time.
func TestInboxMentionLookbackSkipsKnownThreads(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads:        [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}}},
		mentionThreads: [][]hit{{mentionHit("1699900000.000100", oldTS)}},
		replies: map[string]http.HandlerFunc{oldTS: body(repliesBody(
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "p"},
			msg{ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"},
		))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := e.count(methodReplies); n != 1 {
		t.Fatalf("replies reads = %d, want 1", n)
	}
	if !got.Complete {
		t.Fatalf("complete = false: %+v", got.Sources)
	}
}

// The run's deadline starts once the identity and the user list are in hand.
// Past it nothing more is asked, and every search says why it stopped.
func TestInboxDeadline(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.UsersFile, cache.Users{Members: inboxUsers})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Reply("auth.test", `{"ok":true,"user":"me","team":"example","user_id":"`+inboxMe+`"}`)
	e.serve(slackFake{})

	var o, s bytes.Buffer
	code := runWith([]string{"inbox", "--json"}, &o, &s, testEndpoints(e.srv.URL), timing{clock: newClock(), setup: inboxDeadline, deadline: time.Nanosecond})
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, s.String())
	}
	var got render.Inbox
	decode(t, o.String(), &got)
	if e.count("auth.test") != 1 || e.count("search.messages") != 0 {
		t.Fatalf("requests = %v; setup runs, and nothing after the deadline", e.endpoints())
	}
	for _, src := range got.Sources {
		if src.Complete || src.Reason != reasonDeadline {
			t.Fatalf("source = %+v", src)
		}
	}
}

// A parent at exactly since, or before it, is context: it never makes a thread
// news on its own.
func TestInboxParentAtSinceIsContextNotNews(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: sinceTS, user: inboxBob, text: "x"}}},
		replies: map[string]http.HandlerFunc{sinceTS: body(repliesBody(
			msg{ts: sinceTS, thread: sinceTS, user: inboxBob, text: "parent at since"},
			msg{ts: "1700050000.000100", thread: sinceTS, user: inboxMe, text: "only mine"},
		))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(got.Conversations) != 0 {
		t.Fatalf("conversations = %+v, want none", got.Conversations)
	}
}

// An empty page while Slack says more follow is not the end of the search.
func TestInboxEmptyPageBeforeTheLastIsIncomplete(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{
		{{kind: "channel", ch: random, chName: "random", ts: "1700080000.000100", user: inboxBob, text: "<@" + inboxMe + ">"}},
		{},
		{},
	}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[1]; s.Complete || s.Reason != reasonEmptyPage {
		t.Fatalf("mentions source = %+v", s)
	}
}

// A hit without a timestamp cannot be windowed or placed; the search says so.
func TestInboxHitWithoutATimestamp(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{{
		{kind: "channel", ch: random, chName: "random", ts: "soon", user: inboxBob, text: "<@" + inboxMe + ">"},
		{kind: "channel", ch: random, chName: "random", ts: "1700080000.000100", user: inboxBob, text: "<@" + inboxMe + ">"},
	}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[1]; s.Complete || s.Reason != reasonNoTimestamp || s.Hits != 1 {
		t.Fatalf("mentions source = %+v", s)
	}
	if c := onlyConversation(t, got); len(c.Messages) != 1 {
		t.Fatalf("messages = %v", tsList(c.Messages))
	}
}

// The user id goes into a search query, so one of the wrong shape ends the run
// before any search is made.
func TestInboxRefusesAnIdentityThatIsNotAUserID(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.UsersFile, cache.Users{Members: inboxUsers})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.IdentityFile, cache.Identity{UserID: "U1> OR <@U2", CredentialKey: cred.KeyFor("xoxp-fake-read-token")})
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := e.inboxAt(newClock(), "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not a Slack user id") || e.srv.Hits() != 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, hits = %d", code, stdout, stderr, e.srv.Hits())
	}
}

// --max-calls=N is the same option as --max-calls N.
func TestInboxMaxCallsInline(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{})

	got, _, code := e.inbox("--max-calls=2")
	if code != 0 || got.Calls.Max != 2 || got.Calls.Used != 2 {
		t.Fatalf("exit = %d, calls = %+v", code, got.Calls)
	}
}

// The ceiling counts what the inbox holds, a parent included, and a read that
// would pass it stops and says so; so does a thread at its own cap. What is
// dropped as not news stops counting.
func TestInboxCeilingCountsWhatIsHeld(t *testing.T) {
	t.Parallel()
	r := &inboxRun{
		me: inboxMe, since: sinceTS, until: untilTS, ceiling: 2,
		held: map[string]bool{}, seen: map[string]bool{}, known: map[string]bool{}, convs: map[string]*inboxConv{},
	}
	c := &inboxConv{id: general, kind: render.ConversationChannel, msgs: map[string]slackread.Message{}, threads: map[string]*inboxThread{}}
	r.convs[general] = c
	th := c.thread(oldTS, classOther)
	if stop := r.merge(th, slackread.Message{TS: oldTS, User: inboxBob}); stop != "" {
		t.Fatalf("parent: %q", stop)
	}
	if stop := r.merge(th, slackread.Message{TS: "1700010000.000100", User: inboxMe}); stop != "" {
		t.Fatalf("first reply: %q", stop)
	}
	if stop := r.merge(th, slackread.Message{TS: "1700010000.000100", User: inboxMe}); stop != "" {
		t.Fatalf("the same reply again is not new: %q", stop)
	}
	if stop := r.merge(th, slackread.Message{TS: "1700020000.000100", User: inboxBob}); stop != reasonMessageLimit {
		t.Fatalf("third message: %q, want %q", stop, reasonMessageLimit)
	}
	if len(r.held) != 2 {
		t.Fatalf("held = %d, want 2", len(r.held))
	}
	// Only my reply is new: the thread is not news, and what it held is free.
	r.prune()
	if len(r.held) != 0 {
		t.Fatalf("held after prune = %d, want 0", len(r.held))
	}

	r.ceiling = maxInboxMessages
	full := c.thread("1699000001.000100", classOther)
	for i := range maxThreadMessages {
		full.msgs[fmt.Sprintf("17000%05d.000100", i)] = slackread.Message{}
	}
	if stop := r.merge(full, slackread.Message{TS: "1700080000.000100", User: inboxBob}); stop != reasonMessageLimit {
		t.Fatalf("a thread at its cap: %q, want %q", stop, reasonMessageLimit)
	}
}

// A 429 reaches every verb as the same text, with the wait Slack asked for,
// and nothing but the inbox asks again.
func TestOtherVerbsFailOnceOn429(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		endpoint string
	}{
		{[]string{"read", engID, "5"}, "conversations.history"},
		{[]string{"search", "deploy"}, "search.messages"},
	} {
		t.Run(tc.args[0], func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			e.srv.ReplyFunc(tc.endpoint, limitedThen("7", 1000, ""))

			stdout, stderr, code := e.run(tc.args...)
			if code != 1 || stdout != "" {
				t.Fatalf("exit = %d, stdout = %q", code, stdout)
			}
			if want := tc.endpoint + ": ratelimited (retry after 7s)"; !strings.Contains(stderr, want) {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
			if n := e.count(tc.endpoint); n != 1 {
				t.Fatalf("%s requests = %d, want 1", tc.endpoint, n)
			}
		})
	}
}

// A thread's first message found as a top-level hit is its parent, even when
// the thread is never read: it is listed once.
func TestInboxParentFoundBySearchIsListedOnceWhenTheReadIsRefused(t *testing.T) {
	parentTS := newParentTS
	reply := hit{kind: render.ConversationDM, ch: dmBob, ts: "1700025000.000100", thread: parentTS, user: inboxBob, text: "reply"}
	parent := hit{kind: render.ConversationDM, ch: dmBob, ts: parentTS, user: inboxBob, text: "parent, no thread in its link"}
	for name, page := range map[string][]hit{"reply first": {reply, parent}, "parent first": {parent, reply}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.warmInbox()
			e.serve(slackFake{dms: [][]hit{page}})

			got, _, code := e.inbox("--max-calls", "4")
			if code != 0 {
				t.Fatalf("exit = %d", code)
			}
			c := onlyConversation(t, got)
			if len(c.Messages) != 0 || len(c.Threads) != 1 || c.Threads[0].Parent == nil || c.Threads[0].Parent.TS != parentTS {
				t.Fatalf("conversation = %+v", c)
			}
			if th := c.Threads[0]; th.Complete || th.Reason != reasonBudget {
				t.Fatalf("thread = %+v", th)
			}
		})
	}
}

// A mention-lookback thread read only in part is listed as unread, even when
// what was read holds nothing new: what was not read might.
func TestInboxPartlyReadMentionThreadIsUnread(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	busy := lookbackTS
	n := 0
	e.serve(slackFake{
		mentionThreads: [][]hit{{mentionHit("1699710000.000100", busy)}},
		replies: map[string]http.HandlerFunc{busy: func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"response_metadata":{"next_cursor":"c2"},"messages":[`+
					msg{ts: busy, thread: busy, user: inboxCarol, text: "p"}.json()+`]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":false,"error":"internal_error"}`)
		}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := got.Sources[3]
	if s.Complete || len(s.Unread) != 1 || s.Unread[0].ThreadTS != busy || !strings.Contains(s.Unread[0].Reason, "internal_error") {
		t.Fatalf("mention_threads source = %+v", s)
	}
	if got.Complete {
		t.Fatal("complete = true with a thread read in part")
	}
	text, _, _ := e.inboxAt(newClock())
	if !strings.Contains(text, "thread "+general+" "+busy+": not read to the end, ") {
		t.Fatalf("text does not name the unread thread:\n%s", text)
	}
}

// A lookback thread read in part that held news is listed once, in its
// conversation's form: its unread entry would only say the same thing again.
func TestInboxPartlyReadMentionThreadWithNewsIsListedOnce(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	busy := lookbackTS
	e.serve(slackFake{
		mentionThreads: [][]hit{{mentionHit("1699710000.000100", busy)}},
		replies: map[string]http.HandlerFunc{busy: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "" {
				_, _ = io.WriteString(w, `{"ok":true,"response_metadata":{"next_cursor":"c2"},"messages":[`+
					msg{ts: busy, thread: busy, user: inboxCarol, text: "p"}.json()+","+
					msg{ts: "1700010000.000100", thread: busy, user: inboxCarol, text: "new"}.json()+`]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":false,"error":"internal_error"}`)
		}},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[3]; len(s.Unread) != 1 || s.Unread[0].ThreadTS != busy {
		t.Fatalf("mention_threads source = %+v, want the thread unread", s)
	}
	c := onlyConversation(t, got)
	if len(c.Threads) != 1 || c.Threads[0].Complete || len(c.Threads[0].Replies) != 1 {
		t.Fatalf("threads = %+v, want the thread with its new reply, not complete", c.Threads)
	}
	text, _, _ := e.inboxAt(newClock())
	if strings.Count(text, "  thread #general "+busy+": ") != 1 {
		t.Fatalf("text does not list the thread once in its conversation's form:\n%s", text)
	}
	if strings.Contains(text, "thread "+general+" "+busy+": not read to the end") {
		t.Fatalf("text lists the thread twice:\n%s", text)
	}
}

// A lookback hit that names no thread is a flaw in that search.
func TestInboxMentionLookbackHitWithoutAThread(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	h := mentionHit("1699710000.000100", "")
	e.serve(slackFake{mentionThreads: [][]hit{{h}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[3]; s.Complete || s.Reason != reasonNoThread {
		t.Fatalf("mention_threads source = %+v", s)
	}
}

// The lookback's share is counted in requests, pages and retries included, not
// in threads.
func TestInboxMentionLookbackShareCountsRequests(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	a, b := lookbackTS, olderLookbackTS
	twoPages := func(ts string) http.HandlerFunc {
		n := 0
		return func(w http.ResponseWriter, r *http.Request) {
			n++
			if n == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"response_metadata":{"next_cursor":"c2"},"messages":[`+
					msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}.json()+`]}`)
				return
			}
			_, _ = io.WriteString(w, repliesBody(msg{ts: "1700020000.000100", thread: ts, user: inboxCarol, text: "new"}))
		}
	}
	e.serve(slackFake{
		mentionThreads: [][]hit{{mentionHit("1699710000.000100", a), mentionHit("1699610000.000100", b)}},
		replies:        map[string]http.HandlerFunc{a: twoPages(a), b: twoPages(b)},
	})

	got, _, code := e.inbox("--max-calls", "8")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := e.count(methodReplies); n != 2 {
		t.Fatalf("replies requests = %d, want 2, a quarter of 8", n)
	}
	s := got.Sources[3]
	if s.Complete || len(s.Unread) != 1 || s.Unread[0].ThreadTS != b || s.Unread[0].Reason != reasonProbeShare {
		t.Fatalf("mention_threads source = %+v", s)
	}
}

// The lookback's share is held back from the other reads when it has threads
// to read, so a busy run does not spend it first.
func TestInboxMentionLookbackShareIsReserved(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	hits, threads := threeOldThreads()
	replies := map[string]http.HandlerFunc{}
	for _, ts := range threads {
		replies[ts] = body(repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}, msg{ts: "1700040000.000100", thread: ts, user: inboxBob, text: "r"}))
	}
	probeTS := lookbackTS
	replies[probeTS] = body(repliesBody(msg{ts: probeTS, thread: probeTS, user: inboxCarol, text: "p"}, msg{ts: "1700050000.000100", thread: probeTS, user: inboxCarol, text: "new"}))
	e.serve(slackFake{threads: [][]hit{hits}, mentionThreads: [][]hit{{mentionHit("1699710000.000100", probeTS)}}, replies: replies})

	got, _, code := e.inbox("--max-calls", "6")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	e.checkInbox(got, 6)
	read := map[string]bool{}
	for _, r := range e.requestsTo(methodReplies) {
		read[r.Query.Get("ts")] = true
	}
	if len(read) != 2 || !read[probeTS] {
		t.Fatalf("threads read = %v, want one of the three and the lookback's", read)
	}
}

// A server that repeats one cursor and one page adds no message, so a thread
// read is bounded by pages too.
func TestInboxThreadReadIsBoundedByPages(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{
		threads: [][]hit{{{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}}},
		replies: map[string]http.HandlerFunc{oldTS: body(`{"ok":true,"response_metadata":{"next_cursor":"same"},"messages":[` +
			msg{ts: oldTS, thread: oldTS, user: inboxCarol, text: "p"}.json() + `,` +
			msg{ts: "1700050000.000100", thread: oldTS, user: inboxBob, text: "r"}.json() + `]}`)},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := e.count(methodReplies); n != maxThreadPages {
		t.Fatalf("replies requests = %d, want %d", n, maxThreadPages)
	}
	if th := onlyConversation(t, got).Threads[0]; th.Complete || th.Reason != reasonPageLimit {
		t.Fatalf("thread = %+v", th)
	}
}

// A DM found only by the lookback still says when the DM search stopped early.
func TestInboxDMFoundByTheLookbackCarriesTheDMSearchReason(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	dmThread := lookbackTS
	f := slackFake{
		mentionThreads: [][]hit{{{kind: render.ConversationDM, ch: dmBob, ts: "1699710000.000100", thread: dmThread, user: inboxBob, text: "<@" + inboxMe + ">"}}},
		replies: map[string]http.HandlerFunc{dmThread: body(repliesBody(
			msg{ts: dmThread, thread: dmThread, user: inboxBob, text: "p"},
			msg{ts: "1700050000.000100", thread: dmThread, user: inboxBob, text: "new"},
		))},
	}
	e.serve(f)
	e.srv.ReplyFunc("search.messages", func(w http.ResponseWriter, r *http.Request) {
		switch sourceOf(r.URL.Query().Get("query")) {
		case render.SearchDMs:
			_, _ = io.WriteString(w, `{"ok":false,"error":"internal_error"}`)
		case render.SearchMentionThreads:
			_, _ = io.WriteString(w, searchPage(f.mentionThreads, 1))
		default:
			_, _ = io.WriteString(w, searchPage(nil, 1))
		}
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	c := onlyConversation(t, got)
	if c.Complete || !strings.HasPrefix(c.Reason, "dms search: ") {
		t.Fatalf("conversation = %+v", c)
	}
}

// A whole read that does not return a parent search found drops it: the read
// is the authority on what the thread holds now.
func TestInboxWholeReadDropsAParentItDidNotReturn(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	parentTS := newParentTS
	e.serve(slackFake{
		threads: [][]hit{{
			{kind: "channel", ch: general, chName: "general", ts: "1700030000.000100", thread: parentTS, user: inboxBob, text: "reply"},
			{kind: "channel", ch: general, chName: "general", ts: parentTS, thread: parentTS, user: inboxBob, text: "deleted since"},
		}},
		replies: map[string]http.HandlerFunc{parentTS: body(repliesBody(msg{ts: "1700030000.000100", thread: parentTS, user: inboxBob, text: "reply"}))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if th := onlyConversation(t, got).Threads[0]; th.Parent != nil || !th.Complete {
		t.Fatalf("thread = %+v", th)
	}
}

// A hit whose channel id is not one is skipped, and the search says so.
func TestInboxHitWithoutAChannel(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{{
		{kind: "channel", ch: "not an id", chName: "x", ts: "1700080000.000100", user: inboxBob, text: "<@" + inboxMe + ">"},
	}}})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[1]; s.Complete || s.Reason != reasonNoChannel || s.Hits != 0 {
		t.Fatalf("mentions source = %+v", s)
	}
	if len(got.Conversations) != 0 {
		t.Fatalf("conversations = %+v", got.Conversations)
	}
}

// A 429 that arrives when a stage has spent its part of the budget is not
// retried, but it does not block the method either: the run's own bound, not
// Slack, ruled the retry out, and the lookback's reserved share is still read.
func TestInbox429AtAStageLimitDoesNotBlockTheLookback(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	newer, older := "1699000001.000100", "1699000000.000100"
	quietA, quietB := lookbackTS, olderLookbackTS
	e.serve(slackFake{
		threads: [][]hit{{
			{kind: "channel", ch: general, chName: "general", ts: "1700060000.000100", thread: newer, user: inboxBob, text: "r"},
			{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: older, user: inboxBob, text: "r"},
		}},
		mentionThreads: [][]hit{{mentionHit("1699710000.000100", quietA), mentionHit("1699610000.000100", quietB)}},
		replies: map[string]http.HandlerFunc{
			newer:  body(repliesBody(msg{ts: newer, thread: newer, user: inboxCarol, text: "p"}, msg{ts: "1700060000.000100", thread: newer, user: inboxBob, text: "r"})),
			older:  limitedThen("1", 1000, ""),
			quietA: body(repliesBody(msg{ts: quietA, thread: quietA, user: inboxCarol, text: "p"})),
			quietB: body(repliesBody(msg{ts: quietB, thread: quietB, user: inboxCarol, text: "p"})),
		},
	})

	clk := newClock()
	stdout, _, code := e.inboxAt(clk, "--json", "--max-calls", "8")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got render.Inbox
	decode(t, stdout, &got)
	e.checkInbox(got, 8)
	read := map[string]int{}
	for _, r := range e.requestsTo(methodReplies) {
		read[r.Query.Get("ts")]++
	}
	if read[older] != 1 || read[quietA] != 1 || read[quietB] != 1 || len(clk.slept) != 0 {
		t.Fatalf("reads = %v, slept %v; want one each and no wait", read, clk.slept)
	}
	if s := got.Sources[3]; !s.Complete || len(s.Unread) != 0 {
		t.Fatalf("mention_threads source = %+v", s)
	}
}

// The lookback ends where the window starts: a mention inside the window is the
// mentions search's, and is not read on the lookback's share.
func TestInboxMentionLookbackEndsAtTheWindow(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	atSince := "1699999000.000100"
	e.serve(slackFake{
		mentionThreads: [][]hit{{mentionHit("1700040000.000100", "1700030000.000100"), mentionHit(sinceTS, atSince)}},
		replies:        map[string]http.HandlerFunc{atSince: body(repliesBody(msg{ts: atSince, thread: atSince, user: inboxCarol, text: "p"}))},
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[3]; s.Hits != 1 || !s.Complete {
		t.Fatalf("mention_threads source = %+v, want only the hit at since", s)
	}
	reads := e.requestsTo(methodReplies)
	if len(reads) != 1 || reads[0].Query.Get("ts") != atSince {
		t.Fatalf("replies reads = %d, want one, of the thread at since", len(reads))
	}
}

// Resolving the credential is inside the setup deadline: a keyring that never
// answers ends the run, rather than holding it past every bound it has.
func TestInboxSetupDeadlineCoversTheCredential(t *testing.T) {
	e := newEnv(t)
	release, finished := make(chan struct{}), make(chan struct{})
	old := resolveRead
	resolveRead = func() (cred.Read, error) {
		<-release
		close(finished)
		return cred.Read{}, cred.ErrNotFound
	}
	t.Cleanup(func() {
		close(release)
		<-finished
		resolveRead = old
	})

	var o, s bytes.Buffer
	code := runWith([]string{"inbox"}, &o, &s, testEndpoints(e.srv.URL),
		timing{clock: newClock(), setup: 20 * time.Millisecond, deadline: inboxDeadline})
	if code != 1 || o.Len() != 0 || !strings.Contains(s.String(), "resolving the read credential") || e.srv.Hits() != 0 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, hits = %d", code, o.String(), s.String(), e.srv.Hits())
	}
}

// A window reaching before 1970 starts at 1970: a negative timestamp would
// order after every real one, and the inbox would call nothing complete.
func TestInboxWindowStartsNoEarlierThan1970(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{mentions: [][]hit{{
		{kind: "channel", ch: random, chName: "random", ts: "1700030000.000100", user: inboxBob, text: "<@" + inboxMe + ">"},
	}}})

	got, _, code := e.inbox("--since", "3000w")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got.Since != "1970-01-01T00:00:00Z" {
		t.Fatalf("since = %s", got.Since)
	}
	// The mention lookback starts no earlier either; its search date is the
	// start less the two-day margin every search has.
	if q := got.Sources[3].Query; !strings.Contains(q, " after:1969-12-30 ") {
		t.Fatalf("mention lookback query = %q, want it to start at 1970", q)
	}
	if c := onlyConversation(t, got); len(c.Messages) != 1 {
		t.Fatalf("messages = %v", tsList(c.Messages))
	}
}

// An empty page with no paging block is no evidence the search is done.
func TestInboxEmptyPageWithoutPagingIsIncomplete(t *testing.T) {
	e := newEnv(t)
	e.warmInbox()
	e.serve(slackFake{})
	e.srv.ReplyFunc("search.messages", func(w http.ResponseWriter, r *http.Request) {
		if sourceOf(r.URL.Query().Get("query")) == render.SearchMentions {
			_, _ = io.WriteString(w, `{"ok":true,"messages":{"matches":[]}}`)
			return
		}
		_, _ = io.WriteString(w, searchPage(nil, 1))
	})

	got, _, code := e.inbox()
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if s := got.Sources[1]; s.Complete || s.Reason != reasonEmptyPage {
		t.Fatalf("mentions source = %+v", s)
	}
	if !got.Sources[0].Complete {
		t.Fatalf("dms source = %+v; an empty result with paging is complete", got.Sources[0])
	}
}

// A lookback thread with nothing new is let go as soon as it is read, so it
// holds no room a later one with news needs.
func TestInboxQuietLookbackThreadsFreeTheirRoom(t *testing.T) {
	e := newEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatal(err)
	}
	quietA, quietB, busy := lookbackTS, olderLookbackTS, "1699500000.000100"
	mine := func(ts string) http.HandlerFunc {
		return body(repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}, msg{ts: "1700010000.000100", thread: ts, user: inboxMe, text: "mine"}))
	}
	e.serve(slackFake{replies: map[string]http.HandlerFunc{
		quietA: mine(quietA), quietB: mine(quietB),
		busy: body(repliesBody(msg{ts: busy, thread: busy, user: inboxCarol, text: "p"}, msg{ts: "1700020000.000100", thread: busy, user: inboxCarol, text: "new"})),
	}})
	budget := slackhttp.NewBudget(60)
	hc := slackhttp.New(2 * time.Second)
	hc.Transport = budget.Wrap(hc.Transport)
	r := &inboxRun{
		c: slackread.New(c, hc, e.srv.URL), budget: budget, clock: newClock(), me: inboxMe,
		since: sinceTS, until: untilTS, ceiling: 3, blocked: map[string]bool{},
		held: map[string]bool{}, seen: map[string]bool{}, known: map[string]bool{},
		probes: map[string]probe{}, convs: map[string]*inboxConv{},
		sources: []render.InboxSource{{Name: render.SearchMentionThreads, Complete: true}},
	}
	for _, ts := range []string{quietA, quietB, busy} {
		var m slackread.SearchMatch
		err := json.Unmarshal([]byte(mentionHit("1699400000.000100", ts).json()), &m)
		if err != nil {
			t.Fatal(err)
		}
		r.collectProbe(m, render.SearchMentionThreads)
	}
	if len(r.probeOrder) != 3 {
		t.Fatalf("probes = %v, want three", r.probeOrder)
	}
	err = r.readProbes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s := r.sources[0]; len(s.Unread) != 0 {
		t.Fatalf("unread = %+v; the quiet threads kept their room", s.Unread)
	}
	if conv := r.convs[general]; conv == nil || conv.threads[busy] == nil {
		t.Fatal("the thread with news was not kept")
	}
}

// A thread a read shows is not news is let go before the next read, so it
// holds no room a later thread with news needs. Two threads whose only hit
// from someone else is gone by the time they are read come first; the room
// they held is what the third one needs.
func TestInboxQuietThreadsFreeTheirRoomBeforeTheNextRead(t *testing.T) {
	e := newEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c, err := cred.ResolveRead()
	if err != nil {
		t.Fatal(err)
	}
	quietA, quietB, busy := lookbackTS, olderLookbackTS, oldTS
	gone := func(ts, mine string) http.HandlerFunc {
		return body(repliesBody(msg{ts: ts, thread: ts, user: inboxCarol, text: "p"}, msg{ts: mine, thread: ts, user: inboxMe, text: "mine"}))
	}
	e.serve(slackFake{
		threads: [][]hit{{
			{kind: "channel", ch: general, chName: "general", ts: "1700050000.000100", thread: quietA, user: inboxCarol, text: "deleted"},
			{kind: "channel", ch: general, chName: "general", ts: "1700040000.000100", thread: quietB, user: inboxCarol, text: "deleted"},
			{kind: "channel", ch: general, chName: "general", ts: "1700030000.000100", thread: busy, user: inboxCarol, text: "new"},
		}},
		replies: map[string]http.HandlerFunc{
			quietA: gone(quietA, "1700010000.000100"),
			quietB: gone(quietB, "1700010001.000100"),
			busy: body(repliesBody(
				msg{ts: busy, thread: busy, user: inboxCarol, text: "p"},
				msg{ts: "1700030000.000100", thread: busy, user: inboxCarol, text: "new"},
			)),
		},
	})
	budget := slackhttp.NewBudget(60)
	hc := slackhttp.New(2 * time.Second)
	hc.Transport = budget.Wrap(hc.Transport)
	since := inboxNow.Add(-defaultInboxWindow)
	r := &inboxRun{
		c: slackread.New(c, hc, e.srv.URL), budget: budget, clock: newClock(), me: inboxMe,
		since: sinceTS, until: untilTS, sinceT: since, untilT: inboxNow,
		// Three hits, then a parent and my reply for each quiet thread: the
		// first read fills it, and the rest fit only if that one is let go.
		ceiling: 5, blocked: map[string]bool{},
		held: map[string]bool{}, seen: map[string]bool{}, known: map[string]bool{},
		probes: map[string]probe{}, convs: map[string]*inboxConv{},
	}
	in, err := r.run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !in.Complete {
		t.Fatalf("inbox = %+v, want complete; the quiet threads kept their room", in)
	}
	conv := onlyConversation(t, in)
	if len(conv.Threads) != 1 || conv.Threads[0].ThreadTS != busy || !conv.Threads[0].Complete {
		t.Fatalf("threads = %+v, want only the thread with news, read whole", conv.Threads)
	}
	if n := e.count(methodReplies); n != 3 {
		t.Fatalf("replies reads = %d, want 3", n)
	}
}
