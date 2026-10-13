package slackwrite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// ErrTooManyPages is a scheduled-message list whose cursor did not run out
// within maxListPages. A list cut short would read as complete, so it is
// refused instead.
var ErrTooManyPages = errors.New("the scheduled-message list did not end")

// maxListPages bounds the cursor walk, so a cursor that never empties cannot
// keep the command running.
const maxListPages = 50

// listPageSize is the limit asked for on each page.
const listPageSize = "100"

// ScheduleRequest is chat.scheduleMessage's argument set.
type ScheduleRequest struct {
	// Channel is the conversation, as chat.postMessage takes it: an id, a
	// #name, or a user id for a DM.
	Channel string
	// Text is the message.
	Text string
	// ThreadTS, when set, makes it a reply in that thread.
	ThreadTS string
	// PostAt is when Slack posts it. Slack takes whole seconds.
	PostAt time.Time
}

// ScheduleResponse is chat.scheduleMessage's reply, narrowed to the fields used.
type ScheduleResponse struct {
	// ID is the scheduled_message_id: the handle that lists and cancels it.
	ID string
	// Channel is the conversation id Slack will post in. A #name or a user id
	// given as the channel comes back as an id, and only an id can cancel it.
	Channel string
	// Text is what Slack stored, which can differ from what was submitted.
	Text string
}

// ScheduleMessage asks Slack to post a message at req.PostAt. Nothing is
// visible in the conversation until then. It is attempted at most once.
//
// A reply that passes the envelope but carries no scheduled_message_id, or no
// channel, is malformed, and so ambiguous: the message may be waiting without
// the id and the conversation id that together cancel it, which is a reason
// to look rather than to retry.
func (c *Client) ScheduleMessage(ctx context.Context, req ScheduleRequest) (*ScheduleResponse, error) {
	const method = methodSchedule
	form := url.Values{}
	form.Set("channel", req.Channel)
	form.Set("text", req.Text)
	form.Set("post_at", strconv.FormatInt(req.PostAt.Unix(), 10))
	if req.ThreadTS != "" {
		form.Set("thread_ts", req.ThreadTS)
	}

	var out struct {
		envelope

		ID      flexString `json:"scheduled_message_id"`
		Channel string     `json:"channel"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	}
	err := c.post(ctx, method, form, &out)
	if err != nil {
		return nil, err
	}
	if out.ID == "" || out.Channel == "" {
		return nil, &APIError{Method: method, Code: CodeMalformed}
	}
	return &ScheduleResponse{ID: string(out.ID), Channel: out.Channel, Text: out.Message.Text}, nil
}

// ScheduledMessage is one message waiting to be posted.
type ScheduledMessage struct {
	ID      string
	Channel string
	PostAt  time.Time
	Text    string
}

// ScheduledMessages lists the messages this credential has scheduled that have
// not yet posted, in channel when it is not empty, following the cursor to the
// end. Slack lists only what was scheduled through its API, not what was
// scheduled in its own client.
func (c *Client) ScheduledMessages(ctx context.Context, channel string) ([]ScheduledMessage, error) {
	var all []ScheduledMessage
	cursor := ""
	for range maxListPages {
		page, next, err := c.scheduledPage(ctx, channel, cursor)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("%s: %w within %d pages", methodScheduledList, ErrTooManyPages, maxListPages)
}

// scheduledPage is one page of chat.scheduledMessages.list and the cursor for
// the next, empty after the last.
func (c *Client) scheduledPage(ctx context.Context, channel, cursor string) ([]ScheduledMessage, string, error) {
	form := url.Values{}
	form.Set("limit", listPageSize)
	if channel != "" {
		form.Set("channel", channel)
	}
	if cursor != "" {
		form.Set("cursor", cursor)
	}

	var out struct {
		envelope

		Messages []struct {
			ID        flexString `json:"id"`
			ChannelID string     `json:"channel_id"`
			PostAt    flexString `json:"post_at"`
			Text      string     `json:"text"`
		} `json:"scheduled_messages"`
		Meta struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	err := c.post(ctx, methodScheduledList, form, &out)
	if err != nil {
		return nil, "", err
	}
	page := make([]ScheduledMessage, 0, len(out.Messages))
	for _, m := range out.Messages {
		secs, perr := strconv.ParseInt(string(m.PostAt), 10, 64)
		// The id and the conversation id together cancel an entry, so one
		// without either is as unusable as one without a time.
		if perr != nil || m.ID == "" || m.ChannelID == "" {
			return nil, "", &APIError{Method: methodScheduledList, Code: CodeMalformed}
		}
		page = append(page, ScheduledMessage{
			ID: string(m.ID), Channel: m.ChannelID, PostAt: time.Unix(secs, 0), Text: m.Text,
		})
	}
	return page, out.Meta.NextCursor, nil
}

// DeleteScheduledMessage cancels a scheduled message before it posts. Slack
// refuses once it has posted, and within 60 seconds of its time. It is
// attempted at most once.
func (c *Client) DeleteScheduledMessage(ctx context.Context, channelID, id string) error {
	form := url.Values{}
	form.Set("channel", channelID)
	form.Set("scheduled_message_id", id)

	var out envelope
	return c.post(ctx, methodUnschedule, form, &out)
}

// flexString decodes a JSON string or a JSON number into its text. Slack's
// reference shows post_at as a string in the schedule reply and a number in the
// list, and the list's id as a number where the schedule reply has a string, so
// neither shape is assumed.
type flexString string

// UnmarshalJSON accepts a string or a number.
func (f *flexString) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(b, []byte(`"`)) {
		var s string
		err := json.Unmarshal(b, &s)
		*f = flexString(s)
		return err
	}
	var n json.Number
	err := json.Unmarshal(b, &n)
	*f = flexString(n.String())
	return err
}
