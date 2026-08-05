package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

func header(values ...string) http.Header {
	h := http.Header{}
	for _, v := range values {
		h.Add(scopeHeader, v)
	}
	return h
}

// The scope check on its own, in every shape Slack's header arrives in.
func TestCheckScopes(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		role   role
		want   string
	}{
		{
			name:   "a read token for the read role",
			header: header("channels:history,users:read"),
			role:   readRole,
		},
		{
			name:   "a write token for the write role",
			header: header("channels:read,chat:write"),
			role:   writeRole,
		},
		{
			name:   "a write token for the read role",
			header: header("channels:read,chat:write"),
			role:   readRole,
			want:   "carries chat:write",
		},
		{
			name:   "a read token for the write role",
			header: header("channels:history,users:read"),
			role:   writeRole,
			want:   "does not carry chat:write",
		},
		{
			name:   "spaces around the scopes",
			header: header("channels:read , chat:write , users:read"),
			role:   writeRole,
		},
		{
			name:   "split across repeated headers",
			header: header("channels:read", "chat:write"),
			role:   writeRole,
		},
		// A different grant with the same prefix. Matching by substring would
		// admit a read token that only carries chat:write.public — or refuse a
		// write token that carries no plain chat:write at all.
		{
			name:   "a neighbouring scope is not chat:write",
			header: header("channels:read,chat:write.public"),
			role:   readRole,
		},
		{
			name:   "a neighbouring scope does not satisfy the write role",
			header: header("channels:read,chat:write.public"),
			role:   writeRole,
			want:   "does not carry chat:write",
		},
		// Fail closed, in both directions. Storing it with a warning is how a
		// read token that can post gets enrolled as one that cannot.
		{
			name:   "no header at all, read role",
			header: http.Header{},
			role:   readRole,
			want:   "no x-oauth-scopes header",
		},
		{
			name:   "no header at all, write role",
			header: http.Header{},
			role:   writeRole,
			want:   "no x-oauth-scopes header",
		},
		{
			name:   "a header that names no scopes",
			header: header(" , , "),
			role:   readRole,
			want:   "no x-oauth-scopes header",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkScopes(tc.header, tc.role)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a token it should accept: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("accepted a token it should refuse (wanted %q)", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// The header name is matched however Slack spells it on the wire. This goes
// through a
// real response, because http.Header.Set canonicalises the key on the way in
// and a test that used it would be asserting nothing: assigning the map entry
// directly is what puts a genuinely lower-cased name on the wire.
func TestScopeHeaderNameIsMatchedHoweverSlackSpellsIt(t *testing.T) {
	s := slacktest.New(t)
	s.ReplyFunc("auth.test", func(w http.ResponseWriter, r *http.Request) {
		w.Header()["x-oauth-scopes"] = []string{"channels:read,chat:write"}
		io.WriteString(w, okBody)
	})

	if _, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, writeToken, writeRole); err != nil {
		t.Fatalf("a lower-case header name was not read: %v", err)
	}
}

func TestVerifyReportsTheAccount(t *testing.T) {
	s := slack(t, readScopes, okBody)

	acct, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, readToken, readRole)
	if err != nil {
		t.Fatal(err)
	}
	if acct.user != "stepan" || acct.team != "LedgerRocket" {
		t.Fatalf("account = %+v", acct)
	}
	if got := s.Last().Auth; got != "Bearer "+readToken {
		t.Errorf("Authorization = %q", got)
	}
	if got := s.Last().Endpoint; got != "auth.test" {
		t.Errorf("endpoint = %q, want auth.test", got)
	}
}

// The account is reported alongside a refusal, not instead of it: "it was
// refused, and it belongs to this account" is usually the whole explanation.
func TestVerifyReportsTheAccountEvenWhenItRefuses(t *testing.T) {
	s := slack(t, writeScopes, okBody)

	acct, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, readToken, readRole)
	if err == nil {
		t.Fatal("a token carrying chat:write was accepted for the read role")
	}
	if acct.user != "stepan" {
		t.Fatalf("account = %+v, want the account Slack named", acct)
	}
}

// A transport error can quote the request, and the request carries the token in
// a header. What is reported is that Slack could not be reached, and nothing
// else.
func TestVerifyDoesNotLeakTheTokenIntoATransportError(t *testing.T) {
	s := slacktest.New(t)
	s.Drop("auth.test")

	_, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, readToken, readRole)
	if err == nil {
		t.Fatal("a dropped connection was not an error")
	}
	if strings.Contains(err.Error(), readToken) {
		t.Fatalf("the error carries the token: %v", err)
	}
}

func TestVerifyRejectsAMalformedReply(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("auth.test", `<html>not json</html>`)

	if _, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, readToken, readRole); err == nil {
		t.Fatal("a reply that is not JSON was accepted")
	}
}

// bot_id present, whatever its value — a null answers the question too, so an
// app that reports itself oddly is still an app.
func TestBotIDIsDetectedByPresence(t *testing.T) {
	for _, body := range []string{
		`{"ok":true,"user":"app","team":"T","bot_id":"B1"}`,
		`{"ok":true,"user":"app","team":"T","bot_id":""}`,
		`{"ok":true,"user":"app","team":"T","bot_id":null}`,
	} {
		s := slack(t, readScopes, body)
		_, err := verify(context.Background(), slackhttp.New(slackhttp.DefaultTimeout), s.URL, readToken, readRole)
		if err == nil {
			t.Errorf("accepted a reply carrying bot_id: %s", body)
			continue
		}
		if !strings.Contains(err.Error(), "BOT token") {
			t.Errorf("error for %s = %q", body, err)
		}
	}
}
