package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

var (
	errCouldNotReachSlack = errors.New("  could not reach Slack")
	errCouldNotReadSlack  = errors.New("  could not read Slack's reply")
	errSlackSentReplyThis = errors.New("  Slack sent a reply this cannot read (HTTP")
	errSlackRejectedIt    = errors.New("  Slack rejected it")
)

// writeScope is the scope the whole role split turns on: the write app carries
// it, and the read app must not.
const writeScope = "chat:write"

// scopeHeader is where Slack reports what a token was actually granted.
const scopeHeader = "X-Oauth-Scopes"

// maxBody caps the auth.test response read. Slack's is a few hundred bytes; the
// bound is here because a broken or hostile server should not be able to
// exhaust memory during enrolment.
const maxBody = 1 << 20

// account is whom a token turned out to belong to. It is reported even when the
// token is then refused: knowing which account answered is usually the whole
// explanation for the refusal.
type account struct {
	user   string
	team   string
	userID string
}

// authTest is Slack's reply, narrowed to what enrolment decides on.
type authTest struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	User   string `json:"user"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
	// BotID is a RawMessage so that presence can be told from emptiness: a bot
	// token is identified by the field being there at all.
	BotID json.RawMessage `json:"bot_id"`
}

// verify confirms a token works and that what it was granted matches the role
// it is about to be stored for.
//
// It returns the account whenever Slack named one, alongside any refusal, so
// the caller can say who the token belongs to before saying why it was not
// stored.
func verify(ctx context.Context, hc *http.Client, baseURL, token string, r role) (account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/auth.test", nil)
	if err != nil {
		return account{}, fmt.Errorf("  could not build the request: %w", err)
	}
	// Set here rather than through cred: enrolment holds a string that has not
	// been established to be a credential of either role, which is the question
	// this call exists to answer.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := hc.Do(req)
	if err != nil {
		// The error is not printed: a transport error can quote the request,
		// and the request carries the token in a header.
		return account{}, errCouldNotReachSlack
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return account{}, errCouldNotReadSlack
	}
	var out authTest
	err = json.Unmarshal(body, &out)
	if err != nil {
		return account{}, fmt.Errorf("%w %d)", errSlackSentReplyThis, resp.StatusCode)
	}
	if !out.OK {
		e := out.Error
		if e == "" {
			e = "unknown"
		}
		return account{}, fmt.Errorf("%w: %s", errSlackRejectedIt, e)
	}
	acct := account{user: out.User, team: out.Team, userID: out.UserID}

	// A bot token carries chat:write and would pass the scope check below, but
	// posts as an app rather than as the person who installed it. Refused
	// rather than warned about, because the difference is invisible from here
	// — it only shows up when somebody sees the message in Slack.
	if out.BotID != nil {
		return acct, botTokenError{user: out.User}
	}

	err = checkScopes(resp.Header, r)
	if err != nil {
		return acct, err
	}
	return acct, nil
}

// checkScopes is what mechanically enforces the two-app split: a read token
// that can post collapses the separation the rest of the tool is built on, and
// the granted scopes are the only evidence of which app a token came from.
//
// It fails closed. Storing a token whose scopes could not be read is how a
// token carrying chat:write gets enrolled as a read credential — the one
// outcome the check exists to prevent. An unanswerable question is a refusal.
func checkScopes(h http.Header, r role) error {
	scopes := parseScopes(h)
	if len(scopes) == 0 {
		return errUnknownScopes
	}
	switch granted := contains(scopes, writeScope); {
	case granted && !r.needsWriteScope:
		return errReadTokenCanPost
	case !granted && r.needsWriteScope:
		return errWriteTokenCannotPost
	}
	return nil
}

// parseScopes reads the granted scopes as a set of exact names. Membership is
// by whole scope, never by substring: "chat:write.public" is a different grant
// from "chat:write" and matching one against the other would refuse or admit
// the wrong token.
func parseScopes(h http.Header) []string {
	var out []string
	for _, v := range h.Values(scopeHeader) {
		for s := range strings.SplitSeq(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// The three scope refusals, as sentinels so a caller can tell them apart.
var (
	errUnknownScopes        = errors.New(unknownScopesRefusal)
	errReadTokenCanPost     = errors.New(readTokenCanPostRefusal)
	errWriteTokenCannotPost = errors.New(writeTokenCannotPostRefusal)
)

// botTokenError refuses a bot token; its text is botRefusal with the app's name.
type botTokenError struct{ user string }

func (e botTokenError) Error() string { return fmt.Sprintf(botRefusal, e.user) }

const botRefusal = `  REFUSED: that is a BOT token. Messages would post as an app named
  %q, not as you.
  Copy the *User* OAuth Token (xoxp-...) from OAuth & Permissions — not the
  Bot User OAuth Token (xoxb-...). If the page only offers a bot token, the
  app has Bot Token Scopes set; move chat:write to User Token Scopes and
  reinstall the app.`

const readTokenCanPostRefusal = `  REFUSED: this token carries chat:write, so it is not a read-only token.
  A read token that can post collapses the separation this tool relies on.
  Create a SECOND Slack app with only the read scopes and use its token here.`

const writeTokenCannotPostRefusal = `  REFUSED: this token does not carry chat:write, so it cannot post.
  The write app needs chat:write under User Token Scopes.`

const unknownScopesRefusal = `  REFUSED: Slack's reply carried no x-oauth-scopes header, so what this token
  was granted is unknown and the read/write split cannot be checked. Not
  knowing which scopes a token carries is a refusal, not a warning: the check
  is the only thing standing between a read credential and one that can post.
  If something in the path is rewriting response headers, take it out and
  try again.`
