package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// env is one isolated invocation: a HOME nothing escapes, a fake Slack, and a
// credential that is real enough to authorise a request and useless anywhere
// else.
type env struct {
	t    *testing.T
	home string
	srv  *slacktest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := isolateHome(t)
	t.Setenv("SLACK_READ_TOKEN", "xoxp-fake-read-token")
	return &env{t: t, home: home, srv: slacktest.New(t)}
}

// testEndpoints points the API at the fake Slack and permits the loopback host
// its file server answers on — the same shape production has, where the API is
// Slack's and the permitted hosts are the ones Slack serves files from.
func testEndpoints(api string) endpoints {
	return endpoints{api: api, files: slackhosts.Allow("127.0.0.1")}
}

func (e *env) run(args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	var o, s bytes.Buffer
	code = run(args, &o, &s, testEndpoints(e.srv.URL))
	return o.String(), s.String(), code
}

// warm fills the cache, so a test that is not about fetching the lists does not
// have to stub them.
func (e *env) warm() {
	e.t.Helper()
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: testConvs})
	if err != nil {
		e.t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: testUsers})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) query(i int) url.Values {
	e.t.Helper()
	reqs := e.srv.Requests()
	if i >= len(reqs) {
		e.t.Fatalf("wanted request %d, only %d were made", i, len(reqs))
	}
	return reqs[i].Query
}

var testConvs = []cache.Conversation{
	{ID: "C0ENG0000", Name: "eng"},
	{ID: "C0ALERTS0", Name: "alerts-dev"},
	{ID: "C0RANDOM0", Name: "Random"},
	{ID: "D0STEPAN0", IsIM: true, User: "U0STEPAN0"},
	{ID: "D0DELETED", IsIM: true, User: "U0DELETED"},
}

var testUsers = []cache.User{
	{ID: "U0STEPAN0", Name: "stepan", RealName: "Stepan S", Profile: cache.Profile{DisplayName: "stepan"}},
	{ID: "U0ANN0000", Name: "ann", Profile: cache.Profile{DisplayName: "ann"}},
	{ID: "U0ANNABEL", Name: "annabel", Profile: cache.Profile{DisplayName: "annabel"}},
	{ID: "U0DELETED", Name: "departed", Deleted: true},
}

// permalinkHost is deliberately not the real Slack host: a module-wide guard
// forbids any file but the endpoint packages from naming that one, so that no
// test can be pointed at production. Nothing here depends on it — a link is
// read for its conversation id and its timestamp, and the host is never used.
const permalinkHost = "https://example.slack.test"

// history answers newest-first, which is what conversations.history does.
const twoMessages = `{"ok":true,"messages":[
  {"ts":"1785712434.000000","user":"U0ANN0000","text":"newest"},
  {"ts":"1785708834.000000","user":"U0STEPAN0","text":"oldest"}
]}`

func decode(t *testing.T, s string, v any) {
	t.Helper()
	err := json.Unmarshal([]byte(s), v)
	if err != nil {
		t.Fatalf("output is not JSON (%v): %s", err, s)
	}
}

func TestChannelsListsNamedConversationsInOrder(t *testing.T) {
	e := newEnv(t)
	e.warm()

	stdout, stderr, code := e.run("channels")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	// Sorted; DMs are excluded because they have no name.
	if want := "#Random\n#alerts-dev\n#eng\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests for a warm cache, want 0", got)
	}
}

func TestChannelsFilterIsACaseInsensitiveSubstring(t *testing.T) {
	e := newEnv(t)
	e.warm()

	for _, tc := range []struct{ filter, want string }{
		{"ENG", "#eng\n"},
		{"eng", "#eng\n"},
		{"random", "#Random\n"},
		{"e", "#alerts-dev\n#eng\n"},
		{"#a", "#alerts-dev\n"},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			stdout, stderr, code := e.run("channels", tc.filter)
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if stdout != tc.want {
				t.Fatalf("stdout = %q, want %q", stdout, tc.want)
			}
		})
	}
}

// A filter is a substring, not a pattern. `grep -i` would have read "a.b" as a
// regular expression; a filter is an argument, and an argument is data.
func TestAFilterIsNotAPattern(t *testing.T) {
	e := newEnv(t)
	e.warm()

	stdout, _, code := e.run("channels", "a.erts")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing — the filter was treated as a pattern", stdout)
	}
}

// An empty result is an answer, not a failure. A caller that treats a non-zero
// status as "something went wrong" would
// otherwise report an error for a channel that simply does not exist.
func TestAFilterMatchingNothingIsAnEmptyResultNotAnError(t *testing.T) {
	e := newEnv(t)
	e.warm()

	for _, verb := range []string{"channels", "dms"} {
		t.Run(verb, func(t *testing.T) {
			stdout, stderr, code := e.run(verb, "nothing-matches-this")
			if code != 0 {
				t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing", stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want nothing", stderr)
			}
		})
	}
}

// A question the cache can already answer costs no credential: the token is
// resolved at the point of request, so a warm cache never touches the
// keychain. That is what makes `hear channels` usable on a
// machine whose keychain is locked, or in a container that has no keyring at
// all.
func TestAWarmCacheResolvesNoCredential(t *testing.T) {
	for _, args := range [][]string{{"channels"}, {"dms"}, {"channels", "eng"}, {"refresh"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			consulted := watchCredential(t)

			stdout, stderr, code := e.run(args...)
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if *consulted {
				t.Error("a credential was resolved for a question the cache had already answered")
			}
			if got := e.srv.Hits(); got != 0 {
				t.Errorf("Slack received %d requests, want 0", got)
			}
			if args[0] != "refresh" && stdout == "" {
				t.Error("no output")
			}
		})
	}
}

// A cold cache is filled from Slack, exactly where ensure_conversations did it.
func TestAColdCacheIsFilledFromSlack(t *testing.T) {
	e := newEnv(t)
	e.srv.Reply("conversations.list", `{"ok":true,"channels":[{"id":"C0ENG0000","name":"eng"}]}`)

	stdout, stderr, code := e.run("channels")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if stdout != "#eng\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	var c cache.Conversations
	err := cache.Read(cache.ConversationsFile, &c)
	if err != nil {
		t.Fatalf("the fetched list was not cached: %v", err)
	}
	if len(c.Channels) != 1 || c.Channels[0].ID != "C0ENG0000" {
		t.Fatalf("cached %+v", c.Channels)
	}
}

func TestDMsAreListedByPerson(t *testing.T) {
	e := newEnv(t)
	e.warm()

	stdout, stderr, code := e.run("dms")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	// A deleted account is still listed when the conversation exists: the list
	// answers what conversations there are, not who is still active.
	if want := "@departed\n@stepan\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// history answers newest-first and a transcript reads downwards, so the whole
// page is turned round. Getting this wrong reverses every conversation the
// tool prints, and it reads plausibly either way.
func TestReadPrintsOldestFirst(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", twoMessages)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	oldest, newest := strings.Index(stdout, "oldest"), strings.Index(stdout, "newest")
	if oldest < 0 || newest < 0 {
		t.Fatalf("stdout = %q, want both messages", stdout)
	}
	if oldest > newest {
		t.Fatalf("stdout = %q, want the oldest message first", stdout)
	}
	if got := e.query(0).Get("channel"); got != "C0ENG0000" {
		t.Fatalf("channel = %q, want the resolved id", got)
	}
}

func TestReadDefaultsToTwentyMessages(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", twoMessages)

	if _, stderr, code := e.run("read", "#eng"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("limit"); got != "20" {
		t.Fatalf("limit = %q, want 20", got)
	}
}

func TestReadCountBecomesTheLimit(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", twoMessages)

	if _, stderr, code := e.run("read", "#eng", "5"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("limit"); got != "5" {
		t.Fatalf("limit = %q, want 5", got)
	}
	if n := e.srv.Hits(); n != 1 {
		t.Fatalf("%d requests, want one — a count that fits in a page is one call", n)
	}
}

// A count larger than one page is followed rather than truncated, and stops as
// soon as it has what was asked for.
func TestACountLargerThanAPageIsFollowed(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Pages("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785712434.000000","text":"m4"},{"ts":"1785712433.000000","text":"m3"}],
		  "response_metadata":{"next_cursor":"c1"}}`,
		`{"ok":true,"messages":[{"ts":"1785712432.000000","text":"m2"},{"ts":"1785712431.000000","text":"m1"}]}`)

	stdout, stderr, code := e.run("--json", "read", "#eng", "3")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	var doc render.Transcript
	decode(t, stdout, &doc)
	if len(doc.Messages) != 3 {
		t.Fatalf("got %d messages, want the 3 that were asked for", len(doc.Messages))
	}
	// Oldest first, and it is the oldest of the four that was dropped.
	if got := []string{doc.Messages[0].Text, doc.Messages[2].Text}; got[0] != "m2" || got[1] != "m4" {
		t.Fatalf("messages = %+v, want m2..m4 oldest first", doc.Messages)
	}
	if doc.Complete {
		t.Error("complete = true, but the count filled up before the channel ran out")
	}
	if got := e.query(1).Get("limit"); got != "1" {
		t.Fatalf("second page asked for limit=%q, want only the 1 message still missing", got)
	}
	if got := e.query(1).Get("cursor"); got != "c1" {
		t.Fatalf("second page cursor = %q", got)
	}
}

func TestReadRefusesACountThatIsNotOne(t *testing.T) {
	for _, count := range []string{"abc", "0", "-3", "2.5", ""} {
		t.Run(count, func(t *testing.T) {
			e := newEnv(t)
			e.warm()

			_, stderr, code := e.run("read", "#eng", count)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, "count must be a positive whole number") {
				t.Fatalf("stderr = %q", stderr)
			}
			if got := e.srv.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests for an invocation that was refused", got)
			}
		})
	}
}

// conversations.replies is already oldest-first, beginning with the parent.
// Reversing it here would print every thread backwards — the mirror of the
// history case, and the reason the two orderings are tested separately.
func TestThreadIsNotReversed(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.replies", `{"ok":true,"messages":[
	  {"ts":"1785708834.000000","user":"U0STEPAN0","text":"parent"},
	  {"ts":"1785708900.000000","user":"U0ANN0000","text":"reply"}
	]}`)

	stdout, stderr, code := e.run("thread", "#eng", "1785708834.000000")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if parent, reply := strings.Index(stdout, "parent"), strings.Index(stdout, "reply"); parent > reply {
		t.Fatalf("stdout = %q, want the parent first", stdout)
	}
	q := e.query(0)
	if q.Get("channel") != "C0ENG0000" || q.Get("ts") != "1785708834.000000" {
		t.Fatalf("query = %v", q)
	}
}

func TestAThreadIsWalkedToItsEnd(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Pages("conversations.replies",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","text":"parent"}],
		  "response_metadata":{"next_cursor":"c1"}}`,
		`{"ok":true,"messages":[{"ts":"1785708900.000000","text":"late reply"}]}`)

	stdout, stderr, code := e.run("thread", "#eng", "1785708834.000000")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "late reply") {
		t.Fatalf("stdout = %q, want the second page too", stdout)
	}
	if n := e.srv.Hits(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

// A window has no count to satisfy, so one call would silently answer "nothing
// happened" for any period busier than a page. This is the feature's whole
// claim: --since covers the window it names.
func TestSinceWalksTheWholeWindow(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Pages("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785712434.000000","text":"page one"}],
		  "response_metadata":{"next_cursor":"c1"}}`,
		`{"ok":true,"messages":[{"ts":"1785712400.000000","text":"page two"}],
		  "response_metadata":{"next_cursor":"c2"}}`,
		`{"ok":true,"messages":[{"ts":"1785712300.000000","text":"page three"}]}`)

	stdout, stderr, code := e.run("read", "#eng", "--since", "2h")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, want := range []string{"page one", "page two", "page three"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want %q — the window stopped short", stdout, want)
		}
	}
	if n := e.srv.Hits(); n != 3 {
		t.Fatalf("%d requests, want 3 — one per page until the cursor ran out", n)
	}
	// Oldest first across page boundaries, not just within a page.
	one, three := strings.Index(stdout, "page one"), strings.Index(stdout, "page three")
	if three > one {
		t.Fatalf("stdout = %q, want the oldest page first", stdout)
	}
}

func TestSinceBoundsTheRequestByTime(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", twoMessages)

	before := time.Now()
	if _, stderr, code := e.run("read", "#eng", "--since", "2h"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	q := e.query(0)
	oldest, err := strconv.ParseFloat(q.Get("oldest"), 64)
	if err != nil {
		t.Fatalf("oldest = %q: %v", q.Get("oldest"), err)
	}
	want := before.Add(-2 * time.Hour).Unix()
	if delta := int64(oldest) - want; delta < 0 || delta > 5 {
		t.Fatalf("oldest = %v, want about %v (two hours ago)", int64(oldest), want)
	}
	if q.Get("latest") != "" {
		t.Errorf("latest = %q, want the window open at the newest end", q.Get("latest"))
	}
}

func TestSinceAcceptsTheUnitsPeopleUse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"45s", 45 * time.Second},
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSince(tc.in)
			if err != nil {
				t.Fatalf("parseSince(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSinceRefusesAWindowThatAsksForNothing(t *testing.T) {
	for _, in := range []string{"0", "0h", "-2h", "soon", "2", "h", "99999999999999999999d"} {
		t.Run(in, func(t *testing.T) {
			d, err := parseSince(in)
			if err == nil {
				t.Fatalf("parseSince(%q) = %v, want an error", in, d)
			}
		})
	}
}

// Two ways of saying how much to read, which disagree whenever both are given.
// Refusing is the only answer that cannot silently do the wrong one.
func TestSinceAndACountAreRefusedTogether(t *testing.T) {
	e := newEnv(t)
	e.warm()

	_, stderr, code := e.run("read", "#eng", "20", "--since", "2h")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--since and a count") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

func TestSinceOnlyAppliesToRead(t *testing.T) {
	for _, args := range [][]string{
		{"channels", "--since", "2h"},
		{"thread", "#eng", "1785708834.000000", "--since", "2h"},
		{"search", "hello", "--since", "2h"},
		{"whoami", "--since", "2h"},
	} {
		t.Run(args[0], func(t *testing.T) {
			e := newEnv(t)
			e.warm()

			_, stderr, code := e.run(args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if !strings.Contains(stderr, "--since applies to read") {
				t.Fatalf("stderr = %q", stderr)
			}
			if got := e.srv.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
		})
	}
}

// A permalink is how a message is passed between people, so it is an address.
// The link form has no point in its timestamp — p1785708834862009 is
// 1785708834.862009 — and reading it back wrongly asks Slack about a thread
// fifty thousand years from now, which answers with an empty thread rather
// than an error.
func TestAPermalinkResolvesToItsChannelAndTimestamp(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.replies", `{"ok":true,"messages":[{"ts":"1785708834.862009","text":"linked"}]}`)

	stdout, stderr, code := e.run("read", permalinkHost+"/archives/C0ENG0000/p1785708834862009")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "linked") {
		t.Fatalf("stdout = %q", stdout)
	}
	q := e.query(0)
	if got := q.Get("channel"); got != "C0ENG0000" {
		t.Fatalf("channel = %q", got)
	}
	if got := q.Get("ts"); got != "1785708834.862009" {
		t.Fatalf("ts = %q, want the point put back in", got)
	}
}

// The reply hint has to be runnable. A permalink is not an address that can
// carry a timestamp beside it — `hear thread <link> <ts>` is refused — so a
// transcript reached through a link points its hint at the conversation id.
func TestTheReplyHintFromALinkIsRunnable(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.replies",
		`{"ok":true,"messages":[{"ts":"1785708834.862009","text":"parent","reply_count":4}]}`)

	stdout, stderr, code := e.run("read", permalinkHost+"/archives/C0ENG0000/p1785708834862009")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if want := "hear thread C0ENG0000 1785708834.862009"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want the hint to read %q", stdout, want)
	}
	if strings.Contains(stdout, "hear thread http") {
		t.Fatal("the hint names a link, which the thread verb refuses alongside a ts")
	}
}

func TestAThreadPermalinkReadsTheThreadItPointsInto(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.replies", `{"ok":true,"messages":[{"ts":"1785700000.000100","text":"parent"}]}`)

	link := permalinkHost + "/archives/C0ENG0000/p1785708834862009?thread_ts=1785700000.000100&cid=C0ENG0000"
	if _, stderr, code := e.run("thread", link); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("ts"); got != "1785700000.000100" {
		t.Fatalf("ts = %q, want the thread parent rather than the linked reply", got)
	}
}

func TestReadAndThreadAgreeOnAPermalink(t *testing.T) {
	const link = permalinkHost + "/archives/C0ENG0000/p1785708834862009"
	for _, verb := range []string{"read", "thread"} {
		t.Run(verb, func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			e.srv.Reply("conversations.replies", `{"ok":true,"messages":[{"ts":"1785708834.862009","text":"linked"}]}`)

			if _, stderr, code := e.run(verb, link); code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if got := e.query(0).Get("ts"); got != "1785708834.862009" {
				t.Fatalf("ts = %q", got)
			}
		})
	}
}

// A URL is an address of one kind only. Looking a malformed one up as a
// channel name would report "no such channel: https://…", which sends the
// reader looking for a channel instead of at their link.
func TestAMalformedLinkIsRefusedRatherThanLookedUp(t *testing.T) {
	for _, link := range []string{
		"https://example.com/nothing/here",
		permalinkHost + "/archives/C0ENG0000",
		permalinkHost + "/archives/C0ENG0000/1785708834862009",
		permalinkHost + "/archives/not-an-id/p1785708834862009",
		permalinkHost + "/archives/C0ENG0000/p1785708834862009?thread_ts=nonsense",
	} {
		t.Run(link, func(t *testing.T) {
			e := newEnv(t)
			e.warm()

			_, stderr, code := e.run("read", link)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, "not a Slack") {
				t.Fatalf("stderr = %q, want it to name the link as the problem", stderr)
			}
			if got := e.srv.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
		})
	}
}

func TestAPermalinkTakesNoCountOrWindow(t *testing.T) {
	const link = permalinkHost + "/archives/C0ENG0000/p1785708834862009"
	for _, args := range [][]string{
		{"read", link, "20"},
		{"read", link, "--since", "2h"},
		{"thread", link, "1785708834.862009"},
	} {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			e := newEnv(t)
			e.warm()

			_, stderr, code := e.run(args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr)
			}
			if got := e.srv.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
		})
	}
}

func TestSearchSendsTheWholeQuery(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("search.messages", `{"ok":true,"messages":{"matches":[
	  {"ts":"1785708834.000000","username":"ann","text":"deploy failed","channel":{"id":"C0ENG0000","name":"eng"}}
	]}}`)

	stdout, stderr, code := e.run("search", "deploy", "failed")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if want := "#eng  ann  deploy failed\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	q := e.query(0)
	if got := q.Get("query"); got != "deploy failed" {
		t.Fatalf("query = %q, want the words joined", got)
	}
	if got := q.Get("count"); got != "20" {
		t.Fatalf("count = %q, want 20", got)
	}
}

// Slack's search syntax spells exclusion "-term", so a single-dash word is
// query text rather than an option. Two dashes is the tool's own namespace,
// and "--" is how a query that really begins with them is searched for.
func TestSearchTextThatLooksLikeAnOption(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("search.messages", `{"ok":true,"messages":{"matches":[]}}`)

	if _, stderr, code := e.run("search", "deploy", "-failed"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("query"); got != "deploy -failed" {
		t.Fatalf("query = %q", got)
	}

	e2 := newEnv(t)
	e2.warm()
	e2.srv.Reply("search.messages", `{"ok":true,"messages":{"matches":[]}}`)
	if _, stderr, code := e2.run("search", "--", "--verbose"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e2.query(0).Get("query"); got != "--verbose" {
		t.Fatalf("query = %q, want the words after -- taken literally", got)
	}
}

func TestUnknownOptionIsRefused(t *testing.T) {
	e := newEnv(t)
	e.warm()

	_, stderr, code := e.run("read", "#eng", "--sicne", "2h")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown option: --sicne") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// Options are accepted in any position, so both orders are the same command.
func TestTSIsAcceptedInAnyPosition(t *testing.T) {
	for _, args := range [][]string{
		{"--ts", "read", "#eng"},
		{"read", "--ts", "#eng"},
		{"read", "#eng", "--ts"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			e.srv.Reply("conversations.history", twoMessages)

			stdout, stderr, code := e.run(args...)
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if !strings.HasPrefix(stdout, "1785708834.000000  ") {
				t.Fatalf("stdout = %q, want each line to begin with its timestamp", stdout)
			}
		})
	}
}

func TestRefreshClearsTheCache(t *testing.T) {
	e := newEnv(t)
	e.warm()
	dir, err := cache.Dir()
	if err != nil {
		t.Fatal(err)
	}
	// Every *.json goes, not only the files this package writes.
	stray := filepath.Join(dir, "stray.json")
	err = os.WriteFile(stray, []byte("{}"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := e.run("refresh")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if stdout != "cache cleared\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, f := range []string{cache.ConversationsFile, cache.UsersFile, "stray.json"} {
		_, err := os.Stat(filepath.Join(dir, f))
		if !os.IsNotExist(err) {
			t.Errorf("%s survived refresh (%v)", f, err)
		}
	}
}

func TestNoSuchChannelSaysHowToFixIt(t *testing.T) {
	e := newEnv(t)
	e.warm()

	_, stderr, code := e.run("read", "#nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such channel: nope") || !strings.Contains(stderr, "hear refresh") {
		t.Fatalf("stderr = %q, want the channel named and the cure suggested", stderr)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests for a channel that does not exist", got)
	}
}

func TestReadingAPersonUsesTheirDM(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", twoMessages)

	if _, stderr, code := e.run("read", "@stepan"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("channel"); got != "D0STEPAN0" {
		t.Fatalf("channel = %q, want the DM", got)
	}
}

// A raw conversation id is addressed without consulting the cache at all,
// which is what makes `hear read C0…` work on a machine that has never run
// `hear channels` — and what makes an id pasted from somewhere else usable
// immediately.
func TestARawIDNeedsNoCache(t *testing.T) {
	e := newEnv(t)
	e.srv.Reply("conversations.history", twoMessages)
	e.srv.Reply("users.list", `{"ok":true,"members":[]}`)

	if _, stderr, code := e.run("read", "C0ENG0000"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, r := range e.srv.Requests() {
		if r.Endpoint == "conversations.list" {
			t.Fatal("the conversation list was fetched to resolve an id that needed no resolving")
		}
	}
	if got := e.query(0).Get("channel"); got != "C0ENG0000" {
		t.Fatalf("channel = %q", got)
	}
}

// A prefix matching two people is an error, never a guess: reading the wrong
// person's DM is a disclosure, and the fix — type the whole handle — is
// something only the reader can choose.
func TestAnAmbiguousPersonIsRefused(t *testing.T) {
	e := newEnv(t)
	e.warm()

	_, stderr, code := e.run("read", "@an")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "matches several people") || !strings.Contains(stderr, "hear dms") {
		t.Fatalf("stderr = %q, want the ambiguity named and a way out of it", stderr)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// The first run on an unenrolled machine: nothing is cached, so even a list
// needs a credential, and the answer is the enrolment instructions rather than
// a cache error nobody can act on.
func TestAColdCacheWithNoCredentialExplainsEnrolment(t *testing.T) {
	e := newEnv(t)
	t.Setenv("SLACK_READ_TOKEN", "")

	_, stderr, code := e.run("channels")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no read token found") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := e.srv.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests with no credential, want 0", got)
	}
}

// Slack rejects limit >= 1000, so a larger count is asked for a page at a
// time. Passing the count straight through would turn `hear read #eng 2000`
// into an invalid_limit error rather than 2000 messages.
func TestACountLargerThanSlackAcceptsIsSplitIntoPages(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Pages("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785712434.000000","text":"m2"}],
		  "response_metadata":{"next_cursor":"c1"}}`,
		`{"ok":true,"messages":[{"ts":"1785712433.000000","text":"m1"}]}`)

	if _, stderr, code := e.run("read", "#eng", "1500"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := e.query(0).Get("limit"); got != "999" {
		t.Fatalf("limit = %q, want it capped at what Slack accepts", got)
	}
}

func TestSearchShowsTimestampsToo(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("search.messages", `{"ok":true,"messages":{"matches":[
	  {"ts":"1785708834.000000","username":"ann","text":"hi","channel":{"id":"C0ENG0000","name":"eng"}}
	]}}`)

	stdout, stderr, code := e.run("--ts", "search", "hi")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if want := "1785708834.000000  #eng  ann  hi\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestReadingAPersonWithNoDMExplainsWhy(t *testing.T) {
	e := newEnv(t)
	e.warm()

	_, stderr, code := e.run("read", "@ann")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no open DM") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// The mention hazard, end to end: a display name is chosen by its owner and
// travels through the cache into the transcript. It is printed, never
// interpreted.
func TestADisplayNameNeverBecomesAReplacementTemplate(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: testConvs})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: []cache.User{
		{ID: "U0EVIL000", Profile: cache.Profile{DisplayName: "$1${x}"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0EVIL000","text":"ping <@U0EVIL000>"}]}`)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "ping @$1${x}") {
		t.Fatalf("stdout = %q, want the name printed literally", stdout)
	}
}

func TestJSONOutputShapes(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		e := newEnv(t)
		e.srv.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0STEPAN0"}`)

		stdout, stderr, code := e.run("--json", "whoami")
		if code != 0 {
			t.Fatalf("exit = %d (stderr %q)", code, stderr)
		}
		var got render.Identity
		decode(t, stdout, &got)
		want := render.Identity{Kind: "identity", User: "stepan", Team: "LedgerRocket", UserID: "U0STEPAN0"}
		if got != want {
			t.Fatalf("identity = %+v, want %+v", got, want)
		}
	})

	t.Run("channels", func(t *testing.T) {
		e := newEnv(t)
		e.warm()

		stdout, _, code := e.run("--json", "channels", "eng")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.Channels
		decode(t, stdout, &got)
		if got.Kind != "channels" || len(got.Channels) != 1 {
			t.Fatalf("channels = %+v", got)
		}
		// The id is the reason to ask for JSON: the next command can address
		// what this one found without resolving the name again.
		if got.Channels[0].ID != "C0ENG0000" || got.Channels[0].Name != "eng" {
			t.Fatalf("channel = %+v", got.Channels[0])
		}
	})

	t.Run("dms", func(t *testing.T) {
		e := newEnv(t)
		e.warm()

		stdout, _, code := e.run("--json", "dms", "stepan")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.DMs
		decode(t, stdout, &got)
		if got.Kind != "dms" || len(got.DMs) != 1 {
			t.Fatalf("dms = %+v", got)
		}
		if got.DMs[0] != (render.DM{ID: "D0STEPAN0", UserID: "U0STEPAN0", Name: "stepan"}) {
			t.Fatalf("dm = %+v", got.DMs[0])
		}
	})

	t.Run("transcript", func(t *testing.T) {
		e := newEnv(t)
		e.warm()
		e.srv.Reply("conversations.history",
			`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0ANN0000","text":"hi <@U0STEPAN0>","reply_count":2}]}`)

		stdout, _, code := e.run("--json", "read", "#eng")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.Transcript
		decode(t, stdout, &got)
		if got.Kind != "transcript" || got.Channel.ID != "C0ENG0000" || got.Channel.Input != "#eng" {
			t.Fatalf("transcript = %+v", got)
		}
		if !got.Complete {
			t.Error("complete = false, but Slack returned no cursor")
		}
		if len(got.Messages) != 1 {
			t.Fatalf("messages = %+v", got.Messages)
		}
		m := got.Messages[0]
		if m.Text != "hi <@U0STEPAN0>" || m.TextRendered != "hi @stepan" {
			t.Errorf("text = %q / %q", m.Text, m.TextRendered)
		}
		if m.Author != "ann" || m.User != "U0ANN0000" || m.ReplyCount != 2 {
			t.Errorf("message = %+v", m)
		}
	})

	t.Run("thread", func(t *testing.T) {
		e := newEnv(t)
		e.warm()
		e.srv.Reply("conversations.replies", `{"ok":true,"messages":[{"ts":"1785708834.000000","text":"parent"}]}`)

		stdout, _, code := e.run("--json", "thread", "#eng", "1785708834.000000")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.Transcript
		decode(t, stdout, &got)
		if got.ThreadTS != "1785708834.000000" {
			t.Fatalf("thread_ts = %q, want the thread it is a transcript of", got.ThreadTS)
		}
	})

	t.Run("search", func(t *testing.T) {
		e := newEnv(t)
		e.warm()
		e.srv.Reply("search.messages", `{"ok":true,"messages":{"matches":[
		  {"ts":"1785708834.000000","username":"ann","text":"hi <@U0STEPAN0>","channel":{"id":"C0ENG0000","name":"eng"}}
		]}}`)

		stdout, _, code := e.run("--json", "search", "hi")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.SearchResult
		decode(t, stdout, &got)
		if got.Kind != "search" || got.Query != "hi" || len(got.Matches) != 1 {
			t.Fatalf("search = %+v", got)
		}
		hit := got.Matches[0]
		if hit.Channel.ID != "C0ENG0000" || hit.Author != "ann" || hit.TextRendered != "hi @stepan" {
			t.Fatalf("hit = %+v", hit)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		e := newEnv(t)
		e.warm()

		stdout, _, code := e.run("--json", "refresh")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		var got render.Refresh
		decode(t, stdout, &got)
		if got.Kind != "refresh" || !got.Cleared {
			t.Fatalf("refresh = %+v", got)
		}
	})
}

// An empty result is an empty list, never null: a consumer that iterates the
// field must not have to special-case "nothing matched".
func TestJSONListsAreEmptyRatherThanNull(t *testing.T) {
	e := newEnv(t)
	e.warm()

	for _, args := range [][]string{{"--json", "channels", "no-such-thing"}, {"--json", "dms", "no-such-thing"}} {
		stdout, _, code := e.run(args...)
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if strings.Contains(stdout, "null") {
			t.Fatalf("%v printed %s, want an empty list", args, stdout)
		}
	}
}

// Diagnostics stay on stderr even in JSON mode, so a caller can parse stdout
// unconditionally: an error must not arrive as an unparseable half-object.
func TestJSONKeepsDiagnosticsOffStdout(t *testing.T) {
	e := newEnv(t)
	e.warm()

	stdout, stderr, code := e.run("--json", "read", "#nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing on the parseable stream", stdout)
	}
	if !strings.Contains(stderr, "no such channel") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSlackErrorsAreReported(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history", `{"ok":false,"error":"channel_not_found"}`)

	_, stderr, code := e.run("read", "#eng")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "channel_not_found") {
		t.Fatalf("stderr = %q, want Slack's own error", stderr)
	}
}

func TestEveryVerbNeedingACredentialSaysSoWhenThereIsNone(t *testing.T) {
	for _, args := range [][]string{
		{"whoami"},
		{"read", "#eng"},
		{"thread", "#eng", "1785708834.000000"},
		{"search", "hello"},
	} {
		t.Run(args[0], func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			t.Setenv("SLACK_READ_TOKEN", "")

			_, stderr, code := e.run(args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr, "no read token found") {
				t.Fatalf("stderr = %q", stderr)
			}
			if got := e.srv.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests with no credential, want 0", got)
			}
		})
	}
}

func TestHelpDocumentsEveryVerbAndOption(t *testing.T) {
	isolateHome(t)
	watchCredential(t)
	s := slacktest.New(t)

	var help bytes.Buffer
	if code := run([]string{"--help"}, &help, io.Discard, testEndpoints(s.URL)); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"whoami", "channels", "dms", "read", "thread", "search", "refresh",
		"--ts", "--json", "--since", "permalink",
	} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("--help does not mention %q", want)
		}
	}
}

func TestVerbsRefuseArgumentsTheyCannotUse(t *testing.T) {
	for _, args := range [][]string{
		{"whoami", "extra"},
		{"refresh", "extra"},
		{"channels", "a", "b"},
		{"dms", "a", "b"},
		{"read"},
		{"read", "#eng", "20", "extra"},
		{"thread"},
		{"thread", "#eng"},
		{"thread", "#eng", "1785708834.000000", "extra"},
		{"search"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e := newEnv(t)
			e.warm()
			consulted := watchCredential(t)

			_, stderr, code := e.run(args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr)
			}
			if *consulted {
				t.Error("a credential was resolved for an invocation the command cannot honour")
			}
			if got := e.srv.Hits(); got != 0 {
				t.Errorf("Slack received %d requests, want 0", got)
			}
		})
	}
}

// A duration says nothing about how much there is. A window that never ends is
// stopped at a ceiling and reported as incomplete, rather than buffering a
// channel's whole history before printing a line of it.
func TestSinceStopsAtItsCeilingAndSaysSo(t *testing.T) {
	e := newEnv(t)
	e.warm()

	// A page that always has another page after it: the busy-channel case, and
	// the hostile one, are the same request.
	var page strings.Builder
	page.WriteString(`{"ok":true,"messages":[`)
	for i := range pageSize {
		if i > 0 {
			page.WriteString(",")
		}
		fmt.Fprintf(&page, `{"ts":"178571%04d.000000","text":"m"}`, i)
	}
	page.WriteString(`],"response_metadata":{"next_cursor":"more"}}`)
	e.srv.Reply("conversations.history", page.String())

	stdout, stderr, code := e.run("--json", "read", "#eng", "--since", "7d")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	var doc render.Transcript
	decode(t, stdout, &doc)
	if len(doc.Messages) != maxWindow {
		t.Fatalf("got %d messages, want the %d-message ceiling", len(doc.Messages), maxWindow)
	}
	if doc.Complete {
		t.Error("complete = true, but the window was cut short at the ceiling")
	}
	if n, want := e.srv.Hits(), maxWindow/pageSize; n != want {
		t.Fatalf("%d requests, want %d — it kept paging past the ceiling", n, want)
	}
}

// The one output change reactions make is what `hear read` prints, and the
// packages below it are the only place it was tested. This holds the wiring in
// place: verbs.go hands whole slackread.Message values to the renderer, so
// nothing drops Reactions on the way — a property nothing else asserts.
func TestReadPrintsReactions(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0STEPAN0","text":"deployed",`+
			`"reactions":[{"name":"white_check_mark","count":1,"users":["U0STEPAN0"]}]}]}`)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, ":white_check_mark: 1 (stepan)") {
		t.Fatalf("stdout = %q, want the reaction line", stdout)
	}
}

// The reader's own reaction is marked, end to end: a warm identity reaches the
// transcript. Without this the promotion logic could be correct in render and
// never wired to a real read.
func TestReadMarksTheReadersOwnReaction(t *testing.T) {
	e := newEnv(t)
	e.warm()
	// Warmed the way a user warms it, which also covers the handoff: whoami
	// records the credential the identity came from, and the read accepts it
	// because that is still the credential in use.
	e.srv.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0STEPAN0"}`)
	if _, stderr, code := e.run("whoami"); code != 0 {
		t.Fatalf("whoami exit = %d (stderr %q)", code, stderr)
	}
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0STEPAN0","text":"deployed",`+
			`"reactions":[{"name":"tada","count":1,"users":["U0STEPAN0"]}]}]}`)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, ":tada: 1 (you)") {
		t.Fatalf("stdout = %q, want the reader's own reaction marked", stdout)
	}
}

// A read never fetches the identity. auth.test is left unstubbed, so the fake
// Slack fails the test if the read reaches for it — which is the whole point:
// the marker must not cost a call, on a cold cache or a warm one.
func TestAReadNeverFetchesTheIdentity(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0STEPAN0","text":"deployed",`+
			`"reactions":[{"name":"tada","count":1,"users":["U0STEPAN0"]}]}]}`)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q), want a read with no identity to succeed", code, stderr)
	}
	if !strings.Contains(stdout, ":tada: 1") {
		t.Fatalf("stdout = %q, want the reaction rendered unmarked", stdout)
	}
	if strings.Contains(stdout, "you") {
		t.Fatalf("stdout = %q, want no marker without a warm identity", stdout)
	}
}

// whoami already calls auth.test to answer, so writing the result through makes
// the identity free: one whoami warms it for every later read.
func TestWhoamiWarmsTheIdentity(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0STEPAN0"}`)

	if _, stderr, code := e.run("whoami"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err != nil {
		t.Fatalf("identity after whoami: %v, want it warmed", err)
	}
	if id.UserID != "U0STEPAN0" {
		t.Fatalf("UserID = %q, want U0STEPAN0", id.UserID)
	}
}

// whoami reports who the credential belongs to; warming the cache is a side
// benefit of having asked. An unwritable cache directory must not turn a
// working whoami into a failure — the answer came from Slack, not from disk.
func TestWhoamiSurvivesAnUnwritableCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the cache would stay writable")
	}
	e := newEnv(t)
	e.srv.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0STEPAN0"}`)
	dir, err := cache.Dir()
	if err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Chmod(dir, 0o500)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	stdout, stderr, code := e.run("whoami")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q), want whoami to answer anyway", code, stderr)
	}
	if !strings.Contains(stdout, "U0STEPAN0") {
		t.Fatalf("stdout = %q, want the identity reported", stdout)
	}
}

// An identity warmed under a different credential must not be trusted. A
// SLACK_READ_TOKEN override or a re-enrolment switches accounts without
// clearing the cache, and a marker that says "you" about someone else's
// reaction is worse than no marker at all.
func TestAReadIgnoresAnIdentityFromAnotherCredential(t *testing.T) {
	e := newEnv(t)
	e.warm()
	err := cache.Write(cache.IdentityFile, cache.Identity{
		UserID: "U0STEPAN0", User: "stepan", CredentialKey: "a-different-credential",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.000000","user":"U0STEPAN0","text":"deployed",`+
			`"reactions":[{"name":"tada","count":1,"users":["U0STEPAN0"]}]}]}`)

	stdout, stderr, code := e.run("read", "#eng")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if strings.Contains(stdout, "(you)") {
		t.Fatalf("stdout = %q, want no marker from another credential's identity", stdout)
	}
}

// whoami records which credential the identity came from, so a later read can
// tell whether it still applies.
func TestWhoamiRecordsTheCredentialKey(t *testing.T) {
	e := newEnv(t)
	e.warm()
	e.srv.Reply("auth.test", `{"ok":true,"user":"stepan","team":"LedgerRocket","user_id":"U0STEPAN0"}`)

	if _, stderr, code := e.run("whoami"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	var id cache.Identity
	err := cache.Read(cache.IdentityFile, &id)
	if err != nil {
		t.Fatal(err)
	}
	if id.CredentialKey == "" {
		t.Fatal("CredentialKey is empty, want the credential it came from recorded")
	}
	if strings.Contains(id.CredentialKey, "xoxp-fake-read-token") {
		t.Fatalf("CredentialKey = %q, want a digest rather than the token", id.CredentialKey)
	}
}
