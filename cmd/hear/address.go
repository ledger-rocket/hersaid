package main

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Slack message link is https://<workspace>/archives/<conversation>/p<the
// timestamp without its point>, with thread_ts= added when it points inside a
// thread. It is how a message is passed between people, so it is an address
// here too.
//
// The host is not checked. The path shape is what identifies a link, and the
// host varies — a standard workspace, an enterprise grid, an export viewer —
// so a host allowlist would reject links that are genuinely Slack's while
// admitting nothing a bad host could do: the id and the timestamp are all that
// is taken from the URL, and both are validated below.
var (
	// The conversation id in a link is not always C/D/G — a shared channel can
	// be something else — so this is deliberately wider than resolve's own
	// pattern. Slack decides what exists; this only decides what is an address.
	linkConversation = regexp.MustCompile(`^[A-Z][A-Z0-9]{7,}$`)
	linkTS           = regexp.MustCompile(`^p([0-9]{7,})$`)
	messageTS        = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

// tsFraction is how many of a link's digits are the part after the point. The
// fraction is a sequence number rather than a fraction of a second, and Slack
// pads it to six digits in the link form: p1785708834862009 is
// 1785708834.862009.
const tsFraction = 6

type permalink struct {
	channelID string
	// ts is the message the link points at.
	ts string
	// threadTS is the thread it lives in, when the link says so.
	threadTS string
}

// parentTS is the thread to read: the one the link names, or the message
// itself. conversations.replies on a message with no replies answers with that
// message alone, so a link to an ordinary message reads as itself.
func (p permalink) parentTS() string {
	if p.threadTS != "" {
		return p.threadTS
	}
	return p.ts
}

// parsePermalink reports whether target is a URL — in which case it is an
// address of this kind and nothing else, so a malformed one is an error rather
// than a channel name to look up — and what it names.
func parsePermalink(target string) (permalink, bool, error) {
	if !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
		return permalink{}, false, nil
	}
	malformed := func() (permalink, bool, error) {
		return permalink{}, true, fmt.Errorf("not a Slack message link: %s", target)
	}
	u, err := url.Parse(target)
	if err != nil {
		return malformed()
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[len(parts)-3] != "archives" {
		return malformed()
	}
	channel, stamp := parts[len(parts)-2], parts[len(parts)-1]
	m := linkTS.FindStringSubmatch(stamp)
	if !linkConversation.MatchString(channel) || m == nil {
		return malformed()
	}
	digits := m[1]
	link := permalink{
		channelID: channel,
		ts:        digits[:len(digits)-tsFraction] + "." + digits[len(digits)-tsFraction:],
	}
	if t := u.Query().Get("thread_ts"); t != "" {
		if !messageTS.MatchString(t) {
			return permalink{}, true, fmt.Errorf("not a Slack thread timestamp: thread_ts=%s", t)
		}
		link.threadTS = t
	}
	return link, true, nil
}

// sinceUnit is a duration written the way a person says it. Go's own parser
// stops at hours, and "7d" is the length most reads are asked for.
var sinceUnit = regexp.MustCompile(`^([0-9]+)([smhdw])$`)

var sinceUnits = map[string]time.Duration{
	"s": time.Second,
	"m": time.Minute,
	"h": time.Hour,
	"d": 24 * time.Hour,
	"w": 7 * 24 * time.Hour,
}

// parseSince reads a window width. A non-positive one is refused: it asks for
// no messages at all, which is never what someone meant to type.
func parseSince(v string) (time.Duration, error) {
	if m := sinceUnit.FindStringSubmatch(v); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		unit := sinceUnits[m[2]]
		if err != nil || n > math.MaxInt64/int64(unit) {
			return 0, fmt.Errorf("--since %s: that is longer than any Slack workspace has existed", v)
		}
		if n == 0 {
			return 0, fmt.Errorf("--since %s: a window must be longer than nothing", v)
		}
		return time.Duration(n) * unit, nil
	}
	// Whatever Go itself accepts, so "1h30m" works without a second syntax.
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("--since %s: not a duration — try 30m, 2h, 7d or 1w", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--since %s: a window must be longer than nothing", v)
	}
	return d, nil
}
