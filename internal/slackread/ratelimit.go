package slackread

import (
	"errors"
	"strconv"
	"time"
)

// maxRetryAfterSeconds is the ceiling a Retry-After header may ask for. A day
// is far past any real rate-limit wait, and clamping before the seconds-to-
// duration conversion is what keeps a huge value from overflowing into a
// negative duration.
const maxRetryAfterSeconds = 86400

// RateLimitedError is Slack's HTTP 429 answer: the method that was refused and
// the wait it asked for. RetryAfter is zero when the reply named no wait.
type RateLimitedError struct {
	Method     string
	RetryAfter time.Duration
}

// Error names the method and the wait, so a caller that does not retry can say
// what happened.
func (e RateLimitedError) Error() string {
	if e.RetryAfter > 0 {
		return e.Method + ": ratelimited (retry after " + e.RetryAfter.String() + ")"
	}
	return e.Method + ": ratelimited"
}

// retryAfter reads a Retry-After header as a whole number of seconds. Anything
// else is zero, and a value past a day is clamped rather than allowed to
// overflow into a negative duration.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		n = maxRetryAfterSeconds
	case err != nil:
		return 0
	case n > maxRetryAfterSeconds:
		n = maxRetryAfterSeconds
	}
	return time.Duration(n) * time.Second
}
