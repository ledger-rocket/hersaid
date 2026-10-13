package main

import (
	"fmt"
	"testing"
)

// Sentinels and botTokenError stand in for errors.New(const) and
// fmt.Errorf(botRefusal, user); the refusal texts must not change.
func TestRefusalsKeepTheOldText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ got, want string }{
		{errUnknownScopes.Error(), unknownScopesRefusal},
		{errReadTokenCanPost.Error(), readTokenCanPostRefusal},
		{errWriteTokenCannotPost.Error(), writeTokenCannotPostRefusal},
		{botTokenError{user: "deploybot"}.Error(), fmt.Sprintf(botRefusal, "deploybot")},
	} {
		if c.got != c.want {
			t.Errorf("Error() = %q, want %q", c.got, c.want)
		}
	}
}
