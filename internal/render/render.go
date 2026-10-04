// Package render turns Slack messages into the two shapes a reader wants: the
// transcript a person reads, and the JSON a program parses.
//
// Message text and display names are data here, never program. A workspace
// member — including an external Slack Connect guest — chooses their own
// display name, so mention substitution goes through ReplaceAllStringFunc,
// whose return value is used literally: a name containing "$1" or "${x}" is
// printed, not expanded.
package render

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/humansize"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

// continuation is what a wrapped line is indented by, so the text of a
// multi-line message stays in the text column instead of under the date. It is
// twenty spaces, and it does not widen with --ts: the column is a constant,
// not a computed offset.
const continuation = "                    "

// stampFormat is strftime("%m-%d %H:%M"). The year is omitted deliberately —
// a transcript is nearly always recent, and the two columns it saves matter
// more at a terminal width than the year does.
const stampFormat = "01-02 15:04"

// mention matches Slack's user reference. It deliberately does not match the
// <@U123|label> form, which already carries the label a reader needs.
var mention = regexp.MustCompile(`<@([A-Za-z0-9]+)>`)

// Names maps a user id to the label a reader sees.
type Names map[string]string

// NamesOf indexes users by id. The label is cache.User.Label: the display name
// when set, else the real name, else the handle, else the id.
func NamesOf(users []cache.User) Names {
	n := make(Names, len(users))
	for _, u := range users {
		n[u.ID] = u.Label()
	}
	return n
}

// Text resolves <@U…> mentions to @label. An id nobody knows renders as the id
// itself rather than as "?", so a mention of an external or deleted account is
// still traceable.
func (n Names) Text(s string) string {
	if !strings.Contains(s, "<@") {
		return s
	}
	return mention.ReplaceAllStringFunc(s, func(m string) string {
		id := m[2 : len(m)-1]
		if label := n[id]; label != "" {
			return "@" + label
		}
		return "@" + id
	})
}

// Author names the writer of m: the display name when the user list knows
// them, else the username Slack attached, else the bot id, else "?".
func (n Names) Author(m slackread.Message) string {
	if label := n[m.User]; label != "" {
		return label
	}
	if m.Username != "" {
		return m.Username
	}
	if m.BotID != "" {
		return m.BotID
	}
	return "?"
}

// Options controls how a transcript is written.
type Options struct {
	// ShowTS prefixes each message with its raw Slack timestamp, which is what
	// `sez --thread` and `tap` take as an argument.
	ShowTS bool
	// Location is the zone the date column is rendered in. Nil is time.Local,
	// the zone the machine is in; a test names one explicitly
	// because time.Local is resolved once per process and cannot be changed
	// afterwards.
	Location *time.Location
	// ThreadCommand is the command that opens a thread, e.g. "hear thread
	// #eng". A message with replies carries it as a hint, with its own ts
	// appended: naming the command is what makes the hint runnable.
	ThreadCommand string
	// SelfID is the reader's own user id, warm from the cache. When it appears
	// among a reaction's users that reactor renders as "you" and is promoted to
	// the front, so the line answers whether the reaction is the reader's to
	// remove. Empty leaves the line exactly as it renders today.
	SelfID string
}

func (o Options) location() *time.Location {
	if o.Location == nil {
		return time.Local
	}
	return o.Location
}

// WriteTranscript writes messages oldest-first, one block per message. Ordering is
// the caller's: conversations.history answers newest-first and must be
// reversed, while conversations.replies is already oldest-first.
func WriteTranscript(w io.Writer, msgs []slackread.Message, names Names, opts Options) error {
	loc := opts.location()
	var b strings.Builder
	for _, m := range msgs {
		b.Reset()
		if opts.ShowTS {
			b.WriteString(m.TS)
			b.WriteString("  ")
		}
		b.WriteString(Stamp(m.TS, loc))
		b.WriteString("  ")
		b.WriteString(names.Author(m))
		b.WriteString("  ")
		b.WriteString(strings.ReplaceAll(names.Text(m.Text), "\n", "\n"+continuation))
		if len(m.Reactions) > 0 {
			fmt.Fprintf(&b, "\n%s%s", continuation, reactionsLine(m.Reactions, names, opts.SelfID))
		}
		if len(m.Files) > 0 {
			fmt.Fprintf(&b, "\n%s%s", continuation, filesLine(m.Files))
		}
		if m.ReplyCount > 0 && opts.ThreadCommand != "" {
			fmt.Fprintf(&b, "\n%s⤷ %d replies — %s %s",
				continuation, m.ReplyCount, opts.ThreadCommand, m.TS)
		}
		_, err := fmt.Fprintln(w, b.String())
		if err != nil {
			return err
		}
	}
	return nil
}

// filesLine names the attachments on a message, so a reader can see there is
// something to fetch and what `hear files` would write.
//
// Every file is named, however many there are: the count alone would say a
// message has attachments without saying whether any of them is the one being
// looked for.
func filesLine(files []slackread.File) string {
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("%s (%s)", FileLabel(f), humansize.Bytes(f.Size)))
	}
	return fmt.Sprintf("⤷ %d %s: %s", len(files), noun, strings.Join(parts, ", "))
}

// FileLabel is what to call a file: its name, else the title Slack shows for
// one that has none, else its id.
//
// The result is stripped of control characters. A filename is chosen by whoever
// uploaded it, and one carrying a newline or an escape sequence would otherwise
// break the transcript apart or drive the terminal it is printed to.
func FileLabel(f slackread.File) string {
	label := f.Name
	if label == "" {
		label = f.Title
	}
	if label == "" {
		return f.ID
	}
	return strings.Map(func(r rune) rune {
		// Controls and the Unicode format characters alike: U+202E renders the
		// rest of the label right-to-left, so "annex\u202Efdp.scr" reads as
		// "annexrcs.pdf" in a transcript nobody would think to distrust.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, label)
}

// reactionsLine renders a message's reactions as "one per reaction joined by
// two spaces", each ":name: count (reactors)". Reactors resolve through the
// user list already loaded — this costs no lookup of its own — and an
// unresolved id keeps its id exactly as a mention does.
//
// The number of distinct reactions on a line is deliberately unbounded, where
// the reactors within one are capped: a message carrying a dozen emoji is rare
// enough, and dropping whole reactions would hide which emoji are present at
// all, where dropping a reactor only hides who.
func reactionsLine(reactions []slackread.Reaction, names Names, selfID string) string {
	parts := make([]string, 0, len(reactions))
	for _, r := range reactions {
		part := fmt.Sprintf(":%s: %d", r.Name, r.Count)
		if inside := reactors(r.Count, r.Users, names, selfID); inside != "" {
			part += " (" + inside + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "  ")
}

// reactors returns what goes inside the parentheses: the first three names
// and, when there are more, a +N overflow. The overflow counts from the
// reaction's total, not from the length of the returned ids, because Slack
// truncates the users array on a heavily-reacted message — a count of 50 with
// five ids back must read (A, B, C +47), or the line contradicts itself. With
// no names at all the string is empty and the caller omits the parentheses.
//
// Slack does not order this array, so past three reactors the reader's own name
// can fall into the overflow and the line stops answering "is this mine". When
// selfID names one of them, that user renders as "you" and is promoted to the
// front — occupying one of the three slots rather than adding a fourth — so the
// answer is never pushed out of view.
//
// It answers only for the ids Slack sent. On a heavily-reacted message Slack
// truncates the array, and a reader outside that window cannot be found in it;
// the marker is then absent rather than wrong. Establishing it in that case
// would cost a reactions.get per reaction.
func reactors(count int, users []string, names Names, selfID string) string {
	if selfID != "" {
		hasSelf := false
		ordered := make([]string, 0, len(users)+1)
		for _, u := range users {
			if u == selfID {
				hasSelf = true
				continue
			}
			ordered = append(ordered, u)
		}
		if hasSelf {
			ordered = append([]string{selfID}, ordered...)
			users = ordered
		}
	}
	limit := 3
	var b strings.Builder
	i := 0
	for ; i < len(users) && i < limit; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		if selfID != "" && users[i] == selfID {
			b.WriteString("you")
		} else if label := names[users[i]]; label != "" {
			b.WriteString(label)
		} else {
			b.WriteString(users[i])
		}
	}
	if i == 0 {
		return ""
	}
	if overflow := count - i; overflow > 0 {
		fmt.Fprintf(&b, " +%d", overflow)
	}
	return b.String()
}

// WriteSearch writes search hits. They are not transcript lines: a hit names its
// channel because the results span the workspace, and it carries no date
// column. The author is the username Slack
// returned with the hit rather than a user-list lookup, because search results
// carry no user id to look up.
func WriteSearch(w io.Writer, matches []slackread.SearchMatch, names Names, opts Options) error {
	var b strings.Builder
	for _, m := range matches {
		b.Reset()
		if opts.ShowTS {
			b.WriteString(m.TS)
			b.WriteString("  ")
		}
		b.WriteString("#")
		b.WriteString(orQuestionMark(m.Channel.Name))
		b.WriteString("  ")
		b.WriteString(orQuestionMark(m.Username))
		b.WriteString("  ")
		b.WriteString(names.Text(m.Text))
		_, err := fmt.Fprintln(w, b.String())
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteFiles writes one line per attachment: what happened to it, and either
// where it landed or why it did not.
//
// A skipped file gets a line of its own rather than being left out. The absence
// of a line would be indistinguishable from a message that never carried the
// file, and the reason — a link to someone else's storage, a size limit — is
// the part a reader needs in order to decide what to do next.
func WriteFiles(w io.Writer, results []FileResult) error {
	for _, r := range results {
		var line string
		switch r.Status {
		case "written":
			line = fmt.Sprintf("%-7s %s (%s)", "wrote", r.Path, humansize.Bytes(r.Bytes))
		default:
			line = fmt.Sprintf("%-7s %s — %s", r.Status, r.Name, r.Reason)
		}
		_, err := fmt.Fprintln(w, line)
		if err != nil {
			return err
		}
	}
	return nil
}

func orQuestionMark(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// Stamp renders a Slack timestamp as the transcript's date column, in loc.
//
// A timestamp that will not parse is returned unchanged rather than dropped or
// replaced: it is the only handle the reader has on that message, and Slack
// has no other field that identifies one.
func Stamp(ts string, loc *time.Location) string {
	t, ok := parseTS(ts, loc)
	if !ok {
		return ts
	}
	return t.Format(stampFormat)
}

// parseTS splits a Slack timestamp — "1785708834.862009" — at the dot and
// reads the seconds. The fraction is a sequence number, not a fraction of a
// second, so it is not part of the instant.
func parseTS(ts string, loc *time.Location) (time.Time, bool) {
	sec, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0).In(loc), true
}
