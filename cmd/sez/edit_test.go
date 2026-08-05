package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// stubRead replaces the read that pairs sez with hear. sez holds the write
// credential and the write app has no history scopes, so it cannot read a
// message itself — it asks the hear binary, which holds its own.
func stubRead(t *testing.T, text string, err error) {
	t.Helper()
	prev := readMessage
	readMessage = func(context.Context, string, string) (string, error) { return text, err }
	t.Cleanup(func() { readMessage = prev })
}

// An edit rewrites what people have already read, so the render shows both
// sides. Showing only the replacement would be the same as not showing it: the
// question is what changes, not what it ends up saying.
func TestEditRendersBeforeAndAfter(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubRead(t, "the original wording", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1785712599.000100","message":{"text":"x"}}`)

	code, stdout, stderr := sez(t, s, "--edit", "#eng", "1785712599.000100", "the new wording")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests during a render, want 0", got)
	}
	for _, want := range []string{"the original wording", "the new wording"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to show %q", stdout, want)
		}
	}
}

// A delete destroys something unrecoverable, so the render's whole job is to
// show what is about to go.
func TestDeleteRendersWhatWillBeDestroyed(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubRead(t, "about to be destroyed", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true}`)

	code, stdout, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests during a render, want 0", got)
	}
	if !strings.Contains(stdout, "about to be destroyed") {
		t.Fatalf("stdout = %q, want the message that would be destroyed", stdout)
	}
}

// The rule the pairing exists for: if the current text could not be read, the
// change is refused. Not warned about — refused. The reader was promised sight
// of what changes, and a tool that could not show it has not earned the send.
func TestASendIsRefusedWhenTheMessageCannotBeRead(t *testing.T) {
	for _, verb := range []string{"--edit", "--delete"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			seedCache(t)
			stubRead(t, "", errors.New("hear: not found"))
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply("chat.update", `{"ok":true,"ts":"1.1","message":{"text":"x"}}`)
			s.Reply("chat.delete", `{"ok":true}`)

			args := []string{"--send", verb, "#eng", "1785712599.000100"}
			if verb == "--edit" {
				args = append(args, "replacement")
			}
			code, stdout, _ := sez(t, s, args...)
			if code == 0 {
				t.Fatalf("exit = 0, want a refusal (stdout %q)", stdout)
			}
			if got := s.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0 — the change must not be attempted", got)
			}
		})
	}
}

func TestEditSendsTheUpdate(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "before", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1785712599.000100","message":{"text":"after"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	code, _, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "after")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	// Named rather than taken from Last: the edit is followed by a permalink
	// fetch, so the final request is not the one under test.
	upd := requestTo(t, s, "chat.update")
	if got := upd.Body.Get("ts"); got != "1785712599.000100" {
		t.Fatalf("ts = %q", got)
	}
	if got := upd.Body.Get("text"); got != "after" {
		t.Fatalf("text = %q", got)
	}
	recs := readLog(t, log)
	if len(recs) == 0 {
		t.Fatal("no audit records")
	}
	if got := recs[len(recs)-1]["action"]; got != "message.update" {
		t.Fatalf("action = %v, want message.update", got)
	}
}

// The log records what was destroyed, not merely that something was. Before
// sez could read a message this was impossible; pairing with hear is what
// makes a delete auditable at all.
func TestTheDeletedTextIsRecorded(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "the words that were destroyed", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true}`)

	code, _, stderr := sez(t, s, "--send", "--delete", "#eng", "1785712599.000100")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	last := recs[len(recs)-1]
	if got := last["action"]; got != "message.delete" {
		t.Fatalf("action = %v, want message.delete", got)
	}
	if got, ok := last["text"].(string); !ok || !strings.Contains(got, "the words that were destroyed") {
		t.Fatalf("text = %v, want the destroyed message recorded", last["text"])
	}
}

// An ordinary send says so, rather than being the record with no action.
//
// Seeded deliberately: a #name found in the cache resolves, and every other
// send test here runs against a cold one. That difference hid a change that
// skipped the permalink fetch for resolved targets — which is every ordinary
// send — so this pins the fetch in the state the tests were all missing.
func TestASendNamesItsActionAndKeepsItsPermalink(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.2","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	if code, _, stderr := sez(t, s, "--send", "#eng", "hi"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	for _, r := range recs {
		if got := r["action"]; got != "message.send" {
			t.Fatalf("action = %v, want message.send on every line", got)
		}
	}
	last := recs[len(recs)-1]
	if got, _ := last["permalink"].(string); !strings.Contains(got, "/archives/") {
		t.Fatalf("permalink = %v, want the link to the delivered message", last["permalink"])
	}
}

// The intent line outlives a kill, so it must name the message it addressed —
// otherwise a delete interrupted before its outcome cannot be reconciled
// against Slack, and two identical messages are indistinguishable in the log.
func TestEveryDeleteRecordNamesTheMessage(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "doomed", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true}`)

	if code, _, stderr := sez(t, s, "--send", "--delete", "#eng", "1785712599.000100"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, r := range readLog(t, log) {
		if got := r["ts"]; got != "1785712599.000100" {
			t.Fatalf("ts = %v on a %v line, want the addressed message on every line", got, r["phase"])
		}
	}
}

// --edit and --delete are mutually exclusive, and neither combines with a
// thread parent: all three name a different message.
func TestTheMessageVerbsAreMutuallyExclusive(t *testing.T) {
	for _, args := range [][]string{
		{"--edit", "--delete", "#eng", "1785712599.000100"},
		{"--delete", "--thread", "1785712599.000100", "#eng", "1785712599.000100"},
	} {
		isolate(t)
		seedCache(t)
		s := slacktest.New(t)
		if code, _, _ := sez(t, s, args...); code != 2 {
			t.Fatalf("args %v: exit = %d, want 2 (usage)", args, code)
		}
	}
}

// fakeHear installs an executable named hear beside the test's PATH, so the
// real subprocess path runs. Every other test here substitutes readMessage,
// which leaves viaHear — the part that actually shells out — uncovered.
func fakeHear(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "hear"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prepended, not replaced: a stub that shells out needs sleep and the rest
	// of the system's binaries to resolve, and one that cannot find them exits
	// 127 instantly — which satisfies any test asserting only a non-zero exit.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A refusal has to say why. sez fails closed when it cannot read the message,
// so "hear: exit status 1" leaves the reader with a tool that declined and no
// way to learn what to fix — where hear itself already printed the reason.
func TestAFailedReadReportsWhatHearSaid(t *testing.T) {
	isolate(t)
	seedCache(t)
	fakeHear(t, `echo "hear: no read credential found" >&2; exit 1`)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, _, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
	if code == 0 {
		t.Fatal("exit = 0, want a refusal")
	}
	if !strings.Contains(stderr, "no read credential found") {
		t.Fatalf("stderr = %q, want it to carry hear's own explanation", stderr)
	}
}

// hear can block rather than fail — a locked keychain prompts, and a stalled
// connection waits. Without a bound, sez waits with it and says nothing.
func TestAHangingReadIsBounded(t *testing.T) {
	isolate(t)
	seedCache(t)
	fakeHear(t, "exec sleep 30")
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	// Both, or the bound under test is hearBound's 2×HERSAID_TIMEOUT floor —
	// 40s by default — and the seam below is inert.
	t.Setenv("HERSAID_TIMEOUT", "1")
	prev := hearTimeout
	hearTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hearTimeout = prev })
	s := slacktest.New(t)

	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, _, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
		done <- result{code, stderr}
	}()
	select {
	case res := <-done:
		if res.code == 0 {
			t.Fatal("exit = 0, want a refusal when the read timed out")
		}
		// Named, so an unresolvable stub exiting 127 cannot satisfy this.
		if !strings.Contains(res.stderr, "no answer within") {
			t.Fatalf("stderr = %q, want the refusal to name the timeout", res.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sez did not return; the read is unbounded")
	}
}

// The happy path through the real subprocess, so the decoding is exercised too.
func TestViaHearReadsTheMatchingMessage(t *testing.T) {
	fakeHear(t, `echo '{"messages":[{"ts":"1.1","text":"other"},{"ts":"1785712599.000100","text":"wanted"}]}'`)
	got, err := viaHear(context.Background(), "C1", "1785712599.000100")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wanted" {
		t.Fatalf("text = %q, want the message whose ts matched", got)
	}
}

// An edit destroys the previous wording as surely as a delete destroys the
// message. sez has just read and shown it, so the log keeps it — otherwise the
// record says what a message became and never what it stopped saying.
func TestAnEditRecordsTheWordingItReplaced(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "what it used to say", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1785712599.000100","message":{"text":"what it says now"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	if code, _, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "what it says now"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	for _, r := range readLog(t, log) {
		if got, _ := r["previous_text"].(string); got != "what it used to say" {
			t.Fatalf("previous_text = %v on a %v line, want the replaced wording", r["previous_text"], r["phase"])
		}
	}
}

// change duplicates post's accounting, so it inherits post's invariants — and
// nothing was exercising the inherited branches. A transport-class refusal must
// record ok:null and never be retried: the edit may well have landed.
func TestAnAmbiguousEditIsRecordedAsUnknown(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "before", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":false,"error":"internal_error"}`)

	code, _, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "after")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(stderr, "UNKNOWN") {
		t.Fatalf("stderr = %q, want the delivery reported as unknown", stderr)
	}
	recs := readLog(t, log)
	if got := recs[len(recs)-1]["ok"]; got != nil {
		t.Fatalf("ok = %v, want null — the edit may have landed", got)
	}
}

// The scope paragraph reaches the new verbs too, not only sends.
func TestAMissingScopeOnADeleteExplainsTheFix(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubRead(t, "doomed", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":false,"error":"missing_scope"}`)

	code, _, stderr := sez(t, s, "--send", "--delete", "#eng", "1785712599.000100")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"missing_scope", "chat:write", "hersaid-setup"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
}

// The child must not receive the write credential. sez runs hear so that the
// read happens under a credential sez does not hold; handing over the parent
// environment gives it back — and findHear falls back to PATH, so the binary
// receiving it is not necessarily the one that was installed.
func TestTheReadSubprocessNeverSeesTheWriteToken(t *testing.T) {
	isolate(t)
	seedCache(t)
	fakeHear(t, `if [ -n "${SLACK_WRITE_TOKEN+set}" ]; then echo "LEAKED" >&2; exit 3; fi
echo '{"messages":[{"ts":"1785712599.000100","text":"safe"}]}'`)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
	if strings.Contains(stderr, "LEAKED") {
		t.Fatal("the read subprocess received SLACK_WRITE_TOKEN")
	}
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "safe") {
		t.Fatalf("stdout = %q, want the render to have worked without the token", stdout)
	}
}

// A message built from blocks carries text that is a fallback or empty, so
// showing it would show something its author never wrote — and Slack drops
// existing blocks from a text-only chat.update, so the replacement would not be
// what the message became. Neither verb may proceed on a partial view.
func TestAMessageThatCannotBeShownIsNotChanged(t *testing.T) {
	for _, verb := range []string{"--edit", "--delete"} {
		t.Run(verb, func(t *testing.T) {
			isolate(t)
			seedCache(t)
			fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"","partial":true}]}'`)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply("chat.update", `{"ok":true,"ts":"1.1","message":{"text":"x"}}`)
			s.Reply("chat.delete", `{"ok":true}`)

			args := []string{"--send", verb, "#eng", "1785712599.000100"}
			if verb == "--edit" {
				args = append(args, "replacement")
			}
			code, _, stderr := sez(t, s, args...)
			if code == 0 {
				t.Fatal("exit = 0, want a refusal")
			}
			if got := s.Hits(); got != 0 {
				t.Fatalf("Slack received %d requests, want 0", got)
			}
			if !strings.Contains(stderr, "blocks") {
				t.Fatalf("stderr = %q, want it to say why it refused", stderr)
			}
		})
	}
}

// Slack echoes the ts it updated, but envelope accepts an ok:true with none.
// The addressed ts was stamped on the intent line, and overwriting it with an
// empty echo drops it from the outcome — the invariant the delete path asserts
// would then silently not hold for edits.
func TestAnEditKeepsTheAddressedTSWhenSlackEchoesNone(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "before", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"message":{"text":"after"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	code, stdout, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "after")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "ts=1785712599.000100\n") {
		t.Fatalf("stdout = %q, want the addressed ts reported", stdout)
	}
	for _, r := range readLog(t, log) {
		if got := r["ts"]; got != "1785712599.000100" {
			t.Fatalf("ts = %v on a %v line, want the addressed message", got, r["phase"])
		}
	}
}

// An edit changes what a message says, and the record is left with no link to
// it though the ts and channel are both in hand — the same reasoning that makes
// a send fetch one.
func TestAnEditRecordsAPermalink(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "before", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"1785712599.000100","message":{"text":"after"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	if code, _, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "after"); code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	if got, _ := recs[len(recs)-1]["permalink"].(string); !strings.Contains(got, "/archives/") {
		t.Fatalf("permalink = %v, want a link to the edited message", recs[len(recs)-1]["permalink"])
	}
}

// The render is the whole safety surface for a destructive verb, and the send
// render's shape is already treated as a contract. This pins the other one.
func TestChangeRenderBlockShape(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubRead(t, "the current wording", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"edit", []string{"--edit", "#eng", "1785712599.000100", "the new wording"},
			"--- dry run: nothing sent ---\n" +
				"channel : #eng -> C0123ENG01\n" +
				"message : 1785712599.000100\n" +
				"--- current message ---\n" +
				"the current wording\n" +
				"--- replacement ---\n" +
				"the new wording\n" +
				"----------------------------------------\n"},
		// The irreversible verb, and the one the shape test exists for: no
		// replacement section, and the address line that catches a ts copied
		// from the wrong message.
		{"delete", []string{"--delete", "#eng", "1785712599.000100"},
			"--- dry run: nothing sent ---\n" +
				"channel : #eng -> C0123ENG01\n" +
				"message : 1785712599.000100\n" +
				"--- current message ---\n" +
				"the current wording\n" +
				"----------------------------------------\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := sez(t, s, tc.args...)
			if code != 0 {
				t.Fatalf("exit = %d (stderr %q)", code, stderr)
			}
			if stdout != tc.want {
				t.Fatalf("render =\n%q\nwant\n%q", stdout, tc.want)
			}
		})
	}
}

// requestTo returns the request made to one endpoint, so an assertion about it
// cannot be silently retargeted by a later call.
func requestTo(t *testing.T, s *slacktest.Server, endpoint string) slacktest.Request {
	t.Helper()
	for _, r := range s.Requests() {
		if r.Endpoint == endpoint {
			return r
		}
	}
	t.Fatalf("no request to %s", endpoint)
	return slacktest.Request{}
}

// The addressed ts is the argument the caller gave, not whatever Slack echoes.
// An echo that differed would leave the intent and outcome lines naming
// different messages and send the permalink lookup after the wrong one.
func TestASlackEchoNeverReplacesTheAddressedTS(t *testing.T) {
	log := isolate(t)
	seedCache(t)
	stubRead(t, "before", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.update", `{"ok":true,"ts":"9999999999.999999","message":{"text":"after"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.slack.test/archives/C0123ENG01/p12"}`)

	code, stdout, stderr := sez(t, s, "--send", "--edit", "#eng", "1785712599.000100", "after")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "ts=1785712599.000100\n") {
		t.Fatalf("stdout = %q, want the addressed ts, not Slack's echo", stdout)
	}
	for _, r := range readLog(t, log) {
		if got := r["ts"]; got != "1785712599.000100" {
			t.Fatalf("ts = %v on a %v line, want the addressed message", got, r["phase"])
		}
	}
	if got := requestTo(t, s, "chat.getPermalink").Body.Get("message_ts"); got != "" && got != "1785712599.000100" {
		t.Fatalf("permalink fetched for ts %q, want the addressed message", got)
	}
}
