package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

// hostileName is a name a workspace member or admin can choose: a clear-screen,
// the C1 control-sequence introducer and a right-to-left override.
const hostileName = "x\x1b[2J\u009b31m\u202ey"

// assertPrintable fails on output that could drive or rearrange a terminal.
// Each list prints one name per line, so a newline is the only control allowed,
// beside the two joiners that shape a name.
func assertPrintable(t *testing.T, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	for _, r := range out {
		if r != '\n' && r != '\u200c' && r != '\u200d' && termsafe.IsHostile(r) {
			t.Fatalf("output carries %U, which drives a terminal: %q", r, out)
		}
	}
}

// The lists print names that someone other than the reader chose: a DM is
// named after the other person's display name.
func TestListsStripTerminalControls(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: []cache.Conversation{
		{ID: "C0HOSTILE", Name: hostileName},
		{ID: "D0HOSTILE", IsIM: true, User: "U0HOSTILE"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: []cache.User{
		{ID: "U0HOSTILE", Name: "h", Profile: cache.Profile{DisplayName: hostileName}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	for verb, want := range map[string]string{"channels": "C0HOSTILE\n", "dms": "D0HOSTILE\n"} {
		stdout, stderr, code := e.run(verb)
		if code != 0 {
			t.Fatalf("%s: exit = %d (stderr %q)", verb, code, stderr)
		}
		assertPrintable(t, stdout)
		// A name that had to be changed to print would no longer resolve, so
		// the list gives the id, which does.
		if stdout != want {
			t.Errorf("%s = %q, want %q", verb, stdout, want)
		}
	}
}

// The workspace name in whoami is chosen by the workspace's admins.
func TestWhoamiStripsTerminalControls(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_READ_TOKEN", "xoxp-fake-read-token")
	s := slacktest.New(t)
	s.Reply("auth.test", `{"ok":true,"user":"alice","team":"x\u001b[2J\u009b31m\u202ey","user_id":"U0123ABCD"}`)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"whoami"}, &stdout, &stderr, testEndpoints(s.URL)); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	assertPrintable(t, stdout.String())
	if want := "alice @ x[2J31my  (U0123ABCD)\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// A name with an emoji built from several is an ordinary name: it lists as
// itself, because it still resolves.
func TestADMWithAJoinerInTheNameListsByName(t *testing.T) {
	e := newEnv(t)
	coder := "Sam \U0001F469\u200d\U0001F4BB"
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: []cache.Conversation{
		{ID: "D0SAM0000", IsIM: true, User: "U0SAM0000"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: []cache.User{
		{ID: "U0SAM0000", Name: "sam", Profile: cache.Profile{DisplayName: coder}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := e.run("dms")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if want := "@" + coder + "\n"; stdout != want {
		t.Fatalf("dms = %q, want %q", stdout, want)
	}
}

// Two people whose names both print as their ids are two lines, not one: the
// list folds repeats of the line it prints, not of the name behind it.
func TestTwoDMsListedByIDAreBothListed(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: []cache.Conversation{
		{ID: "D0HOSTIL1", IsIM: true, User: "U0HOSTIL1"},
		{ID: "D0HOSTIL2", IsIM: true, User: "U0HOSTIL2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: []cache.User{
		{ID: "U0HOSTIL1", Name: "h1", Profile: cache.Profile{DisplayName: hostileName}},
		{ID: "U0HOSTIL2", Name: "h2", Profile: cache.Profile{DisplayName: hostileName}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := e.run("dms")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if want := "D0HOSTIL1\nD0HOSTIL2\n"; stdout != want {
		t.Fatalf("dms = %q, want %q", stdout, want)
	}
}

// The reply hint is a command to run. When the address the reader typed would
// print differently, the hint names the conversation id, which still resolves.
func TestTheReplyHintUsesTheIDWhenTheAddressWouldChange(t *testing.T) {
	e := newEnv(t)
	err := cache.Write(cache.ConversationsFile, cache.Conversations{Channels: []cache.Conversation{
		{ID: "C0HOSTILE", Name: hostileName},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = cache.Write(cache.UsersFile, cache.Users{Members: testUsers})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Reply("conversations.history",
		`{"ok":true,"messages":[{"ts":"1785708834.862009","text":"parent","reply_count":4}]}`)

	stdout, stderr, code := e.run("read", "#"+hostileName)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	assertPrintable(t, stdout)
	if want := "hear thread C0HOSTILE 1785708834.862009"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want the hint to read %q", stdout, want)
	}
}
