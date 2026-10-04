package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// isolate stops a real credential, cache or audit log on the developer's
// machine reaching a test, and gives each test its own log to assert on.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("HERSAID_WRITE_CHANNELS", "")
	err := os.Unsetenv("HERSAID_WRITE_CHANNELS")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "state", "hersaid", "posts.jsonl")
}

// seedCache writes the channel cache hear builds, so that a #name resolves to
// an id instead of being passed through.
func seedCache(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "hersaid")
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"channels":[{"id":"C0123ENG01","name":"eng"}]}`
	err = os.WriteFile(filepath.Join(dir, cache.ConversationsFile), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// record is one audit line, decoded loosely so a missing field and a null one
// stay distinguishable.
type record map[string]any

func readLog(t *testing.T, path string) []record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []record
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r record
		err := json.Unmarshal([]byte(line), &r)
		if err != nil {
			t.Fatalf("audit line is not JSON: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

func phases(recs []record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		p, _ := r["phase"].(string)
		out = append(out, p)
	}
	return out
}

func sez(t *testing.T, s *slacktest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr, s.URL)
	return code, stdout.String(), stderr.String()
}

// sezStdin is sez with something on stdin.
func sezStdin(t *testing.T, s *slacktest.Server, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr, s.URL)
	return code, stdout.String(), stderr.String()
}

// sent returns the text field of the chat.postMessage the fake Slack received.
func sent(t *testing.T, s *slacktest.Server) string {
	t.Helper()
	for _, r := range s.Requests() {
		if r.Endpoint == "chat.postMessage" {
			return r.Body.Get("text")
		}
	}
	t.Fatal("no chat.postMessage was received")
	return ""
}

// delivers stubs a successful send, for a test whose subject is what was sent
// rather than what came back.
func delivers(s *slacktest.Server) {
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1785712596.256909","message":{"text":"stored"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://e.test/archives/C/p1"}`)
}

// With no write credential, writing is impossible and no HTTP
// request is issued at all — the refusal happens before a client exists.
func TestCredentialGateIssuesNoRequest(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "") // explicitly empty: absent, not fall-through
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.2"}`)

	code, stdout, stderr := sez(t, s, "--send", "#eng", "hello")

	if code == 0 {
		t.Fatalf("exit code = 0; a refused send must be non-zero (stdout %q)", stdout)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stderr, "no write credential found") {
		t.Fatalf("stderr = %q, want the gate's explanation", stderr)
	}
}

// An explicitly empty credential variable means absent, at the command
// boundary. Blanking the variable is the obvious way to rehearse the closed
// gate; if it fell through to the next source the rehearsal would send. A working token file is planted to make the fall-through
// observable.
func TestExplicitlyEmptyTokenIsTreatedAsAbsent(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hersaid")
	err := os.MkdirAll(cfg, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(cfg, "write.token"), []byte("xoxp-would-work"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.2"}`)

	code, _, stderr := sez(t, s, "--send", "#eng", "hello")

	if code == 0 {
		t.Fatal("exit code = 0; an explicitly empty token must close the gate")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0 — the empty variable fell through", got)
	}
	if !strings.Contains(stderr, "no write credential found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// Rendering and delivering are two separate acts: an instruction
// to "post X" authorises the action, never the unseen wording.
func TestRenderIsTheDefault(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.2"}`)

	code, stdout, stderr := sez(t, s, "#eng", "hello there")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests during a render, want 0", got)
	}
	for _, want := range []string{"dry run: nothing sent", "channel : #eng", "hello there"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	// A dry run is an attempt, and every attempt is logged.
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"dry-run"}) {
		t.Fatalf("audit phases = %v, want [dry-run]", got)
	}
	if recs[0]["dry_run"] != true || recs[0]["ok"] != true {
		t.Fatalf("dry-run record = %v", recs[0])
	}
}

// A message can be drafted and reviewed during a read-only period.
func TestRenderWorksWithNoCredential(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "#eng", "draft")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "token   : absent") {
		t.Fatalf("stdout = %q, want the absent-token notice", stdout)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// A value containing a parameter separator and a second field must
// arrive as one field value. The assertion is on the field-name set the server
// received, because a value check cannot see a value that split into two.
func TestParameterSmugglingIsImpossible(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1717171717.000100","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.invalid/archives/C/p1"}`)

	const smuggle = `hi&blocks=[{"type":"section"}]&channel=C9999EVIL`
	code, _, stderr := sez(t, s, "--send", "#eng", smuggle)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	post := s.Requests()[0]
	if post.Endpoint != "chat.postMessage" {
		t.Fatalf("first request went to %q", post.Endpoint)
	}
	if got, want := post.BodyFields(), []string{"channel", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
	if got := post.Body.Get("text"); got != smuggle {
		t.Fatalf("text = %q, want %q", got, smuggle)
	}
	if got := post.Body.Get("channel"); got != "#eng" {
		t.Fatalf("channel = %q; the smuggled channel= field must not have won", got)
	}
	if len(post.Query) != 0 {
		t.Fatalf("query string = %v, want empty — every field goes in the body", post.Query)
	}
}

// The ordering that matters: a durable intent record exists before
// any network write begins. The assertion runs inside the request handler, so it
// observes the log at exactly the moment the request arrives.
func TestIntentIsRecordedBeforeTheRequestBegins(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	var atRequest []record
	s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		atRequest = readLog(t, log)
		w.Write([]byte(`{"ok":true,"ts":"1.1","message":{"text":"hi"}}`))
	})
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	if code, _, stderr := sez(t, s, "--send", "#eng", "hi"); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, stderr)
	}

	if got := phases(atRequest); !reflect.DeepEqual(got, []string{"intent"}) {
		t.Fatalf("audit at the moment of the request = %v, want [intent]", got)
	}
	if atRequest[0]["ok"] != nil {
		t.Fatalf("intent ok = %v, want null — the outcome is not yet known", atRequest[0]["ok"])
	}
	if got := phases(readLog(t, log)); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("audit phases = %v, want [intent outcome]", got)
	}
}

// A transport failure after the request left is UNKNOWN,
// never failed: recording it as a failure is what invites the retry that
// duplicates a message Slack in fact delivered.
func TestPostSendTransportFailureIsRecordedAsUnknown(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Drop("chat.postMessage")

	code, _, stderr := sez(t, s, "--send", "#eng", "hi")

	if code == 0 {
		t.Fatal("exit code = 0; an unknown delivery must be reported")
	}
	if !strings.Contains(stderr, "UNKNOWN") {
		t.Fatalf("stderr = %q, want it to say the delivery is unknown", stderr)
	}
	if got := s.Hits(); got != 1 {
		t.Fatalf("Slack received %d requests, want exactly 1 — a write is never retried", got)
	}

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("audit phases = %v, want [intent outcome]", got)
	}
	if recs[1]["ok"] != nil {
		t.Fatalf("outcome ok = %v, want null; false would invite a duplicating retry", recs[1]["ok"])
	}
	if msg, _ := recs[1]["error"].(string); !strings.Contains(msg, "unknown") {
		t.Fatalf("outcome error = %q, want it to name the uncertainty", msg)
	}
}

// An error code that means something on another endpoint means nothing here.
// The success path reads a timestamp out of the response, and there is no
// response when Slack refused — so an outcome sez does not recognise must be a
// refusal, never a fall-through into "sent".
func TestAnUnrecognisedRefusalIsNotTreatedAsSent(t *testing.T) {
	for _, code := range []string{"already_reacted", "no_reaction"} {
		t.Run(code, func(t *testing.T) {
			log := isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			s.Reply("chat.postMessage", `{"ok":false,"error":"`+code+`"}`)

			exit, stdout, _ := sez(t, s, "--send", "#eng", "hi")

			if exit == 0 {
				t.Fatalf("exit code = 0 for a refused post (stdout %q)", stdout)
			}
			if strings.Contains(stdout, "sent to") {
				t.Fatalf("stdout = %q; nothing was sent", stdout)
			}
			recs := readLog(t, log)
			if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
				t.Fatalf("audit phases = %v", got)
			}
			if recs[1]["ok"] != false {
				t.Fatalf("outcome ok = %v, want false", recs[1]["ok"])
			}
		})
	}
}

// A Slack refusal is a definite failure: nothing was applied, so ok is false.
func TestAPIRefusalIsRecordedAsFailed(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":false,"error":"channel_not_found"}`)

	if code, _, _ := sez(t, s, "--send", "#eng", "hi"); code == 0 {
		t.Fatal("exit code = 0; a refused post must be non-zero")
	}
	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("audit phases = %v", got)
	}
	if recs[1]["ok"] != false {
		t.Fatalf("outcome ok = %v, want false", recs[1]["ok"])
	}
}

// Slack's own ambiguous codes are unknown too: the request reached it, and it
// may have acted before failing.
func TestSlackInternalErrorIsRecordedAsUnknown(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":false,"error":"internal_error"}`)

	if code, _, _ := sez(t, s, "--send", "#eng", "hi"); code == 0 {
		t.Fatal("exit code = 0")
	}
	recs := readLog(t, log)
	if recs[1]["ok"] != nil {
		t.Fatalf("outcome ok = %v, want null for internal_error", recs[1]["ok"])
	}
}

// The permalink is ancillary: the message is already delivered, so a failure
// there must not turn a success into an error or lose the outcome record.
func TestPermalinkFailureDoesNotFailTheSend(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":false,"error":"message_not_found"}`)

	code, stdout, stderr := sez(t, s, "--send", "#eng", "hi")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "sent to #eng") {
		t.Fatalf("stdout = %q", stdout)
	}
	recs := readLog(t, log)
	if recs[1]["ok"] != true {
		t.Fatalf("outcome ok = %v, want true", recs[1]["ok"])
	}
	if recs[1]["permalink"] != nil {
		t.Fatalf("permalink = %v, want null", recs[1]["permalink"])
	}
}

// The exit code for a lost record. A delivered message whose outcome record
// was lost is not a success: a failed audit append is fatal (exit 3), and an
// agent reading exit codes would never see a stderr line saying so.
// The delivery is still reported, because the message did go out.
//
// The log is closed while the request is in flight, which is the one moment
// that produces exactly this: a durable intent record, a delivery, and no way
// to write the record accounting for it.
func TestALostOutcomeRecordIsFatalAndStillReportsDelivery(t *testing.T) {
	logPath := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	wcred, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	s := slacktest.New(t)
	s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		err := log.Close()
		if err != nil {
			t.Errorf("closing the log: %v", err)
		}
		w.Write([]byte(`{"ok":true,"ts":"1785712596.256909","message":{"text":"hi"}}`))
	})
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	var stdout, stderr bytes.Buffer
	code := post(attempt{
		stdout: &stdout, stderr: &stderr, log: log, baseURL: s.URL, cred: wcred,
		target: resolve.Target{Input: "#eng", ID: "#eng", Name: "eng"},
	},
		audit.Post{Channel: "#eng", ChannelID: "#eng", Text: "hi"}, "", "hi")

	if code != 3 {
		t.Fatalf("exit code = %d, want 3 — a delivery nobody can account for is not a success", code)
	}
	if !strings.Contains(stdout.String(), "sent to #eng") {
		t.Fatalf("stdout = %q, want the delivery still reported", stdout.String())
	}
	if !strings.Contains(stderr.String(), "AUDIT WRITE FAILED") {
		t.Fatalf("stderr = %q, want it to name the lost record", stderr.String())
	}
	// The intent record survives: it was durable before the request left.
	if got := phases(readLog(t, logPath)); !reflect.DeepEqual(got, []string{"intent"}) {
		t.Fatalf("audit phases = %v, want [intent] — the outcome is the record that was lost", got)
	}
}

// The intent-before-request ordering, seen from the failing side: when the
// intent record cannot be written, nothing is sent at all.
//
// A delivered message whose intent never reached disk is exactly what the log
// exists to prevent — there would be no evidence anything was attempted — so
// the refusal comes before the request rather than after it, and exit 1 says
// the command refused rather than 3, which means "it happened and was lost".
//
// The log is closed before post() is called: the same instrument the
// lost-outcome test uses, moved one record earlier.
func TestAFailedIntentRecordRefusesToSend(t *testing.T) {
	logPath := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	wcred, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	err = log.Close()
	if err != nil {
		t.Fatal(err)
	}

	s := slacktest.New(t)
	// Stubbed so that a regression shows up as a delivered message rather than
	// as an unstubbed-endpoint complaint.
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1785712596.256909","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	var stdout, stderr bytes.Buffer
	code := post(attempt{
		stdout: &stdout, stderr: &stderr, log: log, baseURL: s.URL, cred: wcred,
		target: resolve.Target{Input: "#eng", ID: "#eng", Name: "eng"},
	},
		audit.Post{Channel: "#eng", ChannelID: "#eng", Text: "hi"}, "", "hi")

	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0 — no write may begin without a durable intent record", got)
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 — the command refused; nothing was left unaccounted for", code)
	}
	if !strings.Contains(stderr.String(), "refusing to send") {
		t.Fatalf("stderr = %q, want it to say the send was refused", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing reported as delivered", stdout.String())
	}
	if recs := readLog(t, logPath); len(recs) != 0 {
		t.Fatalf("audit records = %v, want none — the log is the thing that failed", recs)
	}
}

// The write allowlist is stronger than the credential gate because it survives
// a fully authorised send: the token works, and the target is still refused
// before any request.
func TestAllowlistRefusesATargetOutsideItWithNoRequest(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "#eng-standup,D0123ABCD")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1"}`)

	code, _, stderr := sez(t, s, "--send", "#general", "hi")

	if code == 0 {
		t.Fatal("exit code = 0; a target outside the allowlist must be refused")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
	if !strings.Contains(stderr, "HERSAID_WRITE_CHANNELS") {
		t.Fatalf("stderr = %q, want it to name the allowlist", stderr)
	}

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"refused"}) {
		t.Fatalf("audit phases = %v, want [refused] — no intent, because nothing was attempted", got)
	}
	if recs[0]["ok"] != false || recs[0]["channel"] != "#general" {
		t.Fatalf("refusal record = %v", recs[0])
	}
}

func TestAllowlistPermitsAListedTarget(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "#eng-standup")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	code, _, stderr := sez(t, s, "--send", "#eng-standup", "hi")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := s.Hits(); got != 2 {
		t.Fatalf("Slack received %d requests, want 2 (post + permalink)", got)
	}
	if got := phases(readLog(t, log)); !reflect.DeepEqual(got, []string{"intent", "outcome"}) {
		t.Fatalf("audit phases = %v", got)
	}
}

// An unset allowlist is no restriction: the feature is opt-in, and defaulting
// to closed would lock out everyone who never asked for it.
func TestUnsetAllowlistDoesNotRestrict(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1","message":{"text":"hi"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	if code, _, stderr := sez(t, s, "--send", "#anything", "hi"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
}

// The allowlist applies to a render too, so a refusal is discovered while
// rehearsing rather than at the moment of sending.
func TestAllowlistAppliesToARender(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	t.Setenv("HERSAID_WRITE_CHANNELS", "#eng")
	s := slacktest.New(t)

	if code, _, _ := sez(t, s, "#general", "hi"); code == 0 {
		t.Fatal("a render to a refused channel exited 0")
	}
	if got := phases(readLog(t, log)); !reflect.DeepEqual(got, []string{"refused"}) {
		t.Fatalf("audit phases = %v, want [refused]", got)
	}
}

// --md converts, and the conversion is upstream of everything: the same text is
// rendered, recorded and sent. Rendering what was typed while sending something
// else would defeat the point of reviewing the render.
func TestMarkdownIsConvertedOnceForEveryConsumer(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	const in = "# Title\n- **one**\nsee [docs](https://e.test/d)"
	const want = "*Title*\n• *one*\nsee <https://e.test/d|docs>"

	code, stdout, stderr := sez(t, s, "--send", "--md", "#eng", in)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != want {
		t.Errorf("text on the wire = %q, want %q", got, want)
	}
	if strings.Contains(stdout, in) {
		t.Errorf("stdout = %q; the unconverted text must not be what was reported", stdout)
	}
	recs := readLog(t, log)
	if got := recs[0]["text"]; got != want {
		t.Errorf("audit text = %q, want the converted text %q", got, want)
	}
}

// The render shows the converted text, so what is reviewed is what would go.
func TestMarkdownIsConvertedForARender(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "--md", "#eng", "**bold** and `**code**`")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "*bold* and `**code**`") {
		t.Fatalf("stdout = %q, want the converted text", stdout)
	}
	if got := readLog(t, log)[0]["text"]; got != "*bold* and `**code**`" {
		t.Errorf("audit text = %q, want the converted text", got)
	}
}

// Without --md the text is delivered exactly as given. The tool is used to
// paste code and logs, so converting by default would corrupt the common case.
func TestMarkdownIsNotConvertedByDefault(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	const in = "# Title\n- **one** __init__ [d](https://e.test)"
	if code, _, stderr := sez(t, s, "--send", "#eng", in); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != in {
		t.Fatalf("text on the wire = %q, want it byte for byte as given: %q", got, in)
	}
}

// The reason --md exists in a tool for pasting code: a fence survives it.
func TestMarkdownConversionLeavesCodeAlone(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	const in = "look:\n```\ndef __init__(self, **kw):\n    x = [a](b)\n```\n**do** read it"
	const want = "look:\n```\ndef __init__(self, **kw):\n    x = [a](b)\n```\n*do* read it"

	if code, _, stderr := sez(t, s, "--send", "--md", "#eng", in); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != want {
		t.Fatalf("text on the wire = %q, want %q", got, want)
	}
}

// Slack has no table syntax, so the pipes and dashes arrive as themselves. It is
// a warning and not a refusal: the message is still legible, and refusing would
// make the tool argue with its user about formatting.
func TestMarkdownTableWarnsAndStillSends(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"tight separator", "| a | b |\n|--|--|\n| 1 | 2 |"},
		{"spaced separator", "| a | b |\n| --- | --- |\n| 1 | 2 |"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
			s := slacktest.New(t)
			delivers(s)

			code, _, stderr := sez(t, s, "--send", "#eng", tc.text)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 — a table is warned about, not refused (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, "looks like a Markdown table") {
				t.Errorf("stderr = %q, want the table warning", stderr)
			}
			if got := sent(t, s); got != tc.text {
				t.Errorf("text on the wire = %q, want %q", got, tc.text)
			}
		})
	}
}

// The warning is keyed on a table's separator row, so ordinary prose containing
// a pipe or a dash does not trip it. A warning on every message would be read
// as noise and stop being read at all.
func TestOrdinaryTextDoesNotWarnAboutTables(t *testing.T) {
	for _, text := range []string{
		"a | b", "grep foo | wc -l", "-- a comment", "a|b|c", "| a | b |",
	} {
		isolate(t)
		t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
		s := slacktest.New(t)

		if _, _, stderr := sez(t, s, "#eng", text); strings.Contains(stderr, "Markdown table") {
			t.Errorf("sez %q warned about a table: %q", text, stderr)
		}
	}
}

// The warning goes to stderr, never stdout: stdout is the rendered message,
// which is read and often piped, and a warning inside it would be part of the
// text under review.
func TestTheTableWarningIsNotPartOfTheRender(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	_, stdout, stderr := sez(t, s, "#eng", "| a |\n|--|\n| 1 |")
	if strings.Contains(stdout, "warning") {
		t.Errorf("stdout = %q, want the warning on stderr alone", stdout)
	}
	if !strings.Contains(stderr, "looks like a Markdown table") {
		t.Errorf("stderr = %q, want the table warning", stderr)
	}
}

// --md runs before the table check, so a table built out of Markdown is still
// caught after conversion.
func TestATableIsDetectedAfterConversion(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	_, _, stderr := sez(t, s, "--md", "#eng", "# H\n| a | b |\n|--|--|")
	if !strings.Contains(stderr, "looks like a Markdown table") {
		t.Fatalf("stderr = %q, want the table warning", stderr)
	}
}

// A thread reply reaches the wire as thread_ts, and nothing else changes: the
// message is still one field alongside the channel.
func TestThreadReplyCarriesTheParentTimestamp(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	const parent = "1785712596.256909"
	code, stdout, stderr := sez(t, s, "--send", "--thread", parent, "#eng", "hi")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	post := s.Requests()[0]
	if got, want := post.BodyFields(), []string{"channel", "text", "thread_ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want %v", got, want)
	}
	if got := post.Body.Get("thread_ts"); got != parent {
		t.Errorf("thread_ts = %q, want %q", got, parent)
	}
	if !strings.Contains(stdout, "(thread "+parent+")") {
		t.Errorf("stdout = %q, want it to say the message went to a thread", stdout)
	}
	for _, r := range readLog(t, log) {
		if r["thread_ts"] != parent {
			t.Errorf("audit record %v, want thread_ts %q on every line of the attempt", r, parent)
		}
	}
}

// No --thread means no thread_ts field at all, rather than an empty one: an
// empty thread_ts is not what "post to the channel" means to Slack.
func TestNoThreadSendsNoThreadField(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	if code, _, stderr := sez(t, s, "--send", "#eng", "hi"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got, want := s.Requests()[0].BodyFields(), []string{"channel", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want %v", got, want)
	}
	for _, r := range readLog(t, log) {
		if r["thread_ts"] != nil {
			t.Errorf("audit thread_ts = %v, want null", r["thread_ts"])
		}
	}
}

// --thread with nothing after it is a usage error, not a post to the channel:
// the parent it was meant to reply to is the thing that went missing.
func TestThreadWithoutAValueIsAUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"--send", "--thread"},
		{"--send", "--thread", "", "#eng", "hi"},
	} {
		isolate(t)
		t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
		s := slacktest.New(t)

		code, stdout, stderr := sez(t, s, args...)
		if code != 2 {
			t.Errorf("sez %v: exit code = %d, want 2", args, code)
		}
		if stdout != "" {
			t.Errorf("sez %v: stdout = %q, want the diagnostic on stderr", args, stdout)
		}
		if !strings.Contains(stderr, "--thread") {
			t.Errorf("sez %v: stderr = %q, want it to name the option", args, stderr)
		}
		if got := s.Hits(); got != 0 {
			t.Errorf("sez %v: Slack received %d requests", args, got)
		}
	}
}

// The dry-run block, whole. Its shape is the oracle's: an agent or a human
// reads it to decide whether to send, so the lines and their order are part of
// the contract rather than incidental formatting.
func TestDryRunBlockShape(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	seedCache(t)
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "--thread", "1785712596.256909", "#eng", "hello\nthere")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	const want = `--- dry run: nothing sent ---
channel : #eng -> C0123ENG01
thread  : 1785712596.256909
--- message as Slack will receive it ---
hello
there
----------------------------------------
`
	if stdout != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout, want)
	}
}

// The absent-token line is what makes a render during a read-only period
// honest: the message is fine, and nothing could have delivered it.
func TestDryRunBlockNamesAnAbsentToken(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "")
	s := slacktest.New(t)

	code, stdout, stderr := sez(t, s, "#eng", "draft")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	const want = `--- dry run: nothing sent ---
channel : #eng
token   : absent (posting currently impossible)
--- message as Slack will receive it ---
draft
----------------------------------------
`
	if stdout != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", stdout, want)
	}
}

// With a credential the line is absent, so its presence means what it says.
func TestDryRunBlockOmitsTheTokenLineWhenOneExists(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	if _, stdout, _ := sez(t, s, "#eng", "draft"); strings.Contains(stdout, "token") {
		t.Fatalf("stdout = %q, want no token line when one resolved", stdout)
	}
}

// Text comes from stdin when no text argument is given, which is how a heredoc
// or a pipe reaches Slack without shell quoting mangling it.
func TestTextIsReadFromStdinWhenOmitted(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	const piped = "line one\nline two: **not** converted\n"
	if code, _, stderr := sezStdin(t, s, piped, "--send", "#eng"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	want := strings.TrimSuffix(piped, "\n")
	if got := sent(t, s); got != want {
		t.Fatalf("text on the wire = %q, want %q", got, want)
	}
}

// A piped message ends where its text ends. Trailing newlines — the ones every
// heredoc and every editor leaves behind — are dropped; keeping them would put
// a blank line on the wire, in the record
// and in the render — three places to notice it and none to fix it.
func TestStdinTrailingNewlinesAreDropped(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	if code, _, stderr := sezStdin(t, s, "hello\n\n\n", "--send", "#eng"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "hello" {
		t.Fatalf("text on the wire = %q, want %q", got, "hello")
	}
	recs := readLog(t, log)
	if got := recs[len(recs)-1]["text"]; got != "hello" {
		t.Fatalf("audit text = %q, want %q", got, "hello")
	}
}

// Interior blank lines are paragraph breaks somebody typed, and trailing spaces
// are text. Only the newlines at the very end go.
func TestStdinKeepsEverythingButTheTrailingNewlines(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	if code, _, stderr := sezStdin(t, s, "one\n\ntwo  \n", "--send", "#eng"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "one\n\ntwo  " {
		t.Fatalf("text on the wire = %q, want %q", got, "one\n\ntwo  ")
	}
}

// --md drops the trailing newlines too, including from text given as an
// argument.
func TestMarkdownConversionDropsTrailingNewlines(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	if code, _, stderr := sez(t, s, "--send", "--md", "#eng", "**bold**\n\n"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "*bold*" {
		t.Fatalf("text on the wire = %q, want %q", got, "*bold*")
	}
}

// The render is the last chance to see what is about to go, so it shows the
// text the wire will get and not a line more.
func TestARenderShowsThePipedTextWithoutATrailingBlankLine(t *testing.T) {
	isolate(t)
	s := slacktest.New(t)

	code, stdout, stderr := sezStdin(t, s, "hello\n", "#eng")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "receive it ---\nhello\n---") {
		t.Fatalf("render = %q, want the message with no blank line after it", stdout)
	}
}

// "-" is the explicit spelling of the same thing, and it wins: a text argument
// after it is ignored rather than silently concatenated, so a command whose
// author meant stdin cannot half-read a leftover argument instead.
func TestDashReadsStdinAndIgnoresLaterArguments(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	code, _, stderr := sezStdin(t, s, "from stdin", "--send", "#eng", "-", "ignored", "also ignored")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "from stdin" {
		t.Fatalf("text on the wire = %q, want %q", got, "from stdin")
	}
}

// Several text arguments are one message, joined with a single space. Without
// it, an unquoted sentence would arrive as its first word.
func TestTextArgumentsAreJoinedWithSpaces(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	if code, _, stderr := sez(t, s, "--send", "#eng", "one", "two", "three"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if got := sent(t, s); got != "one two three" {
		t.Fatalf("text on the wire = %q, want %q", got, "one two three")
	}
}

// An argument that is only whitespace is still empty. Stdin gets the same
// treatment, which is what stops an empty pipe posting a blank message.
func TestAnEmptyStdinIsRefused(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, _, stderr := sezStdin(t, s, "\n \t\n", "--send", "#eng")
	if code == 0 {
		t.Fatal("exit code = 0; an empty message must be refused")
	}
	if !strings.Contains(stderr, "empty message") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// Slack sanitises and truncates what it stores, so what it holds can differ
// from what was submitted. Both go in the record: the difference is the whole
// reason to log the second one.
func TestSlackTextRecordsWhatSlackStored(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage",
		`{"ok":true,"ts":"1.1","message":{"text":"see <https://e.test|docs>"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://e.test/p1"}`)

	const submitted = "see <https://e.test|docs>"
	if code, _, stderr := sez(t, s, "--send", "#eng", submitted); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	outcome := recs[len(recs)-1]
	if outcome["slack_text"] != "see <https://e.test|docs>" {
		t.Errorf("slack_text = %v, want what Slack stored", outcome["slack_text"])
	}
	if outcome["text"] != submitted {
		t.Errorf("text = %v, want what was submitted", outcome["text"])
	}
	// The intent line predates the reply, so it cannot have one.
	if recs[0]["slack_text"] != nil {
		t.Errorf("intent slack_text = %v, want null", recs[0]["slack_text"])
	}
}

// The difference is what makes the field worth having: a message Slack stored
// differently from the way it was submitted is visible in the log.
func TestSlackTextIsRecordedEvenWhenItDiffers(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1","message":{"text":"truncated…"}}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://e.test/p1"}`)

	if code, _, stderr := sez(t, s, "--send", "#eng", "the whole long message"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	outcome := recs[len(recs)-1]
	if outcome["slack_text"] != "truncated…" || outcome["text"] != "the whole long message" {
		t.Fatalf("record = %v, want both what was sent and what Slack kept", outcome)
	}
}

// A reply Slack sends without a message body leaves slack_text null rather than
// empty: the field says "Slack did not tell us", and "" would say it stored
// nothing.
func TestSlackTextIsNullWhenSlackReportsNone(t *testing.T) {
	log := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1"}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://e.test/p1"}`)

	if code, _, stderr := sez(t, s, "--send", "#eng", "hi"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	recs := readLog(t, log)
	if got := recs[len(recs)-1]["slack_text"]; got != nil {
		t.Fatalf("slack_text = %v, want null", got)
	}
}

func TestRefusesEmptyMessage(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	if code, _, _ := sez(t, s, "--send", "#eng", "   "); code == 0 {
		t.Fatal("exit code = 0; an empty message must be refused")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

func TestRefusesSuspiciousThreadTS(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	code, _, _ := sez(t, s, "--send", "--thread", "1.2&channel=C000", "#eng", "hi")
	if code == 0 {
		t.Fatal("exit code = 0; a malformed thread ts must be refused")
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

func TestRefusesAChannelThatLooksLikeAFlag(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)

	if code, _, stderr := sez(t, s, "--send", "--", "-rf", "hi"); code == 0 {
		t.Fatalf("exit code = 0; stderr %q", stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// A mode the audit log cannot be given is reported. tap has the mirror of this
// test: both commands append to the same file, so a log that cannot be kept at
// 0600 must be as visible from one as from the other.
//
// Provoking it needs a file this process can write to but cannot chmod, and the
// only portable one is a file it does not own: /dev/null. Writing a record to
// that file then fails as well, so the exit code and the log contents say
// nothing useful here and the warning is the whole assertion.
func TestAnUnenforceableLogModeIsReported(t *testing.T) {
	logPath := isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")

	err := os.MkdirAll(filepath.Dir(logPath), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Symlink(os.DevNull, logPath)
	if err != nil {
		t.Fatal(err)
	}
	// As root — a container's default — chmod succeeds and there is no warning
	// to report, which would leave this asserting nothing at all.
	probe, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if len(probe.Warnings()) == 0 {
		t.Skipf("this environment can chmod %s, so the warning cannot be provoked", os.DevNull)
	}

	s := slacktest.New(t)
	_, _, stderr := sez(t, s, "#eng", "hi")

	if !strings.Contains(stderr, "sez: warning") || !strings.Contains(stderr, "cannot enforce 0600") {
		t.Fatalf("stderr = %q, want the unenforceable mode reported", stderr)
	}
}

// --help and --version leave no trace at all.
//
// Asking a tool what it does must not enrol the user in
// anything, create a file, or reach a keychain — on macOS that is a modal
// unlock prompt, and on Linux a D-Bus call where there may be no Secret Service.
func TestHelpAndVersionTouchNothing(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version"} {
		t.Run(flag, func(t *testing.T) {
			home := isolateHome(t)
			consulted := watchCredential(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{flag}, refusingReader{t}, &stdout, &stderr, s.URL)

			if code != 0 {
				t.Errorf("exit code = %d, want 0 — it was asked for, so it is output", code)
			}
			if stdout.Len() == 0 {
				t.Error("nothing on stdout")
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want the answer on stdout alone", stderr.String())
			}
			if *consulted {
				t.Error("a credential was resolved; --help must never prompt a keychain")
			}
			if got := s.Hits(); got != 0 {
				t.Errorf("Slack received %d requests", got)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			} else if len(entries) != 0 {
				t.Errorf("left %v behind under HOME; state, cache and config must be untouched", entries)
			}
		})
	}
}

func TestHelpAndVersionSayDifferentThings(t *testing.T) {
	isolateHome(t)
	watchCredential(t)
	s := slacktest.New(t)

	var help, version bytes.Buffer
	run([]string{"--help"}, refusingReader{t}, &help, io.Discard, s.URL)
	run([]string{"--version"}, refusingReader{t}, &version, io.Discard, s.URL)

	// Every flag the command accepts is documented, including the ones the Go
	// version added: the exit codes, the allowlist and the timeout.
	for _, want := range []string{
		"--send", "--dry-run", "--md", "--thread", "--help", "--version",
		"HERSAID_WRITE_CHANNELS", "HERSAID_TIMEOUT", "SLACK_WRITE_TOKEN",
	} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("--help does not mention %q", want)
		}
	}
	if !strings.HasPrefix(version.String(), "sez ") {
		t.Errorf("--version = %q, want it to name the command", version.String())
	}
	if strings.Contains(version.String(), "usage") {
		t.Errorf("--version = %q, want the version alone", version.String())
	}
}

// The flags are answered by the option parser, so they are read with the arity
// the command actually has: --help after another option is help, and --help in
// a value position is that value.
func TestHelpIsReadWithTheRightArity(t *testing.T) {
	t.Run("after another option it is still help", func(t *testing.T) {
		isolateHome(t)
		consulted := watchCredential(t)
		s := slacktest.New(t)

		var stdout, stderr bytes.Buffer
		code := run([]string{"--send", "--help"}, refusingReader{t}, &stdout, &stderr, s.URL)

		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "usage:") {
			t.Errorf("stdout = %q, want the usage text", stdout.String())
		}
		if *consulted {
			t.Error("a credential was resolved")
		}
	})

	t.Run("in a value position it is that value", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
		s := slacktest.New(t)

		var stdout, stderr bytes.Buffer
		code := run([]string{"--send", "--thread", "--help", "#eng", "hi"},
			refusingReader{t}, &stdout, &stderr, s.URL)

		if code == 0 {
			t.Errorf("exit code = 0; --help was the thread ts, and it is not a timestamp")
		}
		if strings.Contains(stdout.String(), "usage:") {
			t.Errorf("stdout = %q; the option's value must not be read as a request for help", stdout.String())
		}
		if got := s.Hits(); got != 0 {
			t.Errorf("Slack received %d requests", got)
		}
	})
}

// A usage error is the other case: a diagnostic on stderr, exit 2.
func TestUsageErrorsAreDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, arg string }{
		{"unknown flag", "--sned"},
		{"a flag one letter off", "-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			s := slacktest.New(t)

			var stdout, stderr bytes.Buffer
			code := run([]string{tc.arg, "#eng", "hi"}, refusingReader{t}, &stdout, &stderr, s.URL)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want the diagnostic on stderr", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want the usage text", stderr.String())
			}
		})
	}
}

// sentinelToken is what a credential resolved behind the seam's back is worth.
const sentinelToken = "xoxp-sentinel-no-test-may-resolve-a-real-credential"

// isolateHome points every directory the commands use, including HOME itself,
// inside one temp directory — so anything created anywhere is visible as an
// entry in it.
//
// It also plants the sentinel in both token variables. watchCredential stubs
// the resolveWrite variable, which covers this file's code as written; code
// that called cred.ResolveWrite directly would walk straight past the stub to
// the keyring, and on a developer's machine — or anywhere SLACK_WRITE_TOKEN is
// exported — that would resolve, prompt, and quietly pass. The env var is the
// first source cred consults, so the sentinel wins instead: nothing real is
// reached, and the bypass surfaces as a request the assertions can see.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("SLACK_READ_TOKEN", sentinelToken)
	t.Setenv("SLACK_WRITE_TOKEN", sentinelToken)
	return home
}

// watchCredential reports whether the command resolved a credential. It answers
// as an unenrolled machine would rather than delegating: if the behaviour under
// test regresses, the test must fail, not open the developer's keychain.
func watchCredential(t *testing.T) *bool {
	t.Helper()
	var consulted bool
	prev := resolveWrite
	resolveWrite = func() (cred.Write, error) {
		consulted = true
		return cred.Write{}, cred.ErrNotFound
	}
	t.Cleanup(func() { resolveWrite = prev })
	return &consulted
}

// refusingReader fails the test if stdin is read. Reading it for --help would
// block on a pipe that nobody is going to write to.
type refusingReader struct{ t *testing.T }

func (r refusingReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// The mirror of cmd/hear's import-graph assertion. Without this, that test could
// pass because nothing anywhere imports slackwrite.
func TestSezDependsOnSlackwrite(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "internal/slackwrite") {
		t.Fatal("sez does not import slackwrite; cmd/hear's import-graph test would then be vacuous")
	}
	// The other direction, and the one --edit and --delete rest on: sez shows a
	// message before changing it by running the hear binary, a separate process
	// holding its own credential. Linking a reader instead would put read code
	// behind the write credential, and the argument for the subprocess is only
	// as good as what stops the next change from taking the shortcut.
	if strings.Contains(string(out), "internal/slackread") {
		t.Fatal("sez imports slackread; it must run the hear binary, not link a reader")
	}
}

// The subprocess below is steered by these variables. Only the test binary
// reads them, so the production binary keeps no base-URL override — an
// environment variable able to retarget a request carrying the write token is
// precisely the ambient configuration the transport is built to exclude.
const (
	helperEnv     = "HERSAID_TEST_HELPER"
	helperURLEnv  = "HERSAID_TEST_HELPER_URL"
	helperArgsEnv = "HERSAID_TEST_HELPER_ARGS"
)

// TestHelperProcess is not a real test: it is the body of the subprocess the
// signal tests drive.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	// The upload tests transfer to a local sink, which the production host set
	// does not permit. Only this test binary can reach here: the variable is
	// unexported and nothing in the shipped command reassigns it.
	uploadHosts = slackhosts.Allow("127.0.0.1", "localhost")
	code := run(strings.Split(os.Getenv(helperArgsEnv), "\x1f"),
		strings.NewReader(""), os.Stdout, os.Stderr, os.Getenv(helperURLEnv))
	os.Exit(code)
}

// startHelper runs sez's run() in a subprocess so a signal can be delivered to
// it without ending the test binary.
func startHelper(t *testing.T, s *slacktest.Server, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		helperURLEnv+"="+s.URL,
		helperArgsEnv+"="+strings.Join(args, "\x1f"),
		"SLACK_WRITE_TOKEN=xoxp-fake-write-token",
		"XDG_CONFIG_HOME="+os.Getenv("XDG_CONFIG_HOME"),
		"XDG_CACHE_HOME="+os.Getenv("XDG_CACHE_HOME"),
		"XDG_STATE_HOME="+os.Getenv("XDG_STATE_HOME"),
	)
	err := cmd.Start()
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// heldOpen stubs chat.postMessage as a request that arrives and never answers,
// so the send window can be interrupted deterministically.
func heldOpen(t *testing.T, s *slacktest.Server) chan struct{} {
	t.Helper()
	arrived := make(chan struct{})
	s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})
	return arrived
}

func waitFor(t *testing.T, cmd *exec.Cmd, arrived chan struct{}) {
	t.Helper()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the request never reached the fake Slack")
	}
}

// A catchable signal is still accounted for: SIGTERM inside the send window still
// leaves a record, and the process exits with the conventional 128+n code.
func TestSignalDuringTheSendWindowStillRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a subprocess")
	}
	log := isolate(t)
	s := slacktest.New(t)
	arrived := heldOpen(t, s)

	cmd := startHelper(t, s, "--send", "#eng", "hello")
	waitFor(t, cmd, arrived)

	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}

	var exitErr *exec.ExitError
	err = cmd.Wait()
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() = %v, want a non-zero exit", err)
	}
	if got := exitErr.ExitCode(); got != 143 {
		t.Fatalf("exit code = %d, want 143 (128 + SIGTERM)", got)
	}

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "abort"}) {
		t.Fatalf("audit phases = %v, want [intent abort]", got)
	}
	if recs[1]["ok"] != nil {
		t.Fatalf("abort ok = %v, want null — the message may have gone", recs[1]["ok"])
	}
}

// SIGKILL cannot be caught, so no outcome record can ever be guaranteed. What is
// guaranteed — and what this asserts — is that the intent record is already
// durable, so there is always evidence of what was about to go out.
func TestSigkillLeavesTheIntentRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a subprocess")
	}
	log := isolate(t)
	s := slacktest.New(t)
	arrived := heldOpen(t, s)

	cmd := startHelper(t, s, "--send", "#eng", "hello")
	waitFor(t, cmd, arrived)

	err := cmd.Process.Kill()
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent"}) {
		t.Fatalf("audit phases = %v, want exactly [intent]", got)
	}
	if recs[0]["text"] != "hello" || recs[0]["channel"] != "#eng" {
		t.Fatalf("intent record = %v, want it to say what was about to go out", recs[0])
	}
}

// A cold cache is not a dead end, and the message has to say so: sez holds a
// write credential only, so the user list it needs to address a person is one
// hear runs away and nothing sez does by itself will fill it.
func TestAColdCacheNamesTheCommandThatFillsIt(t *testing.T) {
	isolate(t) // no seedCache: the cache is cold
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	delivers(s)

	code, _, stderr := sez(t, s, "--send", "@sam", "hello")
	if code == 0 {
		t.Fatal("exit code = 0; nobody could be resolved")
	}
	if !strings.Contains(stderr, "hear dms") {
		t.Fatalf("stderr = %q, want it to name the command that fills the cache", stderr)
	}
	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// The send window ends when the outcome record is on disk, not when Slack
// answers. Between the two is the permalink fetch — a whole round trip — and a
// guard released before it would leave a message that definitely went out with
// nothing but an intent line to account for it.
func TestASignalDuringThePermalinkFetchStillRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a subprocess")
	}
	log := isolate(t)
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1785712596.256909","message":{"text":"hello"}}`)
	arrived := make(chan struct{})
	s.ReplyFunc("chat.getPermalink", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})

	cmd := startHelper(t, s, "--send", "#eng", "hello")
	waitFor(t, cmd, arrived)

	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	err = cmd.Wait()
	if !errors.As(err, &exitErr) {
		t.Fatalf("Wait() = %v, want a non-zero exit", err)
	}
	if got := exitErr.ExitCode(); got != 143 {
		t.Fatalf("exit code = %d, want 143 (128 + SIGTERM)", got)
	}

	recs := readLog(t, log)
	if got := phases(recs); !reflect.DeepEqual(got, []string{"intent", "abort"}) {
		t.Fatalf("audit phases = %v, want [intent abort]", got)
	}
	if recs[1]["ok"] != nil {
		t.Fatalf("abort ok = %v, want null — the outcome was never recorded", recs[1]["ok"])
	}
}

// missing_scope is the one Slack refusal with a fix the user can act on, and
// the bare code names neither the scope nor where to add it. tap already says
// so for reactions:write; a send refused for chat:write left the reader with
// "missing_scope  (logged)" and nowhere to go.
func TestMissingScopeExplainsTheFix(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":false,"error":"missing_scope"}`)

	code, _, stderr := sez(t, s, "--send", "#eng", "hello")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"missing_scope", "chat:write", "hersaid-setup"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to mention %q", stderr, want)
		}
	}
}

// Only that refusal gets the extra paragraph. Printing it for every failure
// would send someone editing OAuth scopes over a message refused for an
// entirely different reason.
func TestOtherRefusalsGetNoScopeAdvice(t *testing.T) {
	isolate(t)
	t.Setenv("SLACK_WRITE_TOKEN", "xoxp-fake-write-token")
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":false,"error":"channel_not_found"}`)

	code, _, stderr := sez(t, s, "--send", "#eng", "hello")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(stderr, "hersaid-setup") {
		t.Fatalf("stderr = %q, want no scope advice for an unrelated refusal", stderr)
	}
}
