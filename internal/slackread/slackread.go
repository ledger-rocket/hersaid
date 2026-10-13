// Package slackread holds read-only Slack endpoints.
//
// It is a separate package from slackwrite so that cmd/hear's import graph
// contains no write code at all — a method set could not achieve that, since
// any package could construct a write client from a package it already imports.
//
// Request construction is private. Callers name an endpoint and supply typed
// arguments; no caller-supplied url.Values ever reaches the wire.
package slackread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
)

var (
	errConversationsListStillPaginating = errors.New("conversations.list: still paginating")
	errUsersListStillPaginating         = errors.New("users.list: still paginating")
	errNoMessageTs                      = errors.New("no message at ts")
	errNoFile                           = errors.New("no file")
	errMalformedResponseHTTP            = errors.New("malformed response (HTTP")
)

// SlackAPI is the production base URL. It is a parameter of New rather than a
// default so that no code path can retarget a request carrying the token
// without saying so at the call site.
const SlackAPI = "https://slack.com/api"

// pageSize is Slack's recommendation; it rejects limit >= 1000.
const pageSize = 200

// conversationTypes decides what the cache can address: without im and mpim,
// no DM would resolve.
const conversationTypes = "public_channel,private_channel,im,mpim"

// maxBody caps a response read; Slack's read endpoints are far below this and a
// hostile or broken server should not be able to exhaust memory.
const maxBody = 32 << 20

// drainLimit is how much of a refused response is read only to free its
// connection for reuse.
const drainLimit = 4 << 10

// maxPages bounds cursor following. At pageSize that is 200,000 entries, far
// past any real workspace; the bound exists because a server that returns the
// same non-empty cursor forever — broken, hostile, or something in the middle —
// would otherwise spin here until the process is killed.
const maxPages = 1000

// ErrNoCredential is a client built with the zero credential. cred.Read{} is
// constructible in any package and ResolveRead returns one alongside its error,
// so the refusal lives here rather than in each caller's control flow.
var ErrNoCredential = errors.New("no read credential")

// Client calls read-only Slack endpoints.
type Client struct {
	hc      *http.Client
	baseURL string
	cred    cred.Read
}

// New returns a client reading from baseURL with hc, authenticating as c. It
// accepts only cred.Read, the mirror of slackwrite.New accepting only
// cred.Write: the role types are what stop a credential reaching the wrong
// half of the API by accident.
func New(c cred.Read, hc *http.Client, baseURL string) *Client {
	return &Client{hc: hc, baseURL: baseURL, cred: c}
}

// Reaction is one entry of a message's reactions array. Count is Slack's own
// count, never len(Users): Slack truncates the users array on heavily-reacted
// messages, so the two genuinely differ and Count is the one that is right.
type Reaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users"`
}

// Message is one entry of a transcript, narrowed to the fields the commands
// render.
type Message struct {
	TS         string     `json:"ts"`
	User       string     `json:"user"`
	Username   string     `json:"username"`
	BotID      string     `json:"bot_id"`
	Text       string     `json:"text"`
	ReplyCount int        `json:"reply_count"`
	Reactions  []Reaction `json:"reactions"`
	// Blocks and Attachments are kept as raw JSON because only their presence is
	// used: a message built from blocks carries text that is a fallback or
	// empty, so a consumer that treats Text as the whole message would be
	// reading something a person never saw.
	Blocks      json.RawMessage `json:"blocks"`
	Attachments json.RawMessage `json:"attachments"`
	// Files is decoded rather than kept raw because the transcript names its
	// entries and `hear files` addresses them.
	Files []File `json:"files"`
}

// File is one entry of a message's files array, narrowed to what naming and
// downloading it need.
type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Mimetype string `json:"mimetype"`
	// Size is what Slack says the file is, which is metadata about the file and
	// not a promise about the response: a download bounds itself on the bytes it
	// actually reads.
	Size int64 `json:"size"`
	// Mode says whether there are bytes to fetch at all. "hosted" and "snippet"
	// are stored by Slack; "external" lives in storage Slack only links to,
	// "tombstone" is a file that has been deleted, and "email" is a mail body.
	Mode               string `json:"mode"`
	IsExternal         bool   `json:"is_external"`
	URLPrivateDownload string `json:"url_private_download"`
}

// HasUnrenderedContent reports whether the message carries content the
// transcript does not show, so Text alone is not a faithful snapshot of it.
//
// Not simply "has blocks": Slack generates a rich_text block for ordinary
// messages, so treating any block as unrenderable would refuse nearly every
// real message. A rich_text block is the text — anything else (a section, an
// image, a bot's layout) leaves Text as a fallback the author may never have
// written. Files count for the same reason: the message is the file, and its
// text is often empty.
//
// Attachments do not count. They are overwhelmingly link unfurls Slack
// generated rather than authored content, and chat.update retains them where
// it drops blocks.
func (m Message) HasUnrenderedContent() bool {
	if len(m.Files) > 0 {
		return true
	}
	return m.HasUnrenderedBlocks()
}

// HasUnrenderedBlocks reports content a transcript cannot show and a caller
// cannot preserve: blocks, and not files.
//
// The distinction is what tells a message that can be acted on from one that
// cannot. A file is listed by name and can be shown; blocks read as fallback
// text or as nothing, so a render of them would show something the author never
// wrote, and a text-only chat.update drops them outright.
//
// Attachments are excluded here for the reason HasUnrenderedContent gives:
// they are overwhelmingly unfurls rather than authored content, and
// chat.update keeps them.
func (m Message) HasUnrenderedBlocks() bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	err := json.Unmarshal(m.Blocks, &blocks)
	if err != nil {
		// Undecodable blocks are unknown content, and unknown is not safe.
		return len(m.Blocks) > 0 && string(m.Blocks) != "null"
	}
	for _, b := range blocks {
		if b.Type != "rich_text" {
			return true
		}
	}
	return false
}

// AuthTestResponse is Slack's auth.test reply, narrowed to the fields used.
type AuthTestResponse struct {
	User   string `json:"user"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
}

// AuthTest identifies the account the credential belongs to.
func (c *Client) AuthTest(ctx context.Context) (*AuthTestResponse, error) {
	var out struct {
		envelope
		AuthTestResponse
	}
	err := c.get(ctx, "auth.test", nil, &out)
	if err != nil {
		return nil, err
	}
	return &out.AuthTestResponse, nil
}

// ConversationsList returns every conversation the token can see, following
// cursor pagination to the end. A single unpaginated call silently truncates a
// workspace larger than one page, which surfaces much later as a channel that
// "does not exist".
func (c *Client) ConversationsList(ctx context.Context) ([]cache.Conversation, error) {
	// Empty, not nil: this list is written to the cache verbatim, where a null
	// in place of [] breaks every `jq '.channels[]'` the README documents.
	all := []cache.Conversation{}
	cursor := ""
	for range maxPages {
		params := url.Values{}
		params.Set("types", conversationTypes)
		params.Set("exclude_archived", "true")
		params.Set("limit", strconv.Itoa(pageSize))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			envelope

			Channels []cache.Conversation `json:"channels"`
		}
		err := c.get(ctx, "conversations.list", params, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Channels...)
		if cursor = out.ResponseMetadata.NextCursor; cursor == "" {
			return all, nil
		}
	}
	return nil, fmt.Errorf("%w after %d pages", errConversationsListStillPaginating, maxPages)
}

// UsersList returns every member, following cursor pagination to the end.
func (c *Client) UsersList(ctx context.Context) ([]cache.User, error) {
	all := []cache.User{}
	cursor := ""
	for range maxPages {
		params := url.Values{}
		params.Set("limit", strconv.Itoa(pageSize))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			envelope

			Members []cache.User `json:"members"`
		}
		err := c.get(ctx, "users.list", params, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Members...)
		if cursor = out.ResponseMetadata.NextCursor; cursor == "" {
			return all, nil
		}
	}
	return nil, fmt.Errorf("%w after %d pages", errUsersListStillPaginating, maxPages)
}

// HistoryOptions bounds a transcript request. conversations.history and
// conversations.replies take the same parameters, so both are bounded by this.
//
// It is a struct rather than four more parameters because Oldest, Latest and
// Cursor are all opaque strings: adjacent arguments of the same type that a
// caller can swap without the compiler noticing, and Slack answers a reversed
// window with an empty result rather than an error.
type HistoryOptions struct {
	// Limit is how many messages to ask for in this page. Zero leaves it to
	// Slack, whose own default is 100. Slack rejects 1000 and above, so a
	// transcript longer than that is reached with Cursor, never with Limit.
	Limit int
	// Oldest and Latest bound the window by message timestamp. Either may be
	// empty, which is unbounded in that direction.
	Oldest string
	Latest string
	// Cursor continues a previous page: it is the NextCursor that page
	// returned. Empty starts at the newest message in the window.
	Cursor string
	// Inclusive keeps the messages at Oldest and Latest in the result. It is
	// what makes a window of one timestamp mean that message rather than
	// nothing.
	Inclusive bool
}

// HistoryPage is one page of a transcript.
type HistoryPage struct {
	// Messages are in the order Slack returned them, which is not the order a
	// transcript prints them: newest first for History, oldest first for
	// Replies.
	Messages []Message
	// NextCursor continues the transcript when it is non-empty; pass it back as
	// HistoryOptions.Cursor. Empty means this page reached the end of the
	// window, and is the only way a caller can know a transcript is complete.
	NextCursor string
}

// History returns one page of a conversation's messages, newest first.
//
// Paging is the caller's rather than this package's, which is why a page and
// not a slice comes back. Slack caps a single call at 999 messages, and the two
// verbs built on this want different things past that point: a transcript of
// the last N stops as soon as it has N, while a window (--since) must be walked
// to its end, and only the caller knows which it is doing. Treating one full
// page as the whole transcript is how a transcript silently loses its
// beginning, so NextCursor is returned rather than discarded.
func (c *Client) History(ctx context.Context, channelID string, opts HistoryOptions) (HistoryPage, error) {
	return c.transcript(ctx, "conversations.history", url.Values{}, channelID, opts)
}

// Message returns the single message at ts, or an error naming the timestamp
// when the conversation holds no such message.
//
// Two endpoints are needed because neither answers for every message. A
// conversation's history is its channel-level messages: a reply inside a thread
// is not one of them and never appears there, however the window is drawn.
// conversations.replies does take a reply's own timestamp, but it answers for a
// thread, so it is not the way to ask about an ordinary message. A caller
// holding a ts — from a transcript, or from a permalink someone pasted — cannot
// tell which kind it has, so it is asked as a channel message first and as a
// thread member second.
func (c *Client) Message(ctx context.Context, channelID, ts string) (Message, error) {
	page, err := c.History(ctx, channelID, HistoryOptions{
		Limit: 1, Oldest: ts, Latest: ts, Inclusive: true,
	})
	if err != nil {
		return Message{}, err
	}
	if m, ok := findTS(page.Messages, ts); ok {
		return m, nil
	}

	// Not found as a channel message, so it is a thread reply or nothing. The
	// failure is named for what was being looked for rather than for the missing
	// message it usually means: a rate limit or an expired token takes this
	// branch too, and neither is evidence that the message does not exist.
	page, err = c.Replies(ctx, channelID, ts, HistoryOptions{Limit: pageSize})
	if err != nil {
		return Message{}, fmt.Errorf("looking for ts %s: %w", ts, err)
	}
	if m, ok := findTS(page.Messages, ts); ok {
		return m, nil
	}
	return Message{}, fmt.Errorf("%w %s", errNoMessageTs, ts)
}

func findTS(msgs []Message, ts string) (Message, bool) {
	for _, m := range msgs {
		if m.TS == ts {
			return m, true
		}
	}
	return Message{}, false
}

// Replies returns one page of a thread's messages, oldest first, beginning with
// the parent. It pages exactly as History does.
func (c *Client) Replies(ctx context.Context, channelID, threadTS string, opts HistoryOptions) (HistoryPage, error) {
	params := url.Values{}
	params.Set("ts", threadTS)
	return c.transcript(ctx, "conversations.replies", params, channelID, opts)
}

// SearchMatch is one search.messages hit.
type SearchMatch struct {
	Message

	Permalink string `json:"permalink"`
	Channel   struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		IsIM   bool   `json:"is_im"`
		IsMPIM bool   `json:"is_mpim"`
	} `json:"channel"`
}

// Search runs a workspace message search.
func (c *Client) Search(ctx context.Context, query string, count int) ([]SearchMatch, error) {
	results, err := c.SearchPage(ctx, SearchOptions{Query: query, Count: count})
	if err != nil {
		return nil, err
	}
	return results.Matches, nil
}

// envelope is the part of every Slack reply that decides success.
type envelope struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// APIError is Slack's ok:false answer to Method, with the error code it gave.
// It is exported so a caller can decide what a refusal means by its code
// rather than by matching the text.
type APIError struct{ Method, Code string }

func (e APIError) Error() string { return e.Method + ": " + e.Code }

// authCodes are the refusals that say the credential itself is no good, so
// nothing that follows with it can succeed either.
var authCodes = map[string]bool{
	"invalid_auth":     true,
	"not_authed":       true,
	"token_revoked":    true,
	"token_expired":    true,
	"account_inactive": true,
}

// IsAuthError reports whether err, however wrapped, is Slack refusing the
// credential rather than the request.
func IsAuthError(err error) bool {
	var ae APIError
	return errors.As(err, &ae) && authCodes[ae.Code]
}

// enveloped is what get needs of its destination. Every response struct embeds
// envelope, so each satisfies it without saying so.
type enveloped interface{ status() (bool, string) }

func (e envelope) status() (bool, string) { return e.OK, e.Error }

// requestTimeout prefers the client's own bound and falls back to the package
// default, so a client built elsewhere cannot leave a request unbounded.
func requestTimeout(hc *http.Client) time.Duration {
	if hc.Timeout > 0 {
		return hc.Timeout
	}
	return slackhttp.DefaultTimeout
}

// FileInfo returns the metadata for one file, addressed by its own id rather
// than through a message.
//
// A canvas is why this exists. Slack gives one a permalink of its own —
// /docs/<team>/<file id> — and nothing about it names a message, so there is no
// timestamp to ask a transcript for. A file's id is enough for files.info, and
// the reply carries the download URL the bytes come from.
func (c *Client) FileInfo(ctx context.Context, fileID string) (File, error) {
	params := url.Values{}
	params.Set("file", fileID)

	var out struct {
		envelope

		File File `json:"file"`
	}
	err := c.get(ctx, "files.info", params, &out)
	if err != nil {
		return File{}, fmt.Errorf("looking up file %s: %w", fileID, err)
	}
	if out.File.ID == "" {
		return File{}, fmt.Errorf("%w %s", errNoFile, fileID)
	}
	return out.File, nil
}

// fileIDShape is what a Slack file id looks like.
var fileIDShape = regexp.MustCompile(`^F[A-Z0-9]{6,}$`)

// Canvases are the canvas documents attached to a conversation.
//
// Slack surfaces one in more than one place and the shapes do not agree with
// the documentation, so all of them are read: properties.canvas is the one
// conversations.info documents, properties.tabs carries a canvas a channel
// shows as a tab, and properties.meeting_notes is what a notes canvas is filed
// under. A real channel was observed carrying the last two and not the first.
//
// Duplicates are collapsed, because a channel commonly lists the same document
// as both a tab and its meeting notes.
func (c *Client) Canvases(ctx context.Context, channelID string) ([]string, error) {
	params := url.Values{}
	params.Set("channel", channelID)

	var out struct {
		envelope

		Channel struct {
			Properties struct {
				Canvas struct {
					FileID string `json:"file_id"`
				} `json:"canvas"`
				MeetingNotes struct {
					FileID string `json:"file_id"`
				} `json:"meeting_notes"`
				Tabs []struct {
					Type string `json:"type"`
					Data struct {
						FileID string `json:"file_id"`
					} `json:"data"`
				} `json:"tabs"`
			} `json:"properties"`
		} `json:"channel"`
	}
	err := c.get(ctx, "conversations.info", params, &out)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", channelID, err)
	}

	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		// Checked for shape, not merely for emptiness: these ids are reported
		// back to a person when a channel holds several, and a value out of a
		// reply should not be able to put anything else on that line.
		if !fileIDShape.MatchString(id) || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	p := out.Channel.Properties
	add(p.Canvas.FileID)
	for _, t := range p.Tabs {
		// Slack documents both names for the same kind of tab.
		switch t.Type {
		case "canvas", "channel_canvas":
			add(t.Data.FileID)
		}
	}
	add(p.MeetingNotes.FileID)
	return ids, nil
}

// transcript is the request both transcript endpoints make. They differ only in
// the method name and in replies naming its parent, so sharing the construction
// is what keeps the two option sets from drifting apart.
func (c *Client) transcript(ctx context.Context, method string, params url.Values,
	channelID string, opts HistoryOptions,
) (HistoryPage, error) {
	params.Set("channel", channelID)
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Oldest != "" {
		params.Set("oldest", opts.Oldest)
	}
	if opts.Latest != "" {
		params.Set("latest", opts.Latest)
	}
	if opts.Cursor != "" {
		params.Set("cursor", opts.Cursor)
	}
	if opts.Inclusive {
		params.Set("inclusive", "true")
	}
	var out struct {
		envelope

		Messages []Message `json:"messages"`
	}
	err := c.get(ctx, method, params, &out)
	if err != nil {
		return HistoryPage{}, err
	}
	return HistoryPage{Messages: out.Messages, NextCursor: out.ResponseMetadata.NextCursor}, nil
}

func (c *Client) get(ctx context.Context, method string, params url.Values, out enveloped) error {
	// Structural: a zero credential cannot reach the wire, whatever the caller
	// did with ResolveRead's error. No request is issued at all — an
	// `Authorization: Bearer ` for Slack to reject would not be the same thing.
	if c.cred.IsZero() {
		return fmt.Errorf("%s: %w", method, ErrNoCredential)
	}

	u := c.baseURL + "/" + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	// Every request carries a deadline of its own, so a caller passing
	// context.Background() still cannot hang: the client timeout is a backstop,
	// not the only bound.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(c.hc))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	c.cred.Authorize(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("network error calling %s: %w", method, err)
	}
	defer resp.Body.Close()

	// A 429 is told apart from every other refusal before the body is read: the
	// status decides, not the JSON, and a rate limit is the one refusal a caller
	// may want to wait out.
	if resp.StatusCode == http.StatusTooManyRequests {
		// Drained, so the connection can carry the retry a caller may make.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		return RateLimitedError{Method: method, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("network error calling %s: %w", method, err)
	}
	err = json.Unmarshal(body, out)
	if err != nil {
		return fmt.Errorf("%s: %w %d)", method, errMalformedResponseHTTP, resp.StatusCode)
	}
	if ok, apiErr := out.status(); !ok {
		if apiErr == "" {
			apiErr = "unknown error"
		}
		return APIError{Method: method, Code: apiErr}
	}
	return nil
}
