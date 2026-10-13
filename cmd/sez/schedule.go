package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

// The scheduling options.
const (
	flagAt         = "--at"
	flagIn         = "--in"
	flagScheduled  = "--scheduled"
	flagUnschedule = "--unschedule"
)

// scheduleHorizon is how far ahead Slack takes a scheduled message.
const scheduleHorizon = 120 * 24 * time.Hour

// atLayouts are the local forms --at takes besides RFC 3339, which carries its
// own offset. clockLayout is a time today.
var atLayouts = []string{
	"2006-01-02T15:04:05", "2006-01-02T15:04",
	"2006-01-02 15:04:05", "2006-01-02 15:04",
}

const clockLayout = "15:04"

var errNotScheduled = errors.New("no scheduled message")

// scheduling reports that --at or --in was given.
func (f *flags) scheduling() bool { return f.at != "" || f.in != "" }

// actions counts the flags that each choose an action other than a send.
func (f *flags) actions() int {
	n := 0
	for _, set := range []bool{f.edit, f.del, f.scheduled, f.unschedule} {
		if set {
			n++
		}
	}
	return n
}

// scheduleTime is when a scheduled message posts, resolved against now, or zero
// when nothing is scheduled. done reports that the invocation ends here with
// code: a time that does not parse is a usage error, and one outside the window
// Slack takes is refused before anything is opened, as an empty message is.
//
// It is resolved on every run. --in therefore means a different time on the
// render and on the --send that follows it, and both say which.
func scheduleTime(stderr io.Writer, f flags, now time.Time) (time.Time, int, bool) {
	if !f.scheduling() {
		return time.Time{}, 0, false
	}
	postAt, refusal := resolveWhen(f, now)
	if refusal != "" {
		return time.Time{}, cli.Usage(stderr, name, usage, refusal), true
	}
	// Slack takes whole seconds, and the render and the record show what it
	// is sent.
	postAt = postAt.Truncate(time.Second)
	switch {
	case !postAt.After(now):
		return time.Time{}, fail(stderr, "refusing to schedule for %s: that time has passed", when(postAt)), true
	case postAt.After(now.Add(scheduleHorizon)):
		return time.Time{}, fail(stderr, "refusing to schedule for %s: Slack takes at most 120 days ahead", when(postAt)), true
	}
	return postAt, 0, false
}

// resolveWhen is the time --at or --in names, or the usage refusal for one
// that does not parse.
func resolveWhen(f flags, now time.Time) (time.Time, string) {
	if f.in != "" {
		// Under a second, truncating to the second Slack takes could land on
		// now or before it, so whether it passed would depend on the clock.
		d, err := time.ParseDuration(f.in)
		if err != nil || d < time.Second {
			return time.Time{}, "--in needs a duration of at least 1s, e.g. 90m or 2h30m"
		}
		return now.Add(d), ""
	}
	return parseAt(f.at, now)
}

// parseAt reads an --at value, or gives the usage refusal. A form without an
// offset is local time, in now's zone. HH:MM is today; one that has already
// passed is refused by the caller rather than moved to tomorrow, since the
// date is what the reader checks the render against and it should be the one
// that was typed.
func parseAt(s string, now time.Time) (time.Time, string) {
	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return t, ""
	}
	// A layout without a zone parses to the wall time as typed, in UTC.
	// localWall places it in the zone.
	for _, layout := range atLayouts {
		wall, perr := time.Parse(layout, s)
		if perr == nil {
			return localWall(s, wall, now.Location())
		}
	}
	clock, err := time.Parse(clockLayout, s)
	if err != nil {
		return time.Time{}, "--at needs RFC 3339 (2026-10-13T09:00:00Z), or local YYYY-MM-DDTHH:MM[:SS] or HH:MM"
	}
	y, m, d := now.Date()
	return localWall(s, time.Date(y, m, d, clock.Hour(), clock.Minute(), 0, 0, time.UTC), now.Location())
}

// wallLayout compares wall times to the second.
const wallLayout = "2006-01-02T15:04:05"

// localWall is the instant that wall's fields name in loc, or the refusal when
// they name none or two. At a daylight-saving change the clocks skip an hour,
// which time.Date moves to a real instant an hour away, and repeat one, which
// it resolves to either reading. Both would post an hour from the time typed.
func localWall(input string, wall time.Time, loc *time.Location) (time.Time, string) {
	want := wall.Format(wallLayout)
	t := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, loc)
	if t.Format(wallLayout) != want {
		return time.Time{}, "--at " + input + " is not a local time: the clocks skip it at a daylight-saving change"
	}
	_, offset := t.Zone()
	for _, other := range neighbourOffsets(t) {
		alt := t.Add(time.Duration(offset-other) * time.Second)
		if other == offset || alt.Format(wallLayout) != want {
			continue
		}
		first, second := t, alt
		if alt.Before(t) {
			first, second = alt, t
		}
		return time.Time{}, "--at " + input + " is ambiguous: the clocks show it twice at a daylight-saving change;" +
			" give RFC 3339 with an explicit offset, " + first.Format(time.RFC3339) + " or " + second.Format(time.RFC3339)
	}
	return t, ""
}

// neighbourOffsets are the UTC offsets, in seconds, of the zones just before
// and just after the one t falls in. A repeated wall time has its second
// reading at one of them.
func neighbourOffsets(t time.Time) []int {
	var out []int
	start, end := t.ZoneBounds()
	if !start.IsZero() {
		_, off := start.Add(-time.Second).Zone()
		out = append(out, off)
	}
	if !end.IsZero() {
		_, off := end.Zone()
		out = append(out, off)
	}
	return out
}

// when is a schedule time as sez shows it: local time, which is what the
// person meant, beside UTC, which is what the record holds.
func when(t time.Time) string {
	return t.Local().Format("Mon 2006-01-02 15:04:05 MST") +
		" (" + t.UTC().Format("2006-01-02 15:04:05") + " UTC)"
}

// listInvocation is the invocation for --scheduled, whose one argument, the
// conversation to list, is optional.
func listInvocation(f flags, stderr io.Writer) (invocation, int, bool) {
	if len(f.rest) > 1 {
		return invocation{}, cli.Usage(stderr, name, usage, "--scheduled takes at most one target"), true
	}
	inv := invocation{f: f, verb: verbList}
	if len(f.rest) == 1 {
		inv.channel = f.rest[0]
		if strings.HasPrefix(inv.channel, "-") {
			return invocation{}, fail(stderr, "refusing a channel that looks like a flag: %s", inv.channel), true
		}
	}
	return inv, 0, false
}

// list prints the scheduled messages waiting, in one conversation or in all.
//
// It is a read, so it renders nothing first and writes no audit record, and the
// write allowlist does not narrow it. It needs the write credential all the
// same, the only one sez holds.
func list(inv invocation, stdout, stderr io.Writer, baseURL string) int {
	wcred, err := resolveWrite()
	if err != nil {
		fmt.Fprint(stderr, noCredentialWhy+"  --scheduled asks Slack with it; there is nothing to render.\n")
		return cli.ExitFailure
	}
	channelID, refusal := listChannel(inv.channel)
	if refusal != "" {
		return fail(stderr, "%s", refusal)
	}
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	client := slackwrite.New(wcred, slackhttp.New(timeout), baseURL)
	// The list endpoint needs no scope, so a failure carries no hint to grant
	// one.
	msgs, err := client.ScheduledMessages(context.Background(), channelID)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	printScheduled(stdout, msgs)
	return 0
}

// listChannel is the conversation id to list, empty for all of them, or the
// refusal. The list endpoint takes an id only, so a #name must resolve.
func listChannel(input string) (string, string) {
	if input == "" {
		return "", ""
	}
	target, err := resolve.Resolve(context.Background(), cache.Offline(), resolve.ForEdit, input)
	if err != nil {
		return "", coldCache(err).Error()
	}
	if !channelRe.MatchString(target.ID) {
		return "", "refusing suspicious channel: " + target.ID
	}
	return target.ID, ""
}

// printScheduled prints msgs soonest first: the id, the conversation and the
// time on one line, then the text indented under it. The text is the
// author's own, but it is still printed through termsafe, as everything from
// Slack is.
func printScheduled(stdout io.Writer, msgs []slackwrite.ScheduledMessage) {
	if len(msgs) == 0 {
		fmt.Fprintln(stdout, "no scheduled messages")
		return
	}
	slices.SortStableFunc(msgs, func(a, b slackwrite.ScheduledMessage) int {
		return a.PostAt.Compare(b.PostAt)
	})
	for _, m := range msgs {
		fmt.Fprintf(stdout, "%s  %s  %s\n", termsafe.Text(m.ID), termsafe.Text(m.Channel), when(m.PostAt))
		for line := range strings.SplitSeq(termsafe.TextKeepingNewlines(m.Text), "\n") {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}
}

// schedule is the send path for a scheduled message. It has post's shape: the
// intent before the request, a catchable-signal outcome, one attempt and a
// classified outcome. A message Slack posts later is as much a send as one it
// posts now.
func schedule(a attempt, rec audit.Post, thread, text string) int {
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(a.stderr, a.log, rec, err.Error())
	}
	stop, err := begin(a.stderr, a.log, rec)
	if err != nil {
		return fail(a.stderr, "%v — refusing to send", err)
	}
	defer stop()

	client := slackwrite.New(a.cred, slackhttp.New(timeout), a.baseURL)
	resp, err := client.ScheduleMessage(context.Background(), slackwrite.ScheduleRequest{
		Channel: a.target.ID, Text: text, ThreadTS: thread, PostAt: rec.PostAt,
	})
	if err != nil {
		return changeOutcome(a.stderr, a.log, rec, err)
	}

	// A #name passed through, or a user id, is resolved by Slack, and a
	// cancellation can only name the id it resolved to. The reply is the one
	// place it appears, and ScheduleMessage refuses a reply without it.
	rec.OK, rec.ScheduledMessageID, rec.SlackText = audit.Succeeded, resp.ID, resp.Text
	rec.ChannelID = resp.Channel
	werr := writeRecord(a.stderr, a.log, rec)

	// Reported even when the record was lost: the message is waiting in Slack,
	// and the exit status below says only that it cannot be accounted for.
	fmt.Fprintf(a.stdout, "scheduled to %s", a.target.Input)
	if thread != "" {
		fmt.Fprintf(a.stdout, " (thread %s)", thread)
	}
	fmt.Fprintf(a.stdout, " id=%s channel=%s\n", termsafe.Text(resp.ID), termsafe.Text(rec.ChannelID))
	fmt.Fprintf(a.stdout, "post at : %s\n", when(rec.PostAt))
	return orAuditFailure(werr, 0)
}

// deliverUnschedule renders or sends a cancellation.
//
// The read gates it, as it gates an edit or a delete: the render shows the
// message that will not now be posted, and the record keeps its text and time,
// so the log says what was cancelled rather than that something was. sez holds
// only the write credential, so the read needs it even for a render.
func deliverUnschedule(a attempt, p preview, rec audit.Post, inv invocation) int {
	if !p.haveCred {
		fmt.Fprint(a.stderr, noCredentialFor(verbUnschedule))
		return abort(a.stderr, a.log, rec,
			"cannot read the scheduled message: listing it needs the write credential")
	}
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(a.stderr, a.log, rec, err.Error())
	}
	client := slackwrite.New(a.cred, slackhttp.New(timeout), a.baseURL)
	m, err := findScheduled(client, a.target, rec.ScheduledMessageID)
	if err != nil {
		return abort(a.stderr, a.log, rec, "cannot read the scheduled message: "+err.Error())
	}
	rec.Text, rec.PostAt = m.Text, m.PostAt
	if !inv.f.send {
		renderUnschedule(p, m)
		return dryRunRecorded(a.stderr, a.log, rec)
	}
	return unschedule(a, client, rec)
}

// findScheduled is the scheduled message id in target, or the error saying it
// is not there.
func findScheduled(client *slackwrite.Client, target resolve.Target, id string) (slackwrite.ScheduledMessage, error) {
	msgs, err := client.ScheduledMessages(context.Background(), target.ID)
	if err != nil {
		return slackwrite.ScheduledMessage{}, err
	}
	for _, m := range msgs {
		if m.ID == id {
			return m, nil
		}
	}
	return slackwrite.ScheduledMessage{}, fmt.Errorf(
		"%w %s in %s; it may have posted already, been cancelled, or be in another conversation",
		errNotScheduled, id, target.Input)
}

// renderUnschedule is the dry run for a cancellation.
func renderUnschedule(p preview, m slackwrite.ScheduledMessage) {
	renderHeader(p)
	fmt.Fprintf(p.stdout, "message : %s (scheduled)\n", m.ID)
	fmt.Fprintf(p.stdout, "post at : %s\n", when(m.PostAt))
	fmt.Fprintln(p.stdout, "--- scheduled message, cancelled under --send ---")
	fmt.Fprintln(p.stdout, termsafe.TextKeepingNewlines(m.Text))
	fmt.Fprintln(p.stdout, "----------------------------------------")
}

// unschedule cancels the scheduled message, with the accounting every write
// gets.
func unschedule(a attempt, client *slackwrite.Client, rec audit.Post) int {
	stop, err := begin(a.stderr, a.log, rec)
	if err != nil {
		return fail(a.stderr, "%v — refusing to send", err)
	}
	defer stop()

	err = client.DeleteScheduledMessage(context.Background(), a.target.ID, rec.ScheduledMessageID)
	if err != nil {
		return changeOutcome(a.stderr, a.log, rec, err)
	}
	rec.OK = audit.Succeeded
	werr := writeRecord(a.stderr, a.log, rec)
	fmt.Fprintf(a.stdout, "unscheduled %s from %s\n", rec.ScheduledMessageID, a.target.Input)
	return orAuditFailure(werr, 0)
}
