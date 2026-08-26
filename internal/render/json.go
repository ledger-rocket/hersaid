package render

import (
	"encoding/json"
	"io"
	"time"

	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// The structured output is one JSON object per invocation, tagged with Kind.
//
// A single tagged object rather than a stream: every one of these commands
// answers exactly one question, and a consumer that has the whole answer can
// tell an empty result from a truncated one — which a stream of lines cannot
// express without a trailer nobody reads. Kind is what lets a caller dispatch
// without having to remember which verb it ran.
const (
	KindIdentity   = "identity"
	KindChannels   = "channels"
	KindDMs        = "dms"
	KindTranscript = "transcript"
	KindSearch     = "search"
	KindRefresh    = "refresh"
	KindFiles      = "files"
)

// Identity is `hear whoami --json`.
type Identity struct {
	Kind   string `json:"kind"`
	User   string `json:"user"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
}

// ChannelRef names a conversation. Name is empty for a DM, which has none in
// Slack's model, and Input is the argument that addressed it — absent when the
// conversation was not addressed by the caller, as in a search hit.
type ChannelRef struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Input string `json:"input,omitempty"`
}

// Channels is `hear channels --json`. The text form prints "#name" alone; the
// ids are here because the whole point of the structured output is that the
// next command can address what this one found without a second lookup.
type Channels struct {
	Kind     string       `json:"kind"`
	Channels []ChannelRef `json:"channels"`
}

// DM is one 1:1 conversation: the channel to address, the person it is with,
// and their label.
type DM struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
}

// DMs is `hear dms --json`.
type DMs struct {
	Kind string `json:"kind"`
	DMs  []DM   `json:"dms"`
}

// Reaction is one message reaction in structured form. Users carries raw ids:
// this is a wire form, and a consumer that has ids can resolve them.
type Reaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users"`
}

// Message is one message in structured form.
//
// Text is exactly what Slack sent, mentions and all, so a consumer can still
// see which user id was referenced. TextRendered is the same text with those
// mentions resolved, which is what the transcript shows. Both are always
// present: a field that appears only when it differs is a field consumers get
// wrong.
type Message struct {
	TS           string     `json:"ts"`
	Time         string     `json:"time,omitempty"`
	Author       string     `json:"author"`
	User         string     `json:"user,omitempty"`
	BotID        string     `json:"bot_id,omitempty"`
	Text         string     `json:"text"`
	TextRendered string     `json:"text_rendered"`
	ReplyCount   int        `json:"reply_count,omitempty"`
	Reactions    []Reaction `json:"reactions,omitempty"`
	// Files names the attachments, so a consumer can see there is something to
	// fetch and pass this message's ts to `hear files`. Partial alone would not
	// say so: it is also set by blocks, which cannot be downloaded.
	Files []FileRef `json:"files,omitempty"`
	// Partial marks a message whose Text is not the whole of it — one carrying
	// blocks, attachments or files, which the transcript does not render. A
	// consumer acting on the text of such a message is acting on something the
	// author never saw.
	Partial bool `json:"partial,omitempty"`
	// Blocks narrows Partial to the part that cannot be shown or preserved.
	// Files are listed in Files and are not this: they have names a render can
	// print, where a block reads as fallback text or as nothing. A consumer
	// deciding whether it may act on Text wants this rather than Partial.
	//
	// Always emitted, never omitted when false. A consumer has to tell "this
	// message has no blocks" from "this hear is too old to say", and an omitted
	// field cannot: sez refuses to change a message it cannot account for, and
	// a missing key that decoded as false would turn that refusal off.
	Blocks bool `json:"blocks"`
}

// Transcript is `hear read --json` and `hear thread --json`.
type Transcript struct {
	Kind    string     `json:"kind"`
	Channel ChannelRef `json:"channel"`
	// ThreadTS is set when the transcript is a thread rather than a channel.
	ThreadTS string    `json:"thread_ts,omitempty"`
	Messages []Message `json:"messages"`
	// Complete is false when Slack held more messages in the requested window
	// than are listed — a count that filled up, or a page bound that was hit.
	// Without it a truncated transcript would look exactly like a whole one.
	Complete bool `json:"complete"`
}

// FileRef is one attachment as a transcript reports it. Mode says whether there
// are bytes behind it: "hosted" and "snippet" can be downloaded, "external" is
// a link to storage elsewhere.
type FileRef struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Mimetype string `json:"mimetype,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

// FileResult is what became of one attachment. Status is "written", "skipped"
// or "failed"; Reason is set on the latter two and Path only on the first.
type FileResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Files is `hear files --json`. Dir is where the message's files were written,
// empty when none of them were.
type Files struct {
	Kind    string       `json:"kind"`
	Channel ChannelRef   `json:"channel"`
	TS      string       `json:"ts"`
	Dir     string       `json:"dir,omitempty"`
	Files   []FileResult `json:"files"`
}

// SearchHit is one search result.
type SearchHit struct {
	TS           string     `json:"ts"`
	Time         string     `json:"time,omitempty"`
	Channel      ChannelRef `json:"channel"`
	Author       string     `json:"author"`
	Text         string     `json:"text"`
	TextRendered string     `json:"text_rendered"`
	Permalink    string     `json:"permalink,omitempty"`
}

// SearchResult is `hear search --json`.
type SearchResult struct {
	Kind    string      `json:"kind"`
	Query   string      `json:"query"`
	Matches []SearchHit `json:"matches"`
}

// Refresh is `hear refresh --json`.
type Refresh struct {
	Kind    string `json:"kind"`
	Cleared bool   `json:"cleared"`
}

// Messages converts a transcript to its structured form.
func Messages(msgs []slackread.Message, names Names) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		var reactions []Reaction
		if len(m.Reactions) > 0 {
			reactions = make([]Reaction, 0, len(m.Reactions))
			for _, r := range m.Reactions {
				// Copied, and empty rather than nil: Slack omits the users
				// array on some reactions, and a null in place of [] breaks
				// every `jq '.reactions[].users[]'`. The copy also keeps a
				// consumer that mutates this slice out of the source message.
				users := make([]string, 0, len(r.Users))
				users = append(users, r.Users...)
				reactions = append(reactions, Reaction{Name: r.Name, Count: r.Count, Users: users})
			}
		}
		out = append(out, Message{
			TS:           m.TS,
			Time:         rfc3339(m.TS),
			Author:       names.Author(m),
			User:         m.User,
			BotID:        m.BotID,
			Text:         m.Text,
			TextRendered: names.Text(m.Text),
			ReplyCount:   m.ReplyCount,
			Reactions:    reactions,
			Files:        fileRefs(m.Files),
			Partial:      m.HasUnrenderedContent(),
			Blocks:       m.HasUnrenderedBlocks(),
		})
	}
	return out
}

func fileRefs(files []slackread.File) []FileRef {
	if len(files) == 0 {
		return nil
	}
	out := make([]FileRef, 0, len(files))
	for _, f := range files {
		out = append(out, FileRef{
			ID: f.ID, Name: FileLabel(f), Size: f.Size, Mimetype: f.Mimetype, Mode: f.Mode,
		})
	}
	return out
}

// Hits converts search results to their structured form.
func Hits(matches []slackread.SearchMatch, names Names) []SearchHit {
	out := make([]SearchHit, 0, len(matches))
	for _, m := range matches {
		out = append(out, SearchHit{
			TS:           m.TS,
			Time:         rfc3339(m.TS),
			Channel:      ChannelRef{ID: m.Channel.ID, Name: m.Channel.Name},
			Author:       orQuestionMark(m.Username),
			Text:         m.Text,
			TextRendered: names.Text(m.Text),
			Permalink:    m.Permalink,
		})
	}
	return out
}

// JSON writes v as this command's structured output.
//
// HTML escaping is off: Slack text is full of <@U…>, <#C…|name> and <http://…>,
// whose angle brackets the default encoder replaces with unicode escapes — the
// same string to a parser, and unreadable to the person working out why their
// parse is wrong. Indented for the same reason: stdout here is read by people
// at least as often as by programs, and jq neither knows nor cares.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// rfc3339 renders a Slack timestamp for the structured output, in UTC.
//
// Not the local zone the transcript uses: this is a document that travels, and
// a consumer reading one on another machine — or comparing two — has no way to
// know which zone produced it. The transcript is the opposite case, read by a
// person where they are.
func rfc3339(ts string) string {
	t, ok := parseTS(ts, time.UTC)
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339)
}

// KindCanvas tags the structured form of a canvas.
const KindCanvas = "canvas"

// Canvas is `hear canvas --json`.
type Canvas struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	// Body is Markdown, or the HTML Slack served when --html was given.
	Body string `json:"body"`
}
