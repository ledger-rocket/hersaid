package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// stubRead replaces the read that pairs sez with hear. sez holds the write
// credential and the write app has no history scopes, so it cannot read a
// message itself — it asks the hear binary, which holds its own.
func stubRead(t *testing.T, text string, err error) {
	t.Helper()
	stubReadFull(t, current{Text: text}, err)
}

// stubReadFull is stubRead for a test that cares what the message carried
// besides its text.
func stubReadFull(t *testing.T, now current, err error) {
	t.Helper()
	prev := readMessage
	readMessage = func(context.Context, string, string, verb) (current, error) { return now, err }
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
	fakeHear(t, `echo '{"messages":[{"ts":"1.1","text":"other","blocks":false},{"ts":"1785712599.000100","text":"wanted","blocks":false}]}'`)
	got, err := viaHear(context.Background(), "C1", "1785712599.000100", verbEdit)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "wanted" {
		t.Fatalf("text = %q, want the message whose ts matched", got.Text)
	}
}

// A message carrying files can be deleted: the files are named, so the render
// shows what goes. This is the case sez --file creates, and refusing it meant
// the tool could post a message it could not remove.
func TestADeleteReadsAMessageThatCarriesFiles(t *testing.T) {
	fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"look at these","blocks":false,`+
		`"files":[{"name":"trace.pdf"},{"name":"out.log"}]}]}'`)
	got, err := viaHear(context.Background(), "C1", "1785712599.000100", verbDelete)
	if err != nil {
		t.Fatalf("viaHear() error = %v; a delete may proceed on a message with files", err)
	}
	if got.Text != "look at these" {
		t.Fatalf("text = %q", got.Text)
	}
	if len(got.Files) != 2 || got.Files[0].Name != "trace.pdf" || got.Files[1].Name != "out.log" {
		t.Fatalf("files = %+v, want both named so the render can show them", got.Files)
	}
}

// An edit may not. chat.update carries text alone and whether the shares
// survive it is not established, so an edit could silently detach the
// attachments — which is what this gate exists to prevent.
func TestAnEditStillRefusesAMessageThatCarriesFiles(t *testing.T) {
	fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"look","blocks":false,`+
		`"files":[{"name":"trace.pdf"}]}]}'`)
	_, err := viaHear(context.Background(), "C1", "1785712599.000100", verbEdit)
	if err == nil {
		t.Fatal("viaHear() = nil error, want an edit refused")
	}
	if !strings.Contains(err.Error(), "cannot edit") {
		t.Fatalf("error = %v, want it to say an edit is what is refused", err)
	}
}

// Blocks are refused for either verb: they read as fallback text or as nothing,
// so a render of them would show something the author never wrote.
func TestBlocksAreStillRefusedForBoth(t *testing.T) {
	for _, v := range []verb{verbEdit, verbDelete} {
		fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"fallback","blocks":true}]}'`)
		_, err := viaHear(context.Background(), "C1", "1785712599.000100", v)
		if err == nil {
			t.Fatalf("verb %v: viaHear() = nil error, want blocks refused", v)
		}
		if !strings.Contains(err.Error(), "blocks") {
			t.Fatalf("verb %v: error = %v", v, err)
		}
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
echo '{"messages":[{"ts":"1785712599.000100","text":"safe","blocks":false}]}'`)
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
			fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"","blocks":true}]}'`)
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

// An older hear says a message is partial without saying why. sez cannot tell a
// file it could show from a block it could not, so it refuses rather than
// guessing: findHear falls back to PATH, so the two binaries are not always
// upgraded together, and a gate that read a missing field as false would be off
// exactly when the pair is mismatched.
func TestAnOlderHearThatCannotSayWhyIsRefused(t *testing.T) {
	for _, v := range []verb{verbEdit, verbDelete} {
		fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"fallback","partial":true}]}'`)
		_, err := viaHear(context.Background(), "C1", "1785712599.000100", v)
		if err == nil {
			t.Fatalf("verb %v: viaHear() = nil error; an unexplained partial must be refused", v)
		}
		if !strings.Contains(err.Error(), "too old") {
			t.Fatalf("verb %v: error = %v, want it to name the mismatch", v, err)
		}
	}
}

// A hear old enough to omit the field is refused even when it reports nothing
// unrenderable, because that is exactly what the dangerous case looks like: a
// hear from before files were known reports a message carrying them as plain,
// with no files array and partial false. It is indistinguishable from a sound
// reply about an ordinary message, so the field's absence has to be the answer.
func TestAnOlderHearIsRefusedEvenWhenItClaimsNothing(t *testing.T) {
	for _, v := range []verb{verbEdit, verbDelete} {
		fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"plain"}]}'`)
		_, err := viaHear(context.Background(), "C1", "1785712599.000100", v)
		if err == nil {
			t.Fatalf("verb %v: viaHear() = nil error; a hear that cannot describe content must be refused", v)
		}
		if !strings.Contains(err.Error(), "too old") {
			t.Fatalf("verb %v: error = %v, want it to name the mismatch", v, err)
		}
	}
}

// A name of nothing but control characters is empty once it is safe to print,
// and the account of what a delete removes must not be a blank line.
func TestAFileWithNoPrintableNameFallsBackToItsID(t *testing.T) {
	hostile := string(rune(0x202e)) + string(rune(0x200e))
	fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"look","blocks":false,`+
		`"files":[{"id":"F0123ABCD","name":"`+hostile+`"}]}]}'`)
	got, err := viaHear(context.Background(), "C1", "1785712599.000100", verbDelete)
	if err != nil {
		t.Fatal(err)
	}
	// The raw name is kept; the id is what the render falls back to.
	if len(got.Files) != 1 || got.Files[0].ID != "F0123ABCD" {
		t.Fatalf("files = %+v, want the id carried through", got.Files)
	}
	if label := fileLabel(got.Files[0]); label != "F0123ABCD" {
		t.Fatalf("fileLabel = %q, want the id where the name cannot be shown", label)
	}
}

// The delete render is the whole safety surface for the irreversible verb, and
// the file lines are the part this change added. Pinned byte for byte: a
// regression that dropped them would leave the render claiming to account for
// the message while saying nothing about the attachments going with it.
func TestTheDeleteRenderNamesEveryFile(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubReadFull(t, current{
		Text:  "look at these",
		Files: []audit.File{{Name: "trace.pdf"}, {Name: "out.log"}},
	}, nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	_, stdout, _ := sez(t, s, "--delete", "#eng", "1785712599.000100")
	want := "--- dry run: nothing sent ---\n" +
		"channel : #eng -> C0123ENG01\n" +
		"message : 1785712599.000100\n" +
		"--- current message ---\n" +
		"look at these\n" +
		"files   : trace.pdf\n" +
		"          out.log\n" +
		"note    : the files stay in Slack; deleting a message never removes them\n" +
		"----------------------------------------\n"
	if stdout != want {
		t.Fatalf("render =\n%q\nwant\n%q", stdout, want)
	}
}

// The names go through termsafe on the way to a terminal, exactly as a
// filename does everywhere else in this tool.
func TestTheDeleteRenderCannotBeDrivenByAFilename(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubReadFull(t, current{
		Text:  "look",
		Files: []audit.File{{Name: "in" + string(rune(0x1b)) + "[31mnocent" + string(rune(0x202e)) + ".pdf"}},
	}, nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
	// Asserting only that the hostile runes are absent would pass on an empty
	// render, which is the one outcome that proves nothing: the render has to
	// have happened and shown the name.
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "nocent") {
		t.Fatalf("stdout = %q, want the sanitised name rendered", stdout)
	}
	for _, banned := range []rune{0x1b, 0x202e} {
		if strings.ContainsRune(stdout, banned) {
			t.Errorf("the render carries %U from a filename", banned)
		}
	}
}

// The record names every file whose share the delete removes. Without it they
// would survive in neither the message nor the log.
func TestADeleteRecordsTheFilesItRemoved(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	stubReadFull(t, current{Text: "look", Files: []audit.File{{Name: "trace.pdf"}, {Name: "out.log"}}}, nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true}`)

	if code, _, stderr := sez(t, s, "--send", "--delete", "#eng", "1785712599.000100"); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	recs := readLog(t, logPath)
	last := recs[len(recs)-1]
	files, ok := last["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %#v, want both names recorded", last["files"])
	}
	for i, want := range []string{"trace.pdf", "out.log"} {
		m := files[i].(map[string]any)
		if got, _ := m["name"].(string); got != want {
			t.Errorf("files[%d].name = %v, want %s", i, m["name"], want)
		}
	}
}

// The forward twin of the version-skew guard. A later hear could report a kind
// of unrenderable content this sez has never heard of — partial set, no blocks,
// no files to explain it — and that is the same unshowable message the gate
// exists to refuse, arriving from the future rather than the past.
func TestAHearThatKnowsSomethingThisSezDoesNotIsRefused(t *testing.T) {
	for _, v := range []verb{verbEdit, verbDelete} {
		fakeHear(t, `echo '{"messages":[{"ts":"1785712599.000100","text":"fallback",`+
			`"partial":true,"blocks":false}]}'`)
		_, err := viaHear(context.Background(), "C1", "1785712599.000100", v)
		if err == nil {
			t.Fatalf("verb %v: viaHear() = nil error; an unexplained partial must be refused", v)
		}
		if !strings.Contains(err.Error(), "update sez") {
			t.Fatalf("verb %v: error = %v, want it to name the direction of the mismatch", v, err)
		}
	}
}

// A file's name reaches the record as Slack holds it. termsafe is for display:
// a name carrying a stripped rune is recorded whole, since JSON carries it
// safely and the log is meant to say what Slack has rather than what printed.
func TestTheRecordKeepsTheNameSlackHolds(t *testing.T) {
	logPath := isolate(t)
	seedCache(t)
	raw := "in" + string(rune(0x202e)) + "voice.pdf"
	stubReadFull(t, current{Text: "look", Files: []audit.File{{Name: raw, ID: "F0123ABCD"}}}, nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.delete", `{"ok":true}`)

	if code, _, stderr := sez(t, s, "--send", "--delete", "#eng", "1785712599.000100"); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	recs := readLog(t, logPath)
	files, ok := recs[len(recs)-1]["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files = %#v, want the one file", recs[len(recs)-1]["files"])
	}
	m := files[0].(map[string]any)
	if got, _ := m["name"].(string); got != raw {
		t.Fatalf("recorded name = %q, want the raw name Slack holds", got)
	}
	if got, _ := m["id"].(string); got != "F0123ABCD" {
		t.Fatalf("recorded id = %q, want the id that reconciles against files.info", got)
	}
}

// A message is a share of a file, not the file. Deleting the message takes the
// share and leaves the file in the workspace — true through Slack's own
// interface as well as through chat.delete, so this is not a difference between
// this tool and Slack. It is said at the moment somebody is deciding, because
// almost nobody expects it.
func TestTheDeleteRenderSaysTheFilesOutliveTheMessage(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubReadFull(t, current{Text: "look", Files: []audit.File{{Name: "trace.pdf"}}}, nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "--delete", "#eng", "1785712599.000100")
	if code != 0 {
		t.Fatalf("exit = %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "the files stay in Slack") {
		t.Fatalf("render = %q, want it to say the files outlive the message", stdout)
	}
}

// A message with nothing attached says nothing about files.
func TestADeleteWithNoFilesSaysNothingAboutThem(t *testing.T) {
	isolate(t)
	seedCache(t)
	stubRead(t, "just words", nil)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	_, stdout, _ := sez(t, s, "--delete", "#eng", "1785712599.000100")
	if strings.Contains(stdout, "stay in Slack") {
		t.Fatalf("render = %q, want no note where there are no files", stdout)
	}
}
