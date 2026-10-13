package render

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/humansize"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

// KindInbox tags `hear inbox --json`.
const KindInbox = "inbox"

// The kinds of conversation an inbox groups messages under.
const (
	ConversationDM      = "dm"
	ConversationGroupDM = "group_dm"
	ConversationChannel = "channel"
)

// Where a conversation's or a thread's messages came from. SourceSearch means
// they are search hits: what the index held, which is not always the whole of
// what Slack holds.
const (
	SourceReplies = "replies"
	SourceSearch  = "search"
)

// The searches an inbox finds things with, in the order they run, as its
// sources name them.
const (
	SearchDMs            = "dms"
	SearchMentions       = "mentions"
	SearchThreads        = "threads"
	SearchMentionThreads = "mention_threads"
)

// parentWidth is how many runes of a thread's parent the text form shows. The
// parent is context for the replies under it, so one line is enough to say
// which thread this is.
const parentWidth = 80

// replyIndent sets a thread's replies under their parent.
const replyIndent = "    "

// Inbox is `hear inbox --json`.
//
// Complete is the conjunction of every source and every conversation and
// thread below it: a caller that checks only this field cannot mistake a
// partial answer for a whole one.
type Inbox struct {
	Kind          string              `json:"kind"`
	Since         string              `json:"since"`
	Until         string              `json:"until"`
	UserID        string              `json:"user_id"`
	Complete      bool                `json:"complete"`
	Calls         InboxCalls          `json:"calls"`
	Sources       []InboxSource       `json:"sources"`
	Conversations []InboxConversation `json:"conversations"`
}

// InboxCalls is how many requests the run made, against the ceiling it was
// given. Cache counts the cache fills, which the ceiling does not cover: one is
// paid once and then shared by every command.
type InboxCalls struct {
	Used  int `json:"used"`
	Max   int `json:"max"`
	Cache int `json:"cache"`
}

// InboxSource is one search the inbox ran to find what is new. Hits counts the
// matches inside the range it covers. Unread lists the threads a source found
// but could not read to the end, so a caller can read them itself; only the
// mention lookback has any, since its threads are known to matter only once
// read.
type InboxSource struct {
	Name     string        `json:"name"`
	Query    string        `json:"query"`
	Complete bool          `json:"complete"`
	Hits     int           `json:"hits"`
	Reason   string        `json:"reason,omitempty"`
	Unread   []InboxUnread `json:"unread,omitempty"`
}

// InboxUnread is a thread a source found and could not read, and why.
type InboxUnread struct {
	Channel  ChannelRef `json:"channel"`
	ThreadTS string     `json:"thread_ts"`
	Reason   string     `json:"reason"`
}

// InboxConversation is one conversation with something new in it. User is the
// other person in a 1:1 DM, when a message names them. Name is that person's
// label when the user list knows them: what `hear dms --json` gives them under
// the same key, and the author a message of theirs carries. It is empty for a
// person the list does not know, where `hear dms` falls back to the id, since
// User already carries the id. The text header is built from it. It is not a
// channel name, which a DM does not have.
type InboxConversation struct {
	Channel  ChannelRef     `json:"channel"`
	Kind     string         `json:"kind"`
	User     string         `json:"user,omitempty"`
	Name     string         `json:"name,omitempty"`
	Source   string         `json:"source"`
	Complete bool           `json:"complete"`
	Reason   string         `json:"reason,omitempty"`
	Messages []InboxMessage `json:"messages"`
	Threads  []InboxThread  `json:"threads"`
}

// InboxThread is a thread with new replies. Parent is nil when Slack did not
// return it; it is never also listed among the conversation's messages.
type InboxThread struct {
	ThreadTS string         `json:"thread_ts"`
	Parent   *InboxMessage  `json:"parent"`
	Replies  []InboxMessage `json:"replies"`
	Source   string         `json:"source"`
	Complete bool           `json:"complete"`
	Reason   string         `json:"reason,omitempty"`
}

// InboxMessage is a transcript message and whether it names the reader. It
// embeds Message so every field a transcript carries, blocks included, is
// here under the same name: a message taken from the inbox can be handed to
// sez exactly as one taken from `hear read --json`.
type InboxMessage struct {
	Message

	MentionsMe bool `json:"mentions_me"`
}

// WriteInbox writes the inbox as text: each conversation under a header, its
// messages and threads oldest first, each thread's replies indented under its
// parent, and a closing list of everything that is not complete.
//
// Every string that came from Slack goes through termsafe first. A display
// name or a message is chosen by its author, and the inbox puts more of them
// in front of a reader in one place than any other command does.
func WriteInbox(w io.Writer, in Inbox, opts Options) error {
	var b strings.Builder
	for _, c := range in.Conversations {
		b.WriteString(conversationHeader(c))
		b.WriteByte('\n')
		for _, e := range entries(c) {
			if e.thread != nil {
				writeThread(&b, c, *e.thread, opts)
				continue
			}
			writeMessage(&b, "", *e.message, opts)
		}
	}
	writeIncomplete(&b, in)
	_, err := io.WriteString(w, b.String())
	return err
}

// entry is a conversation's top-level message or one of its threads, so the
// two can be read in one order.
type entry struct {
	ts      string
	message *InboxMessage
	thread  *InboxThread
}

// entries interleaves a conversation's messages and threads by timestamp,
// a thread placed where its parent was posted.
func entries(c InboxConversation) []entry {
	out := make([]entry, 0, len(c.Messages)+len(c.Threads))
	for i := range c.Messages {
		out = append(out, entry{ts: c.Messages[i].TS, message: &c.Messages[i]})
	}
	for i := range c.Threads {
		out = append(out, entry{ts: c.Threads[i].ThreadTS, thread: &c.Threads[i]})
	}
	sort.SliceStable(out, func(i, j int) bool { return TSLess(out[i].ts, out[j].ts) })
	return out
}

// ConversationLabel is how a conversation is named to a reader: @person for a
// 1:1 DM, by the name the JSON carries or else the id, the name Slack gives a
// group DM, #name for a channel, and the id when there is nothing better.
func ConversationLabel(c InboxConversation) string {
	switch {
	case c.Kind == ConversationDM && c.Name != "":
		return "@" + termsafe.Text(c.Name)
	case c.Kind == ConversationDM && c.User != "":
		return "@" + termsafe.Text(c.User)
	case c.Kind == ConversationChannel && c.Channel.Name != "":
		return "#" + termsafe.Text(c.Channel.Name)
	case c.Channel.Name != "":
		return termsafe.Text(c.Channel.Name)
	default:
		return termsafe.Text(c.Channel.ID)
	}
}

func conversationHeader(c InboxConversation) string {
	label := ConversationLabel(c)
	switch c.Kind {
	case ConversationDM:
		return label + "  (dm)"
	case ConversationGroupDM:
		return label + "  (group dm)"
	default:
		return label
	}
}

// writeThread writes a thread's parent on one line, with the command that
// opens the whole thread, and its new replies under it. The replies are always
// written: a count of new replies with nothing under it is the line a reader
// skips.
func writeThread(b *strings.Builder, c InboxConversation, t InboxThread, opts Options) {
	hint := fmt.Sprintf("  (hear thread %s %s)", termsafe.Text(c.Channel.ID), termsafe.Text(t.ThreadTS))
	b.WriteString("thread ")
	if t.Parent == nil {
		b.WriteString(Stamp(t.ThreadTS, opts.location()))
		b.WriteString(" (parent not returned)")
	} else {
		b.WriteString(head(*t.Parent, opts))
		b.WriteString(termsafe.Text(oneLine(t.Parent.TextRendered, parentWidth)))
	}
	b.WriteString(hint)
	b.WriteByte('\n')
	for i := range t.Replies {
		writeMessage(b, replyIndent, t.Replies[i], opts)
	}
}

// head is a message line up to its text: the optional raw timestamp, the date
// column and the author, as a transcript has them.
func head(m InboxMessage, opts Options) string {
	var b strings.Builder
	if opts.ShowTS {
		b.WriteString(m.TS)
		b.WriteString("  ")
	}
	b.WriteString(Stamp(m.TS, opts.location()))
	b.WriteString("  ")
	b.WriteString(termsafe.Text(m.Author))
	b.WriteString("  ")
	return b.String()
}

func writeMessage(b *strings.Builder, indent string, m InboxMessage, opts Options) {
	b.WriteString(indent)
	b.WriteString(head(m, opts))
	text := termsafe.TextKeepingNewlines(m.TextRendered)
	b.WriteString(strings.ReplaceAll(text, "\n", "\n"+indent+continuation))
	if len(m.Files) > 0 {
		fmt.Fprintf(b, "\n%s%s%s", indent, continuation, fileRefsLine(m.Files))
	}
	b.WriteByte('\n')
}

// fileRefsLine names a message's attachments, as a transcript's files line
// does, from the structured form the inbox holds.
func fileRefsLine(files []FileRef) string {
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("%s (%s)", termsafe.Text(f.Name), humansize.Bytes(f.Size)))
	}
	return fmt.Sprintf("⤷ %d %s: %s", len(files), noun, strings.Join(parts, ", "))
}

// oneLine is s on a single line of at most width runes, marked when cut.
func oneLine(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width]) + "…"
}

// writeIncomplete names every search, conversation and thread that is not
// complete, and why. Nothing is written for a complete inbox.
func writeIncomplete(b *strings.Builder, in Inbox) {
	if in.Complete {
		return
	}
	b.WriteString("incomplete:\n")
	writeIncompleteSources(b, in)
	for _, c := range in.Conversations {
		label := ConversationLabel(c)
		if !c.Complete && c.Reason != "" {
			fmt.Fprintf(b, "  %s: %s\n", label, termsafe.Text(c.Reason))
		}
		for _, t := range c.Threads {
			if !t.Complete {
				fmt.Fprintf(b, "  thread %s %s: %s\n", label, termsafe.Text(t.ThreadTS), termsafe.Text(t.Reason))
			}
		}
	}
}

// writeIncompleteSources names each search that is not complete, and each
// thread one found and could not read to the end. A thread read in part that
// held news is listed with its conversation instead; its unread entry would
// only say the same thing twice.
func writeIncompleteSources(b *strings.Builder, in Inbox) {
	shown := map[string]bool{}
	for _, c := range in.Conversations {
		for _, t := range c.Threads {
			shown[c.Channel.ID+"/"+t.ThreadTS] = true
		}
	}
	for _, s := range in.Sources {
		if !s.Complete {
			fmt.Fprintf(b, "  search %s: %s\n", s.Name, termsafe.Text(s.Reason))
		}
		for _, u := range s.Unread {
			if shown[u.Channel.ID+"/"+u.ThreadTS] {
				continue
			}
			fmt.Fprintf(b, "  thread %s %s: not read to the end, %s\n",
				termsafe.Text(u.Channel.ID), termsafe.Text(u.ThreadTS), termsafe.Text(u.Reason))
		}
	}
}

// TSLess orders two Slack timestamps by time: seconds as a number, then the
// part after the point as a decimal fraction, so "5.1" is after "5.000100" as
// it would be on a number line. Slack writes six digits after the point, and a
// timestamp with fewer is still ordered correctly. One that does not parse
// sorts by its text, after every one that does.
func TSLess(a, b string) bool {
	as, af, aok := splitTS(a)
	bs, bf, bok := splitTS(b)
	switch {
	case aok && bok:
		if as != bs {
			return as < bs
		}
		width := max(len(af), len(bf))
		return padRight(af, width) < padRight(bf, width)
	case aok != bok:
		return aok
	default:
		return a < b
	}
}

// ValidTS reports whether ts reads as a Slack timestamp: digits, a point, and
// digits. A message without one cannot be ordered, windowed or addressed.
func ValidTS(ts string) bool {
	_, _, ok := splitTS(ts)
	return ok
}

// splitTS is a timestamp's seconds and the digits after its point.
func splitTS(ts string) (int64, string, bool) {
	sec, frac, ok := strings.Cut(ts, ".")
	if !ok || !allDigits(sec) || !allDigits(frac) {
		return 0, "", false
	}
	s, err := strconv.ParseInt(sec, 10, 64)
	return s, frac, err == nil
}

func padRight(digits string, width int) string {
	return digits + strings.Repeat("0", width-len(digits))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
