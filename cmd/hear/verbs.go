package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

func (h *hear) whoami() int {
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}
	resp, err := c.AuthTest(context.Background())
	if err != nil {
		return h.fail(err)
	}
	// whoami already paid for auth.test, so the result is written through: one
	// whoami warms the marker every later read uses, at no extra call. The error
	// is dropped deliberately — the answer came from Slack, and an unwritable
	// cache must not fail a whoami that already has it.
	_ = cache.Write(cache.IdentityFile, cache.Identity{
		UserID: resp.UserID, User: resp.User, Team: resp.Team,
		CredentialKey: h.src.credentialKey(),
	})
	if h.asJSON {
		return h.emit(render.Identity{
			Kind: render.KindIdentity, User: resp.User, Team: resp.Team, UserID: resp.UserID,
		})
	}
	// Two spaces before the id: the column is part of the format people read.
	fmt.Fprintf(h.stdout, "%s @ %s  (%s)\n", resp.User, resp.Team, resp.UserID)
	return cli.ExitOK
}

func (h *hear) channels(filter string) int {
	convs, err := h.store.Conversations(context.Background())
	if err != nil {
		return h.fail(err)
	}
	refs := []render.ChannelRef{}
	for _, c := range convs {
		// A DM has no name, and neither does anything else Slack hid from the
		// list. Both are dropped: this lists channels.
		if c.Name == "" || !matches("#"+c.Name, filter) {
			continue
		}
		refs = append(refs, render.ChannelRef{ID: c.ID, Name: c.Name})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].ID < refs[j].ID
	})
	if h.asJSON {
		return h.emit(render.Channels{Kind: render.KindChannels, Channels: refs})
	}
	// Unique, as `sort -u` was: the id distinguishes two conversations the
	// transcript form cannot, so only the structured output lists both.
	seen := ""
	for i, r := range refs {
		if i > 0 && r.Name == seen {
			continue
		}
		seen = r.Name
		fmt.Fprintln(h.stdout, "#"+r.Name)
	}
	return cli.ExitOK
}

func (h *hear) dms(filter string) int {
	ctx := context.Background()
	convs, err := h.store.Conversations(ctx)
	if err != nil {
		return h.fail(err)
	}
	users, err := h.store.Users(ctx)
	if err != nil {
		return h.fail(err)
	}
	names := render.NamesOf(users)

	// Deleted accounts are listed here when a DM with them still exists, which
	// disagrees with resolution, where a deleted account matches nothing. The
	// disagreement is deliberate: the list
	// answers "what conversations do I have", and a DM with someone who has
	// left is one of them, whether or not it can still be addressed by name.
	list := []render.DM{}
	for _, c := range convs {
		if !c.IsIM {
			continue
		}
		label := names[c.User]
		if label == "" {
			label = c.User
		}
		if !matches("@"+label, filter) {
			continue
		}
		list = append(list, render.DM{ID: c.ID, UserID: c.User, Name: label})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].ID < list[j].ID
	})
	if h.asJSON {
		return h.emit(render.DMs{Kind: render.KindDMs, DMs: list})
	}
	seen := ""
	for i, d := range list {
		if i > 0 && d.Name == seen {
			continue
		}
		seen = d.Name
		fmt.Fprintln(h.stdout, "@"+d.Name)
	}
	return cli.ExitOK
}

func (h *hear) read(args []string) int {
	if len(args) == 0 {
		return h.usage("read needs a channel, a person or a permalink")
	}
	target, rest := args[0], args[1:]

	if link, isLink, err := parsePermalink(target); isLink {
		if err != nil {
			return h.fail(err)
		}
		// A permalink names one message, so there is nothing for a count or a
		// window to mean. Refusing is the honest answer; honouring the address
		// and ignoring the rest would read something nobody asked for.
		if len(rest) > 0 {
			return h.usage("a permalink names one message; a count applies only to a channel")
		}
		if h.since != 0 {
			return h.usage("a permalink names one message; --since applies only to a channel")
		}
		return h.readThread(render.ChannelRef{ID: link.channelID, Input: target}, link.parentTS())
	}

	count := defaultCount
	switch len(rest) {
	case 0:
	case 1:
		if h.since != 0 {
			return h.usage("--since and a count ask for different things; give one")
		}
		n, err := strconv.Atoi(rest[0])
		if err != nil || n <= 0 {
			return h.usage("count must be a positive whole number: " + rest[0])
		}
		count = n
	default:
		return h.usage("read takes an address and at most a count")
	}

	ctx := context.Background()
	t, err := h.address(ctx, target)
	if err != nil {
		return h.fail(err)
	}
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}

	var msgs []slackread.Message
	var complete bool
	if h.since != 0 {
		msgs, complete, err = window(ctx, c, t.ID, time.Now().Add(-h.since))
	} else {
		msgs, complete, err = latest(ctx, c, t.ID, count)
	}
	if err != nil {
		return h.fail(err)
	}
	// conversations.history answers newest-first; a transcript reads downwards.
	slices.Reverse(msgs)
	return h.transcript(channelRef(t), "", msgs, complete)
}

func (h *hear) thread(args []string) int {
	if len(args) == 0 {
		return h.usage("thread needs an address and a ts")
	}
	target, rest := args[0], args[1:]

	if link, isLink, err := parsePermalink(target); isLink {
		if err != nil {
			return h.fail(err)
		}
		if len(rest) > 0 {
			return h.usage("the permalink already names the timestamp")
		}
		return h.readThread(render.ChannelRef{ID: link.channelID, Input: target}, link.parentTS())
	}
	if len(rest) != 1 {
		return h.usage("thread needs an address and a ts")
	}
	t, err := h.address(context.Background(), target)
	if err != nil {
		return h.fail(err)
	}
	return h.readThread(channelRef(t), rest[0])
}

func (h *hear) readThread(ch render.ChannelRef, ts string) int {
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}
	// Not reversed: conversations.replies answers oldest-first, beginning with
	// the parent, which is already the order a thread reads in.
	msgs, complete, err := replies(context.Background(), c, ch.ID, ts)
	if err != nil {
		return h.fail(err)
	}
	return h.transcript(ch, ts, msgs, complete)
}

func (h *hear) search(query string) int {
	// The user list is loaded before the search: a cold cache is filled by the
	// same credential, so failing here fails before a query reaches Slack
	// rather than after.
	names, err := h.names()
	if err != nil {
		return h.fail(err)
	}
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}
	matches, err := c.Search(context.Background(), query, searchCount)
	if err != nil {
		return h.fail(err)
	}
	if h.asJSON {
		return h.emit(render.SearchResult{
			Kind: render.KindSearch, Query: query, Matches: render.Hits(matches, names),
		})
	}
	opts := render.Options{ShowTS: h.showTS}
	err = render.WriteSearch(h.stdout, matches, names, opts)
	if err != nil {
		return h.fail(err)
	}
	return cli.ExitOK
}

func (h *hear) refresh() int {
	err := cache.Clear()
	if err != nil {
		return h.fail(err)
	}
	if h.asJSON {
		return h.emit(render.Refresh{Kind: render.KindRefresh, Cleared: true})
	}
	fmt.Fprintln(h.stdout, "cache cleared")
	return cli.ExitOK
}

// transcript writes msgs, having resolved the names the mentions in them refer
// to. The user list is loaded here rather than before the messages, so a
// transcript Slack refuses costs no users.list call.
func (h *hear) transcript(ch render.ChannelRef, threadTS string, msgs []slackread.Message, complete bool) int {
	names, err := h.names()
	if err != nil {
		return h.fail(err)
	}
	if h.asJSON {
		return h.emit(render.Transcript{
			Kind:     render.KindTranscript,
			Channel:  ch,
			ThreadTS: threadTS,
			Messages: render.Messages(msgs, names),
			Complete: complete,
		})
	}
	opts := render.Options{
		ShowTS:        h.showTS,
		ThreadCommand: "hear thread " + address(ch),
	}
	// The identity is a label the read already holds, never a fetch: a cold or
	// unreadable one leaves SelfID empty and the transcript renders unchanged.
	// It is used only when it came from the credential in use — a token switch
	// leaves the file behind, and marking another account's reaction "you" is
	// worse than marking nothing.
	var id cache.Identity
	err = cache.Read(cache.IdentityFile, &id)
	if err == nil {
		if key := h.src.credentialKey(); key != "" && key == id.CredentialKey {
			opts.SelfID = id.UserID
		}
	}
	err = render.WriteTranscript(h.stdout, msgs, names, opts)
	if err != nil {
		return h.fail(err)
	}
	return cli.ExitOK
}

func (h *hear) names() (render.Names, error) {
	users, err := h.store.Users(context.Background())
	if err != nil {
		return nil, err
	}
	return render.NamesOf(users), nil
}

func (h *hear) emit(v any) int {
	err := render.JSON(h.stdout, v)
	if err != nil {
		return h.fail(err)
	}
	return cli.ExitOK
}

func (h *hear) usage(msg string) int { return cli.Usage(h.stderr, name, usage, msg) }

// channelRef describes a resolved target for the output.
func channelRef(t resolve.Target) render.ChannelRef {
	return render.ChannelRef{ID: t.ID, Name: t.Name, Input: t.Input}
}

// address is how a channel is named back to the reader inside a hint: what
// they typed, when that is something they can type again.
//
// A permalink is not. It names one message, so `hear thread <link> <ts>` is
// refused as a contradiction — and a hint that cannot be run is worse than no
// hint, because it is only discovered to be wrong after being tried.
func address(ch render.ChannelRef) string {
	if ch.Input != "" && !strings.HasPrefix(ch.Input, "http") {
		return ch.Input
	}
	return ch.ID
}

// matches is a case-insensitive substring filter, and no match is an empty
// result rather than an error. It is a substring and not a pattern — a regexp
// would read "a.b" as a
// regular expression, and a filter is an argument, not a program.
func matches(line, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(line), strings.ToLower(filter))
}

// latest collects the n most recent messages, newest first.
//
// One call when n fits in a page. Past that it follows the cursor rather than
// truncating, and stops as soon as
// it has n: only the caller knows a transcript is bounded by a count.
func latest(ctx context.Context, c *slackread.Client, channelID string, n int) ([]slackread.Message, bool, error) {
	var out []slackread.Message
	cursor := ""
	for range maxPages {
		limit := min(n-len(out), maxLimit)
		p, err := c.History(ctx, channelID, slackread.HistoryOptions{Limit: limit, Cursor: cursor})
		if err != nil {
			return nil, false, err
		}
		out = append(out, p.Messages...)
		cursor = p.NextCursor
		if len(out) >= n || cursor == "" {
			// Complete means nothing was left out: the channel ran out before
			// the count did.
			complete := cursor == "" && len(out) <= n
			if len(out) > n {
				out = out[:n]
			}
			return out, complete, nil
		}
	}
	return nil, false, fmt.Errorf("conversations.history: still paginating after %d pages", maxPages)
}

// window collects everything posted since oldest, newest first.
//
// It pages to the end of the window rather than stopping at the first page: a
// window has no count to satisfy, so one call would silently answer "nothing
// happened" for any period busier than a page. It does stop at maxWindow, and
// says so: what it keeps is the newest of the window, because that is the end
// conversations.history answers from.
func window(ctx context.Context, c *slackread.Client, channelID string, oldest time.Time) ([]slackread.Message, bool, error) {
	var out []slackread.Message
	cursor := ""
	from := slackTime(oldest)
	for range maxPages {
		p, err := c.History(ctx, channelID, slackread.HistoryOptions{
			Limit: pageSize, Oldest: from, Cursor: cursor,
		})
		if err != nil {
			return nil, false, err
		}
		out = append(out, p.Messages...)
		if len(out) >= maxWindow {
			return out[:maxWindow], false, nil
		}
		if cursor = p.NextCursor; cursor == "" {
			return out, true, nil
		}
	}
	return nil, false, fmt.Errorf("conversations.history: still paginating after %d pages", maxPages)
}

// replies collects a whole thread, oldest first. A thread has no count to
// stop at, so it is walked to its end.
func replies(ctx context.Context, c *slackread.Client, channelID, ts string) ([]slackread.Message, bool, error) {
	var out []slackread.Message
	cursor := ""
	for range maxPages {
		p, err := c.Replies(ctx, channelID, ts, slackread.HistoryOptions{Limit: pageSize, Cursor: cursor})
		if err != nil {
			return nil, false, err
		}
		out = append(out, p.Messages...)
		if cursor = p.NextCursor; cursor == "" {
			return out, true, nil
		}
	}
	return nil, false, fmt.Errorf("conversations.replies: still paginating after %d pages", maxPages)
}

// slackTime formats an instant as the `oldest` bound. Slack's timestamps carry
// six digits after the point, and it accepts the same shape back.
func slackTime(t time.Time) string {
	return fmt.Sprintf("%d.000000", t.Unix())
}
