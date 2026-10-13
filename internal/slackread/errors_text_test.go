package slackread

import (
	"fmt"
	"testing"
)

// The API error type stands in for fmt.Errorf("%s: %s", method, apiErr); the
// text must not change.
func TestAPIErrorKeepsTheOldText(t *testing.T) {
	t.Parallel()
	got := APIError{Method: "conversations.history", Code: "channel_not_found"}.Error()
	if want := fmt.Sprintf("%s: %s", "conversations.history", "channel_not_found"); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
