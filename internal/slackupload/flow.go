package slackupload

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/slackwrite"
)

// Staged is one file Slack has taken the bytes of but shown to nobody.
type Staged struct {
	Local

	// ID is what the completion leg names the file by.
	ID string
}

// Stage runs legs 1 and 2 for every file, in order.
//
// done is called as each file finishes its transfer, so a caller watching for a
// signal knows which files exist while the send is still running rather than
// only when this returns. It is called on this goroutine, before the next file
// starts.
//
// The files that did stage are returned alongside any error. They are not a
// partial success — nothing is visible until Share — but they are what an
// interrupted attempt has to be reconciled against, so losing them to an error
// return would be losing the only record of what happened.
//
// Every failure here means the message was not posted. That is true by
// construction rather than by classification: no comment has been submitted
// anywhere, and a staged file is not a message.
func (c *Client) Stage(ctx context.Context, files []Local, done func(Staged)) ([]Staged, error) {
	staged := make([]Staged, 0, len(files))
	for _, f := range files {
		ticket, err := c.api.GetUploadURL(ctx, f.Name, f.Size)
		if err != nil {
			return staged, fmt.Errorf("%s: %w", f.Name, err)
		}
		// transfer bounds itself by this file's size; a gigabyte held to the
		// bound that suits a small JSON reply fails on size alone over a link
		// that is working.
		err = c.transfer(ctx, ticket.UploadURL, f)
		if err != nil {
			return staged, err
		}
		s := Staged{Local: f, ID: ticket.FileID}
		staged = append(staged, s)
		if done != nil {
			done(s)
		}
	}
	return staged, nil
}

// ShareRequest is what makes staged files visible.
type ShareRequest struct {
	// Files are what Stage returned, in the order they should appear.
	Files []Staged
	// ChannelID must be a conversation id: completion resolves no names.
	ChannelID string
	// ThreadTS, when set, threads the message.
	ThreadTS string
	// Comment, when set, is what the message says.
	Comment string
}

// Shared is a completed upload.
type Shared struct {
	// IDs are the file ids Slack confirmed, in the order requested.
	IDs []string
	// TS is the message the files landed in, when the reply said. It is empty
	// when the reply carried no share for this channel, which is a success
	// whose anchor is the file ids.
	TS string
}

// ShareBoundary is called at the two moments the caller's own accounting turns
// on. Both run on the calling goroutine.
//
// They exist because those moments are inside this call. Without them a caller
// could only mark "completing" before it and "decided" after it returns, and a
// signal arriving between Slack's answer being classified and this function
// returning would record "unknown" for an outcome already known.
type ShareBoundary struct {
	// Issued is called once the completion request is on the wire. From here
	// the effect of an interruption is genuinely unknown.
	Issued func()
	// Decided is called once Slack's answer has been read and classified,
	// before anything else happens — a permalink lookup after this is a whole
	// round trip, and a signal during it must not turn a confirmed delivery
	// into an unknown one.
	Decided func(Shared, error)
}

// Share runs leg 3, the only leg that makes anything visible.
func (c *Client) Share(ctx context.Context, r ShareRequest, at ShareBoundary) (Shared, error) {
	req := slackwrite.CompleteRequest{
		ChannelID: r.ChannelID,
		ThreadTS:  r.ThreadTS,
		Comment:   r.Comment,
		Files:     make([]slackwrite.CompleteFile, 0, len(r.Files)),
	}
	for _, f := range r.Files {
		req.Files = append(req.Files, slackwrite.CompleteFile{ID: f.ID, Title: f.Name})
	}

	if at.Issued != nil {
		at.Issued()
	}
	reply, err := c.api.CompleteUpload(ctx, req)

	shared, err := decide(reply, err, r)
	if at.Decided != nil {
		at.Decided(shared, err)
	}
	return shared, err
}

// decide turns Slack's answer into an outcome. It is separate from Share so
// that the boundary callback fires on exactly what the caller is returned.
func decide(reply *slackwrite.Completion, err error, r ShareRequest) (Shared, error) {
	if err != nil {
		return Shared{}, err
	}
	err = reconcile(reply.Files, r.Files)
	if err != nil {
		return Shared{}, err
	}
	ts, err := consensus(reply.Files, r.ChannelID)
	if err != nil {
		return Shared{}, err
	}
	ids := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		ids = append(ids, f.ID)
	}
	return Shared{IDs: ids, TS: ts}, nil
}

// ErrUnreconciled is a completion reply that does not describe what was asked
// for. It is not a refusal: Slack answered ok, so what it did is unknown.
var ErrUnreconciled = errors.New("the completion reply does not match what was staged")

// reconcile requires the returned ids to be exactly the requested ids, each
// once. A reply that omits one, repeats one or names one that was never staged
// is an error rather than a success with a quietly different list.
func reconcile(got []slackwrite.SharedFile, want []Staged) error {
	seen := map[string]int{}
	for _, f := range got {
		seen[f.ID]++
	}
	var missing, extra []string
	for _, w := range want {
		switch seen[w.ID] {
		case 0:
			missing = append(missing, w.ID)
		case 1:
			delete(seen, w.ID)
		default:
			return fmt.Errorf("%w: %s appears %d times", ErrUnreconciled, w.ID, seen[w.ID])
		}
	}
	for id := range seen {
		extra = append(extra, id)
	}
	sort.Strings(extra)
	switch {
	case len(missing) > 0 && len(extra) > 0:
		return fmt.Errorf("%w: no answer for %s, and %s was never staged",
			ErrUnreconciled, strings.Join(missing, ", "), strings.Join(extra, ", "))
	case len(missing) > 0:
		return fmt.Errorf("%w: no answer for %s", ErrUnreconciled, strings.Join(missing, ", "))
	case len(extra) > 0:
		return fmt.Errorf("%w: %s was never staged", ErrUnreconciled, strings.Join(extra, ", "))
	}
	return nil
}

// ErrManyMessages is a reply whose share timestamps disagree.
var ErrManyMessages = errors.New("the completion reply describes more than one message")

// consensus is the one timestamp every share for this channel agrees on.
//
// Disagreement is not resolved by picking one. The record would then name a
// message that holds some of the files, and the permalink would point at it as
// though it were the whole thing.
func consensus(files []slackwrite.SharedFile, channelID string) (string, error) {
	var found string
	for _, f := range files {
		for _, ts := range f.Shares.TimestampsFor(channelID) {
			if found == "" {
				found = ts
				continue
			}
			if ts != found {
				return "", fmt.Errorf("%w: %s and %s", ErrManyMessages, found, ts)
			}
		}
	}
	return found, nil
}

// provenRefusal names the completion errors whose meaning is that nothing was
// shared. Slack does not document leg 3 as transactional across the files
// array, so a code is on this list only when its own meaning forecloses the
// share — not merely because it looks like argument validation.
var provenRefusal = map[string]bool{
	// The token carries no authority to share anything, so nothing was.
	slackwrite.CodeMissingScope: true,
	// Permission to post to the target is the operation; denied, it did not
	// happen.
	"posting_to_channel_denied": true,
}

// ClassifyShare says what an error from Share means for the message.
//
// It is not slackwrite.Classify. That function sends every unrecognised error
// to Ambiguous, which is right for one irreversible call, but leg 3 needs the
// opposite default read off a list of codes whose meaning is established:
// anything not on it — file_not_found included, since it says one ticket was
// missing and nothing about the rest — is unknown.
func ClassifyShare(err error) slackwrite.Outcome {
	if err == nil {
		return slackwrite.Delivered
	}
	var pre *slackwrite.PreflightError
	if errors.As(err, &pre) {
		return slackwrite.Rejected
	}
	if errors.Is(err, ErrUnreconciled) || errors.Is(err, ErrManyMessages) {
		return slackwrite.Ambiguous
	}
	var api *slackwrite.APIError
	if errors.As(err, &api) && provenRefusal[api.Code] {
		return slackwrite.Rejected
	}
	return slackwrite.Ambiguous
}
