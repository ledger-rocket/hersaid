// Package slackwrite holds the Slack endpoints that act as the user.
//
// Requests are built from endpoint-specific typed arguments, never from a
// caller-supplied url.Values, so no argument can introduce a field the endpoint
// method did not declare — `blocks=`, which Slack renders in place of `text`,
// is smuggled in exactly that way when a field reaches the wire unencoded.
//
// Every write is attempted at most once. Errors are typed so that a caller can
// tell "Slack refused this" from "the request left and the answer did not come
// back": see Classify.
package slackwrite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
)

// SlackAPI is the production base URL. It is a parameter of New rather than a
// default so that no code path can retarget a request carrying the write token
// without saying so at the call site.
const SlackAPI = "https://slack.com/api"

// The endpoints this package calls. They are named here rather than only at the
// call site because Classify has to reason about which endpoint produced a
// reply: an error code means different things depending on what was asked.
const (
	methodPostMessage    = "chat.postMessage"
	methodPermalink      = "chat.getPermalink"
	methodAddReaction    = "reactions.add"
	methodRemoveReaction = "reactions.remove"
	methodUpdate         = "chat.update"
	methodDelete         = "chat.delete"
	methodGetUploadURL   = "files.getUploadURLExternal"
	methodCompleteUpload = "files.completeUploadExternal"
)

const maxBody = 4 << 20

// Client calls Slack endpoints that act as the authenticated user.
type Client struct {
	hc      *http.Client
	baseURL string
	cred    cred.Write
}

// New returns a client posting to baseURL with hc, authenticating as c. It
// accepts only cred.Write, so a read credential cannot reach a write endpoint.
func New(c cred.Write, hc *http.Client, baseURL string) *Client {
	return &Client{hc: hc, baseURL: baseURL, cred: c}
}

// PreflightError is a failure before anything left this process. Nothing was
// applied.
type PreflightError struct {
	Method string
	Err    error
}

func (e *PreflightError) Error() string { return e.Method + ": " + e.Err.Error() }
func (e *PreflightError) Unwrap() error { return e.Err }

// TransportError is a request that left this process without a usable reply.
// Whether Slack applied it is not knowable from here.
type TransportError struct {
	Method string
	Err    error
}

func (e *TransportError) Error() string {
	return "network error calling " + e.Method + ": " + e.Err.Error()
}
func (e *TransportError) Unwrap() error { return e.Err }

// APIError is a reply Slack rejected, by its own error code.
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string { return e.Method + ": " + e.Code }

// scopeNames maps each endpoint to the user token scope it needs, so a
// missing_scope refusal can name the scope actually missing rather than
// re-granting one the token already holds. It is fixed at compile time: a
// value out of a response must not be interpolated into instructions telling
// somebody what to grant.
var scopeNames = map[string]string{
	methodPostMessage:    "chat:write",
	methodUpdate:         "chat:write",
	methodDelete:         "chat:write",
	methodAddReaction:    "reactions:write",
	methodRemoveReaction: "reactions:write",
	methodGetUploadURL:   "files:write",
	methodCompleteUpload: "files:write",
}

// Scope returns the user token scope the endpoint that produced err needs, or
// "" when err is nil or is not an *APIError.
func Scope(err error) string {
	var api *APIError
	if !errors.As(err, &api) {
		return ""
	}
	return scopeNames[api.Method]
}

// ErrNoCredential is a client built with the zero credential. cred.Write{} is
// constructible in any package and ResolveWrite returns one alongside its
// error, so the refusal lives here rather than in each caller's control flow:
// the guarantee is that no HTTP request is issued at all, and Slack rejecting
// an empty bearer token would not be that.
var ErrNoCredential = errors.New("no write credential")

// Codes a caller acts on by name. The first two are idempotent successes: the
// end state is the one that was asked for.
const (
	// CodeAlreadyReacted means the reaction is already on the message.
	CodeAlreadyReacted = "already_reacted"
	// CodeNoReaction means the reaction was not on the message to begin with.
	CodeNoReaction = "no_reaction"
	// CodeMissingScope means the write app lacks the scope this endpoint needs.
	CodeMissingScope = "missing_scope"
	// CodeMalformed is a reply that did not parse. It is ambiguous: Slack may
	// have acted before the reply was mangled.
	CodeMalformed = "malformed response"
)

// ambiguous codes mean the request reached Slack but its effect is unknown.
var ambiguous = map[string]bool{
	"fatal_error":    true,
	"internal_error": true,
	CodeMalformed:    true,
}

// idempotent reports a refusal whose reason is that the end state asked for is
// already in place.
//
// It is scoped to the endpoint that can ask for that state. "already_reacted"
// from chat.postMessage is not an idempotent success — it is an answer nobody
// should have received, and treating it as one would report a message as sent
// that Slack refused.
func idempotent(method, code string) bool {
	switch method {
	case methodAddReaction:
		return code == CodeAlreadyReacted
	case methodRemoveReaction:
		return code == CodeNoReaction
	}
	return false
}

// Code returns the Slack error code in err, or "".
func Code(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

// Outcome is how a write attempt must be recorded.
type Outcome int

const (
	// Delivered: Slack confirmed it.
	Delivered Outcome = iota
	// Rejected: Slack refused it, so nothing was applied.
	Rejected
	// Ambiguous: it may or may not have been applied. It is recorded as
	// unknown and never retried automatically — a retry after a transport
	// failure duplicates a message that was in fact delivered.
	Ambiguous
	// AlreadyDone: Slack refused it because the end state asked for is already
	// the current one, as already_reacted and no_reaction are. Nothing was
	// applied and nothing needs to be, so it is recorded as a success. It is an
	// Outcome of its own rather than each caller's special case, so that a
	// caller switching on the classification alone cannot report an idempotent
	// success as a failure: the desired state holds, so the record says ok.
	AlreadyDone
)

// Classify maps an attempt's error onto an Outcome. The classification is
// complete: acting on the Outcome alone is enough, and no caller has to inspect
// Code first to avoid getting an idempotent success wrong.
//
// An unrecognised error is Ambiguous, not Rejected. That is the safe direction:
// recording a possible delivery as a definite failure is what invites the
// duplicate, whereas an unnecessary "unknown" only asks a human to look.
func Classify(err error) Outcome {
	if err == nil {
		return Delivered
	}
	var pre *PreflightError
	if errors.As(err, &pre) {
		return Rejected
	}
	var api *APIError
	if errors.As(err, &api) {
		switch {
		case idempotent(api.Method, api.Code):
			return AlreadyDone
		case ambiguous[api.Code]:
			return Ambiguous
		}
		return Rejected
	}
	return Ambiguous
}

// PostMessageResponse is chat.postMessage's reply, narrowed to the fields used.
type PostMessageResponse struct {
	// TS is the timestamp Slack assigned to the message.
	TS string
	// Text is what Slack actually stored, which can differ from what was
	// submitted; it sanitises and truncates.
	Text string
}

// PostMessage delivers a message. It is attempted at most once.
func (c *Client) PostMessage(ctx context.Context, channel, text, threadTS string) (*PostMessageResponse, error) {
	const method = methodPostMessage
	form := url.Values{}
	form.Set("channel", channel)
	form.Set("text", text)
	if threadTS != "" {
		form.Set("thread_ts", threadTS)
	}

	var out struct {
		envelope

		TS      string `json:"ts"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	err := c.post(ctx, method, form, &out)
	if err != nil {
		return nil, err
	}
	return &PostMessageResponse{TS: out.TS, Text: out.Message.Text}, nil
}

// Update rewrites an existing message in place. ts is the timestamp of the
// message to edit. It is attempted at most once.
func (c *Client) Update(ctx context.Context, channelID, ts, text string) (*PostMessageResponse, error) {
	const method = methodUpdate
	form := url.Values{}
	form.Set("channel", channelID)
	form.Set("ts", ts)
	form.Set("text", text)

	var out struct {
		envelope

		TS      string `json:"ts"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	err := c.post(ctx, method, form, &out)
	if err != nil {
		return nil, err
	}
	return &PostMessageResponse{TS: out.TS, Text: out.Message.Text}, nil
}

// Delete removes a message. ts is the timestamp of the message to delete. It is
// attempted at most once. chat.delete returns no message body, so it echoes
// nothing.
func (c *Client) Delete(ctx context.Context, channelID, ts string) error {
	const method = methodDelete
	form := url.Values{}
	form.Set("channel", channelID)
	form.Set("ts", ts)

	var out envelope
	return c.post(ctx, method, form, &out)
}

// Permalink returns a link to a message. It runs on the write credential
// because it is ancillary to a send that has just happened, and keeping it here
// leaves the read binary's import graph unchanged.
//
// A failure is never fatal to the send it follows — the message is already
// delivered — so callers discard the error and log an empty permalink.
func (c *Client) Permalink(ctx context.Context, channel, messageTS string) (string, error) {
	const method = methodPermalink
	form := url.Values{}
	form.Set("channel", channel)
	form.Set("message_ts", messageTS)

	var out struct {
		envelope

		Permalink string `json:"permalink"`
	}
	err := c.post(ctx, method, form, &out)
	if err != nil {
		return "", err
	}
	return out.Permalink, nil
}

// AddReaction puts an emoji on a message. emoji is the bare name, without
// colons. It is attempted at most once.
func (c *Client) AddReaction(ctx context.Context, channel, timestamp, emoji string) error {
	return c.reaction(ctx, methodAddReaction, channel, timestamp, emoji)
}

// RemoveReaction takes an emoji off a message. It is attempted at most once.
func (c *Client) RemoveReaction(ctx context.Context, channel, timestamp, emoji string) error {
	return c.reaction(ctx, methodRemoveReaction, channel, timestamp, emoji)
}

// UploadTicket is files.getUploadURLExternal's reply: the token for the next
// leg.
type UploadTicket struct {
	// FileID names the staged upload for the completion leg.
	FileID string
	// UploadURL is where the bytes go, with its credential in the query string.
	UploadURL string
}

// GetUploadURL declares a file to be uploaded, returning the upload ticket. It
// is leg one of a three-leg upload: nothing is visible to anyone until the
// completion leg runs. It is attempted at most once.
//
// A reply that passes the envelope but carries an empty file_id or an empty
// upload_url is refused here, while the phase is still "nothing has been
// staged", rather than two legs later where the failure would be about a URL
// that never was.
func (c *Client) GetUploadURL(ctx context.Context, filename string, length int64) (*UploadTicket, error) {
	const method = methodGetUploadURL
	form := url.Values{}
	form.Set("filename", filename)
	form.Set("length", strconv.FormatInt(length, 10))

	var out struct {
		envelope

		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
	}
	err := c.post(ctx, method, form, &out)
	if err != nil {
		return nil, err
	}
	if out.FileID == "" || out.UploadURL == "" {
		return nil, &APIError{Method: method, Code: CodeMalformed}
	}
	return &UploadTicket{FileID: out.FileID, UploadURL: out.UploadURL}, nil
}

// CompleteRequest is files.completeUploadExternal's argument set.
type CompleteRequest struct {
	// Files are the staged uploads, shared in the order given.
	Files []CompleteFile
	// ChannelID is the conversation the files appear in.
	ChannelID string
	// ThreadTS, when set, threads the message.
	ThreadTS string
	// Comment is the message carried alongside the files.
	Comment string
}

// CompleteFile names one staged upload for completion.
type CompleteFile struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Completion is files.completeUploadExternal's reply.
type Completion struct {
	Files []SharedFile
}

// SharedFile is one file's view of the completion.
type SharedFile struct {
	ID     string `json:"id"`
	Shares Shares `json:"shares"`
}

// Shares reports where a file landed. Slack files a share under public or
// private according to the channel, so both maps must be read.
type Shares struct {
	Public  map[string][]Share `json:"public"`
	Private map[string][]Share `json:"private"`
}

// Share is one message that a file appears in.
type Share struct {
	TS string `json:"ts"`
}

// TimestampsFor returns every timestamp for channel from both maps, skipping
// empty ones. Returning only the first would disable the caller's consensus
// check, which exists to notice a reply describing more than one message.
func (s Shares) TimestampsFor(channel string) []string {
	var ts []string
	for _, shares := range []map[string][]Share{s.Public, s.Private} {
		for _, sh := range shares[channel] {
			if sh.TS != "" {
				ts = append(ts, sh.TS)
			}
		}
	}
	return ts
}

// CompleteUpload shares the staged files, making them visible, and returns the
// reply. It is leg three of a three-leg upload; it is the only leg that makes
// anything visible. It is attempted at most once.
func (c *Client) CompleteUpload(ctx context.Context, req CompleteRequest) (*Completion, error) {
	const method = methodCompleteUpload
	form := url.Values{}
	form.Set("channel_id", req.ChannelID)
	if req.ThreadTS != "" {
		form.Set("thread_ts", req.ThreadTS)
	}
	if req.Comment != "" {
		form.Set("initial_comment", req.Comment)
	}
	files, err := json.Marshal(req.Files)
	if err != nil {
		return nil, &PreflightError{Method: method, Err: err}
	}
	form.Set("files", string(files))

	var out struct {
		envelope

		Files []SharedFile `json:"files"`
	}
	err = c.post(ctx, method, form, &out)
	if err != nil {
		return nil, err
	}
	return &Completion{Files: out.Files}, nil
}

func (c *Client) reaction(ctx context.Context, method, channel, timestamp, emoji string) error {
	form := url.Values{}
	form.Set("channel", channel)
	form.Set("timestamp", timestamp)
	form.Set("name", emoji)

	var out envelope
	return c.post(ctx, method, form, &out)
}

// envelope is the part of every Slack reply that decides success.
type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// enveloped is what post needs of its destination.
type enveloped interface{ status() (bool, string) }

func (e envelope) status() (bool, string) { return e.OK, e.Error }

func (c *Client) post(ctx context.Context, method string, form url.Values, out enveloped) error {
	// Structural: a zero credential cannot reach the wire, whatever the caller
	// did with ResolveWrite's error. Nothing has left the process, so it is a
	// PreflightError like any other refusal before the request was built.
	if c.cred.IsZero() {
		return &PreflightError{Method: method, Err: ErrNoCredential}
	}

	// The deadline is set before the request is built, so a caller passing
	// context.Background() still cannot hang.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(c.hc))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return &PreflightError{Method: method, Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.cred.Authorize(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return &TransportError{Method: method, Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		// The request left; a truncated reply says nothing about what Slack did.
		return &TransportError{Method: method, Err: err}
	}
	err = json.Unmarshal(body, out)
	if err != nil {
		return &APIError{Method: method, Code: CodeMalformed}
	}
	if ok, code := out.status(); !ok {
		if code == "" {
			code = "unknown error"
		}
		return &APIError{Method: method, Code: code}
	}
	return nil
}

func requestTimeout(hc *http.Client) time.Duration {
	if hc.Timeout > 0 {
		return hc.Timeout
	}
	return slackhttp.DefaultTimeout
}
