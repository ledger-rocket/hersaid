package slackread

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slacktest"
)

// limited makes method answer as Slack does when it rate-limits a caller: HTTP
// 429, a Retry-After header, and a JSON body that says so.
func limited(s *slacktest.Server, method, retryAfter string) {
	s.ReplyFunc(method, func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
	})
}

// A 429 is told apart from every other refusal, with the wait Slack asked for,
// so a caller that retries can wait exactly that long and one that does not can
// say what happened.
func TestA429IsARateLimitedErrorCarryingRetryAfter(t *testing.T) {
	s := slacktest.New(t)
	limited(s, "conversations.history", "7")

	_, err := client(t, s).History(context.Background(), "C0000000001", HistoryOptions{Limit: 5})
	var rl RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want a RateLimitedError", err, err)
	}
	if rl.Method != "conversations.history" || rl.RetryAfter != 7*time.Second {
		t.Fatalf("got %+v, want method conversations.history, retry after 7s", rl)
	}
	if got, want := err.Error(), "conversations.history: ratelimited (retry after 7s)"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// Every endpoint goes through the same request path, so search is limited the
// same way.
func TestA429OnSearchIsARateLimitedError(t *testing.T) {
	s := slacktest.New(t)
	limited(s, "search.messages", "3")

	_, err := client(t, s).SearchPage(context.Background(), SearchOptions{Query: "x", Count: 10})
	var rl RateLimitedError
	if !errors.As(err, &rl) || rl.Method != "search.messages" || rl.RetryAfter != 3*time.Second {
		t.Fatalf("err = %v, want a RateLimitedError for search.messages after 3s", err)
	}
}

// A header that is missing or is not a whole number of seconds gives no wait
// to honour. It reads as zero, which a caller treats as "do not retry", rather
// than as a guess.
func TestRetryAfterThatCannotBeReadIsZero(t *testing.T) {
	for _, v := range []string{"", "soon", "-5", "1.5", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		t.Run(v, func(t *testing.T) {
			s := slacktest.New(t)
			limited(s, "conversations.replies", v)

			_, err := client(t, s).Replies(context.Background(), "C0000000001", "1.1", HistoryOptions{})
			var rl RateLimitedError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want a RateLimitedError", err)
			}
			if rl.RetryAfter != 0 {
				t.Fatalf("RetryAfter = %v, want 0", rl.RetryAfter)
			}
			if got, want := err.Error(), "conversations.replies: ratelimited"; got != want {
				t.Fatalf("Error() = %q, want %q", got, want)
			}
		})
	}
}

// A huge value is clamped rather than overflowing into a negative duration,
// including one too large for any integer type.
func TestRetryAfterIsClampedToADay(t *testing.T) {
	for _, v := range []string{"99999999999999999", "999999999999999999999999999999"} {
		t.Run(v, func(t *testing.T) {
			s := slacktest.New(t)
			limited(s, "conversations.history", v)

			_, err := client(t, s).History(context.Background(), "C0000000001", HistoryOptions{})
			var rl RateLimitedError
			if !errors.As(err, &rl) || rl.RetryAfter != 24*time.Hour {
				t.Fatalf("err = %v, want a RateLimitedError clamped to 24h", err)
			}
		})
	}
}

// A refusal that is not a 429 keeps the error it always had: other verbs
// print it, and their output must not change.
func TestOtherRefusalsAreNotRateLimited(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history", `{"ok":false,"error":"channel_not_found"}`)

	_, err := client(t, s).History(context.Background(), "C0000000001", HistoryOptions{})
	if _, ok := errors.AsType[RateLimitedError](err); ok {
		t.Fatalf("err = %v is a RateLimitedError; only HTTP 429 is", err)
	}
	if got, want := err.Error(), "conversations.history: channel_not_found"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// A ratelimited code in a 200 body is Slack's ordinary refusal, not a 429. It
// carries no Retry-After, so it stays the plain error it was.
func TestRatelimitedCodeWithoutA429IsAPlainRefusal(t *testing.T) {
	s := slacktest.New(t)
	s.Reply("conversations.history", `{"ok":false,"error":"ratelimited"}`)

	_, err := client(t, s).History(context.Background(), "C0000000001", HistoryOptions{})
	if _, ok := errors.AsType[RateLimitedError](err); ok {
		t.Fatalf("err = %v is a RateLimitedError; only HTTP 429 is", err)
	}
	if got, want := err.Error(), "conversations.history: ratelimited"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// A 429 whose body is not JSON is still a rate limit: the status decides, not
// the body.
func TestA429WithAnUnreadableBodyIsStillRateLimited(t *testing.T) {
	s := slacktest.New(t)
	s.ReplyFunc("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	})

	_, err := client(t, s).History(context.Background(), "C0000000001", HistoryOptions{})
	var rl RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second {
		t.Fatalf("err = %v, want a RateLimitedError after 2s", err)
	}
}
