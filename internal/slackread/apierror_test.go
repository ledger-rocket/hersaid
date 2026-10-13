package slackread

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// A caller decides what a refusal means by its code, never by matching the
// text, so the code is reachable through errors.As.
func TestAPIErrorCarriesMethodAndCode(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.replies", `{"ok":false,"error":"thread_not_found"}`)

	_, err := client(t, s).Replies(context.Background(), "C0000000001", "1.1", HistoryOptions{})
	var ae APIError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &ae) {
		t.Fatalf("err = %v (%T), want an APIError", err, err)
	}
	if ae.Method != "conversations.replies" || ae.Code != "thread_not_found" {
		t.Fatalf("got %+v", ae)
	}
	if got, want := err.Error(), "conversations.replies: thread_not_found"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// errTextOnly reads like an auth refusal and is not one: the predicate decides
// by code, never by text.
var errTextOnly = errors.New("search.messages: invalid_auth")

// The codes that say the credential itself is no good are told apart from
// every other refusal, wherever they are wrapped.
func TestIsAuthError(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive"} {
		err := fmt.Errorf("context: %w", APIError{Method: "search.messages", Code: code})
		if !IsAuthError(err) {
			t.Errorf("IsAuthError(%s) = false", code)
		}
	}
	for _, err := range []error{
		nil,
		APIError{Method: "search.messages", Code: "missing_scope"},
		APIError{Method: "search.messages", Code: "invalid_auth_extra"},
		RateLimitedError{Method: "search.messages", RetryAfter: time.Second},
		errTextOnly,
	} {
		if IsAuthError(err) {
			t.Errorf("IsAuthError(%v) = true", err)
		}
	}
}
