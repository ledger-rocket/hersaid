package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackdocs"
	"github.com/ledger-rocket/hersaid/internal/slackfiles"
	"github.com/ledger-rocket/hersaid/internal/slackread"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

var (
	errNotCanvasLink        = errors.New("not a canvas link")
	errNotSlackLink         = errors.New("not a Slack link")
	errHasNoCanvas          = errors.New("has no canvas")
	errCanvasesNameOneYou   = errors.New("canvases; name the one you want")
	errNotCanvasChannelLink = errors.New("not a canvas or channel link")
)

// maxCanvasSize bounds what is read into memory. A canvas is a document someone
// wrote, not a video: the largest plausible one is far under this, and the
// bound is what stops a mistyped id from being answered with a gigabyte.
const maxCanvasSize = 8 << 20

// fileID matches Slack's file ids.
var fileID = regexp.MustCompile(`^F[A-Z0-9]{6,}$`)

// canvasLink is a canvas's own permalink: /docs/<team id>/<file id>.
var canvasLink = regexp.MustCompile(`^/docs/[A-Z0-9]+/(F[A-Z0-9]{6,})/?$`)

// channelLink is a link to a conversation rather than to a message in one:
// /archives/<channel id>, with no timestamp after it. It is what somebody
// reaching for a channel's canvas copies out of the address bar.
var channelLink = regexp.MustCompile(`^/archives/([CDG][A-Z0-9]{6,})/?$`)

// canvasTarget extracts the file id from a canvas's own address, or returns ""
// when the argument names something else — a channel, most likely, which the
// caller resolves instead.
//
// A canvas has a permalink of its own and no message behind it, so neither a
// timestamp nor a message link is any use here, which is why this verb exists
// beside files rather than inside it.
func canvasTarget(arg string) (string, error) {
	if fileID.MatchString(arg) {
		return arg, nil
	}
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		u, err := url.Parse(arg)
		if err != nil {
			return "", fmt.Errorf("%w: %s", errNotCanvasLink, termsafe.Text(arg))
		}
		// The host is checked as well as the path. Nothing downstream trusts it
		// — the id is looked up on Slack's own API and the bytes come from a
		// host-checked URL in Slack's reply — but a link to somewhere else is
		// not a canvas link, and accepting one invites a reader to believe a
		// pasted URL was validated when only its shape was.
		if !isSlackHost(u.Hostname()) {
			return "", fmt.Errorf("%w: %s", errNotSlackLink, termsafe.Text(arg))
		}
		if m := canvasLink.FindStringSubmatch(u.Path); m != nil {
			return m[1], nil
		}
		// A Slack link that is not a canvas link is an address: a channel link
		// is what somebody reaching for a channel canvas pastes.
		return "", nil
	}
	// Anything else is an address for the caller to resolve.
	return "", nil
}

// isSlackHost reports a workspace or Slack host, the only places a canvas link
// comes from.
func isSlackHost(host string) bool {
	h := strings.ToLower(host)
	return h == "slack.com" || strings.HasSuffix(h, ".slack.com")
}

// canvas prints a canvas as Markdown.
//
// It is read rather than downloaded: a canvas is a document to look at, and its
// HTML is not what anybody wants on a terminal or in an agent's context. --html
// gives the bytes Slack served, for a caller that would rather render them
// itself.
func (h *hear) canvas(args []string) int {
	if len(args) == 0 {
		return h.usage("canvas needs a canvas link or file id")
	}
	if len(args) > 1 {
		return h.usage("canvas takes one canvas link or file id")
	}
	ctx := context.Background()
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}
	id, err := canvasTarget(args[0])
	if err != nil {
		return h.fail(err)
	}
	if id == "" {
		// Not a canvas's own address, so it names a conversation whose canvas
		// is wanted. That is how a channel canvas is reached: it hangs off the
		// conversation rather than off any message in it.
		id, err = h.channelCanvas(ctx, c, args[0])
		if err != nil {
			return h.fail(err)
		}
	}
	info, err := c.FileInfo(ctx, id)
	if err != nil {
		return h.fail(err)
	}
	if reason := notACanvas(info); reason != "" {
		return h.fail(notCanvasError{title: canvasTitle(info), reason: reason})
	}

	fc, err := h.src.downloader()
	if err != nil {
		return h.fail(err)
	}
	body, err := fc.Read(ctx, info, maxCanvasSize)
	if err != nil {
		return h.fail(fmt.Errorf("%s: %w", canvasTitle(info), err))
	}

	rendered, err := h.canvasBody(body)
	if err != nil {
		return h.fail(fmt.Errorf("%s: %w", canvasTitle(info), err))
	}
	if h.asJSON {
		return h.emit(render.Canvas{
			Kind:  render.KindCanvas,
			ID:    info.ID,
			Title: canvasTitle(info),
			Body:  rendered,
		})
	}
	fmt.Fprint(h.stdout, rendered)
	return 0
}

// canvasBody is the rendering the caller asked for. The Markdown path is the
// default because it is the one a person or an agent can read.
//
// Neither path returns the bytes as they arrived. HTML has no legitimate use
// for a control character, and this output goes to a terminal — so --html means
// "the markup Slack sent" rather than "whatever bytes Slack sent", and a canvas
// cannot drive a terminal through the flag that skips the renderer.
func (h *hear) canvasBody(body []byte) (string, error) {
	if h.canvasHTML {
		return termsafe.TextKeepingNewlines(string(body)), nil
	}
	md, err := slackdocs.Markdown(body)
	if err != nil {
		return "", err
	}
	return md, nil
}

// notCanvasError says why a file is not a canvas, after the file's title.
type notCanvasError struct{ title, reason string }

func (e notCanvasError) Error() string { return e.title + ": " + e.reason }

// canvasTitle is what to call a canvas. Slack names every one of them "canvas"
// and puts the document's real name in the title, so FileLabel's name-first
// order — right for an attachment, whose name is its filename — is backwards
// here.
func canvasTitle(f slackread.File) string {
	if t := termsafe.Text(strings.TrimSpace(f.Title)); t != "" {
		return t
	}
	return render.FileLabel(f)
}

// notACanvas says why a file is not something this verb can render, or "".
func notACanvas(f slackread.File) string {
	switch {
	case f.Mode == "tombstone":
		return "deleted"
	case f.Mode == "external" || f.IsExternal:
		return "an external file, not a canvas"
	case f.URLPrivateDownload == "":
		return "no download url"
	case !strings.HasPrefix(strings.ToLower(f.Mimetype), slackfiles.CanvasMimetype):
		return fmt.Sprintf("a %s, not a canvas — use `hear files` for an attachment",
			termsafe.Text(f.Mimetype))
	}
	return ""
}

// channelCanvas is the canvas attached to a conversation.
//
// A channel may carry more than one — a canvas and a notes document are
// different things — and choosing for somebody would be choosing which document
// they meant. So several are listed rather than guessed at, by the id that
// names each one exactly.
func (h *hear) channelCanvas(ctx context.Context, c *slackread.Client, target string) (string, error) {
	channel, err := channelOf(ctx, h, target)
	if err != nil {
		return "", err
	}
	ids, err := c.Canvases(ctx, channel)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%s %w", termsafe.Text(target), errHasNoCanvas)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%s has %d %w: %s",
		termsafe.Text(target), len(ids), errCanvasesNameOneYou, strings.Join(ids, " "))
}

// channelOf turns what was given into a conversation id.
//
// A link to a conversation is read straight off the path: it already carries
// the id, and the cache may not know a channel somebody is looking at in a
// browser. Anything else resolves the ordinary way.
func channelOf(ctx context.Context, h *hear, target string) (string, error) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil || !isSlackHost(u.Hostname()) {
			return "", fmt.Errorf("%w: %s", errNotSlackLink, termsafe.Text(target))
		}
		if m := channelLink.FindStringSubmatch(u.Path); m != nil {
			return m[1], nil
		}
		return "", fmt.Errorf("%w: %s", errNotCanvasChannelLink, termsafe.Text(target))
	}
	t, err := h.address(ctx, target)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}
