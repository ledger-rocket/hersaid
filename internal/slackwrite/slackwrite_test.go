package slackwrite

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// writeCred mints a cred.Write for tests. The token field is unexported, so a
// test in this package is the only place that can — which is the point: no
// other package can turn a string it chose into a write credential.
func writeCred(t *testing.T, token string) cred.Write {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SLACK_WRITE_TOKEN", token)
	c, err := cred.ResolveWrite()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func client(t *testing.T, s *slacktest.Server) *Client {
	t.Helper()
	return New(writeCred(t, "xoxp-fake-write-token"), slackhttp.New(2*time.Second), s.URL)
}

// The request carries exactly the fields the endpoint declares.
// The assertion is on the field-name set the server received, not on one
// value: a value that split into two fields is invisible to a value check.
func TestPostMessageSendsExactlyTheDeclaredFields(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1","message":{"text":"stored"}}`)

	// Every argument carries a parameter separator and a field Slack honours.
	// `blocks` is the dangerous one: Slack renders it in place of `text`.
	_, err := client(t, s).PostMessage(context.Background(),
		"C0123ABCD&blocks=[{\"type\":\"section\"}]",
		"hi&blocks=[{}]&channel=C9999EVIL",
		"")
	if err != nil {
		t.Fatal(err)
	}

	req := s.Last()
	if got, want := req.BodyFields(), []string{"channel", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("body fields = %v, want exactly %v", got, want)
	}
	if got := req.Body.Get("text"); got != "hi&blocks=[{}]&channel=C9999EVIL" {
		t.Fatalf("text = %q; the separators must not have split it", got)
	}
	if got := req.Body.Get("channel"); got != "C0123ABCD&blocks=[{\"type\":\"section\"}]" {
		t.Fatalf("channel = %q", got)
	}
	if len(req.Query) != 0 {
		t.Fatalf("query string = %v, want empty — every field goes in the body", req.Query)
	}
}

// No request at all, structurally. cred.Write{} is constructible in any
// package and ResolveWrite returns one alongside its error, so a caller that
// mishandled that error must still issue no request at all — an `Authorization: Bearer `
// for Slack to reject is not the guarantee, and it puts the target channel and
// the message text on someone else's wire.
func TestZeroCredentialIssuesNoRequest(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1"}`)
	s.Reply("reactions.add", `{"ok":true}`)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://x/p1"}`)

	c := New(cred.Write{}, slackhttp.New(2*time.Second), s.URL)

	_, err := c.PostMessage(context.Background(), "C0123ABCD", "hi", "")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("PostMessage error = %v, want ErrNoCredential", err)
	}
	var pre *PreflightError
	if !errors.As(err, &pre) {
		t.Fatalf("error = %#v, want a *PreflightError — nothing left the process", err)
	}
	// Rejected, not Ambiguous: no request was issued, so nothing was applied.
	if got := Classify(err); got != Rejected {
		t.Fatalf("Classify = %v, want Rejected", got)
	}
	err = c.AddReaction(context.Background(), "C0123ABCD", "1.1", "thumbsup")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("AddReaction error = %v, want ErrNoCredential", err)
	}
	_, err = c.Permalink(context.Background(), "C0123ABCD", "1.1")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Permalink error = %v, want ErrNoCredential", err)
	}

	if got := s.Hits(); got != 0 {
		t.Fatalf("Slack received %d requests, want 0", got)
	}
}

// thread_ts appears only when a thread was asked for, so an ordinary message is
// not silently threaded.
func TestThreadTSIsOmittedWhenEmpty(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1"}`)

	c := client(t, s)
	_, err := c.PostMessage(context.Background(), "C1", "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Last().BodyFields(), []string{"channel", "text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}

	_, err = c.PostMessage(context.Background(), "C1", "hi", "1785712596.256909")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Last().BodyFields(), []string{"channel", "text", "thread_ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
}

func TestPostMessageReturnsWhatSlackStored(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1785712599.000100","message":{"text":"hi &amp; bye"}}`)

	got, err := client(t, s).PostMessage(context.Background(), "C1", "hi & bye", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.TS != "1785712599.000100" {
		t.Fatalf("TS = %q", got.TS)
	}
	// Slack sanitises; logging both is what makes the difference auditable.
	if got.Text != "hi &amp; bye" {
		t.Fatalf("Text = %q, want what Slack stored", got.Text)
	}
}

func TestReactionsSendExactlyTheDeclaredFields(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":true}`)
	s.Reply("reactions.remove", `{"ok":true}`)

	c := client(t, s)
	err := c.AddReaction(context.Background(), "C1", "1.1", "thumbsup")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Last().BodyFields(), []string{"channel", "name", "timestamp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions.add fields = %v, want %v", got, want)
	}
	if s.Last().Endpoint != "reactions.add" {
		t.Fatalf("endpoint = %q", s.Last().Endpoint)
	}

	err = c.RemoveReaction(context.Background(), "C1", "1.1", "thumbsup")
	if err != nil {
		t.Fatal(err)
	}
	if s.Last().Endpoint != "reactions.remove" {
		t.Fatalf("endpoint = %q", s.Last().Endpoint)
	}
}

func TestPermalink(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("chat.getPermalink", `{"ok":true,"permalink":"https://example.invalid/archives/C1/p1"}`)

	got, err := client(t, s).Permalink(context.Background(), "C1", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.invalid/archives/C1/p1" {
		t.Fatalf("permalink = %q", got)
	}
	if got, want := s.Last().BodyFields(), []string{"channel", "message_ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
}

// A write is attempted at most once per invocation. Nothing in
// this package retries, whatever goes wrong.
func TestWriteIsAttemptedAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*slacktest.Server)
	}{
		{"connection dropped mid-request", func(s *slacktest.Server) { s.Drop("chat.postMessage") }},
		{"server error", func(s *slacktest.Server) {
			s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
			})
		}},
		{"rate limited", func(s *slacktest.Server) {
			s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := slacktest.New(t)
			tc.setup(s)

			_, err := client(t, s).PostMessage(context.Background(), "C1", "hi", "")
			if err == nil {
				t.Fatal("PostMessage succeeded; this case must fail")
			}
			if got := s.Hits(); got != 1 {
				t.Fatalf("Slack received %d requests, want exactly 1 — a write is never retried", got)
			}
		})
	}
}

// A dropped connection is the only situation in which a delivered write can
// look like a failure, so it must be typed apart from a refusal.
func TestDroppedConnectionIsATransportError(t *testing.T) {
	s := slacktest.New(t)
	s.Drop("chat.postMessage")

	_, err := client(t, s).PostMessage(context.Background(), "C1", "hi", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("error = %#v, want a *TransportError", err)
	}
	if Classify(err) != Ambiguous {
		t.Fatalf("Classify = %v, want Ambiguous", Classify(err))
	}
}

func TestAPIErrorCarriesTheCode(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("reactions.add", `{"ok":false,"error":"already_reacted"}`)

	err := client(t, s).AddReaction(context.Background(), "C1", "1.1", "thumbsup")
	if got := Code(err); got != CodeAlreadyReacted {
		t.Fatalf("Code = %q, want %q", got, CodeAlreadyReacted)
	}
	if Classify(err) != AlreadyDone {
		t.Fatalf("Classify = %v, want AlreadyDone", Classify(err))
	}
}

// The classification must be complete on its own: already_reacted and
// no_reaction mean the desired state holds, and a caller that switches on the
// Outcome without first inspecting Code must reach that answer. A fourth
// Outcome is what makes forgetting impossible.
func TestIdempotentCodesAreNotRefusals(t *testing.T) {
	for _, tc := range []struct{ method, code string }{
		{methodAddReaction, CodeAlreadyReacted},
		{methodRemoveReaction, CodeNoReaction},
	} {
		err := &APIError{Method: tc.method, Code: tc.code}
		if got := Classify(err); got != AlreadyDone {
			t.Errorf("Classify(%s: %s) = %v, want AlreadyDone", tc.method, tc.code, got)
		}
	}
}

// An idempotent code is only idempotent for the endpoint that asks for that end
// state. "already_reacted" from chat.postMessage is an answer nobody should
// have received: nothing was posted, so calling it a success would report a
// message as sent that Slack refused — and leave the caller with no response to
// read a timestamp from.
func TestIdempotentCodesAreScopedToTheirEndpoint(t *testing.T) {
	for _, tc := range []struct{ method, code string }{
		{methodPostMessage, CodeAlreadyReacted},
		{methodPostMessage, CodeNoReaction},
		{methodPermalink, CodeAlreadyReacted},
		{methodAddReaction, CodeNoReaction},
		{methodRemoveReaction, CodeAlreadyReacted},
	} {
		err := &APIError{Method: tc.method, Code: tc.code}
		if got := Classify(err); got != Rejected {
			t.Errorf("Classify(%s: %s) = %v, want Rejected", tc.method, tc.code, got)
		}
	}
}

func TestMalformedReplyIsAmbiguous(t *testing.T) {
	s := slacktest.New(t)
	s.ReplyFunc("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>gateway timeout</html>"))
	})

	_, err := client(t, s).PostMessage(context.Background(), "C1", "hi", "")
	if got := Code(err); got != CodeMalformed {
		t.Fatalf("Code = %q, want %q", got, CodeMalformed)
	}
	// Slack may have acted before the reply was mangled.
	if Classify(err) != Ambiguous {
		t.Fatalf("Classify = %v, want Ambiguous", Classify(err))
	}
}

// The classification is the safety property: a possible delivery must never be
// recorded as a definite failure, because that is what invites the retry that
// duplicates the message.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Outcome
	}{
		{"success", nil, Delivered},
		{"slack refused it", &APIError{Method: "chat.postMessage", Code: "channel_not_found"}, Rejected},
		{"missing scope", &APIError{Method: "reactions.add", Code: CodeMissingScope}, Rejected},
		{"already reacted", &APIError{Method: "reactions.add", Code: CodeAlreadyReacted}, AlreadyDone},
		{"no reaction to remove", &APIError{Method: "reactions.remove", Code: CodeNoReaction}, AlreadyDone},
		{
			"an idempotent code from the wrong endpoint",
			&APIError{Method: "chat.postMessage", Code: CodeAlreadyReacted}, Rejected,
		},
		{"no credential", &PreflightError{Method: "chat.postMessage", Err: ErrNoCredential}, Rejected},
		{"slack internal error", &APIError{Method: "chat.postMessage", Code: "internal_error"}, Ambiguous},
		{"slack fatal error", &APIError{Method: "chat.postMessage", Code: "fatal_error"}, Ambiguous},
		{"malformed reply", &APIError{Method: "chat.postMessage", Code: CodeMalformed}, Ambiguous},
		{"nothing left the process", &PreflightError{Method: "chat.postMessage", Err: errors.New("bad url")}, Rejected},
		{"transport failure", &TransportError{Method: "chat.postMessage", Err: errors.New("EOF")}, Ambiguous},
		{"an error nobody typed", errors.New("boom"), Ambiguous},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The base URL is a constructor parameter, so no code path can retarget a
// request carrying the write token without saying so at the call site.
func TestBaseURLIsPerClient(t *testing.T) {
	if !strings.HasPrefix(SlackAPI, "https://") {
		t.Fatalf("SlackAPI = %q, want an https URL", SlackAPI)
	}
	s := slacktest.New(t)
	s.Reply("chat.postMessage", `{"ok":true,"ts":"1.1"}`)
	_, err := client(t, s).PostMessage(context.Background(), "C1", "hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Hits(); got != 1 {
		t.Fatalf("the fake received %d requests, want 1", got)
	}
}
