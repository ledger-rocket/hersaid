package slackread

import (
	"fmt"
	"testing"
)

// PRO-7722 replaced fmt.Errorf("%s: %s", method, apiErr) with apiError; the
// text must not change.
func TestAPIErrorKeepsTheOldText(t *testing.T) {
	t.Parallel()
	got := apiError{method: "conversations.history", code: "channel_not_found"}.Error()
	if want := fmt.Sprintf("%s: %s", "conversations.history", "channel_not_found"); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
