package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/render"
	"github.com/ledger-rocket/hersaid/internal/slackfiles"
)

var errMessage = errors.New("message")

// defaultMaxSize bounds one file. It is large enough for the documents and
// screenshots a message usually carries and small enough that a video posted to
// a channel does not fill a disk before anyone notices; --max-size raises it
// for the case where the video is the point.
const defaultMaxSize = 100 << 20

func (h *hear) files(args []string) int {
	if len(args) == 0 {
		return h.usage("files needs an address and a ts")
	}
	target, rest := args[0], args[1:]

	if link, isLink, err := parsePermalink(target); isLink {
		if err != nil {
			return h.fail(err)
		}
		if len(rest) > 0 {
			return h.usage("the permalink already names the timestamp")
		}
		// link.ts, not the thread it belongs to: a link to a reply names that
		// reply, and its files are the ones asked for.
		return h.fetchFiles(render.ChannelRef{ID: link.channelID, Input: target}, link.ts)
	}
	if len(rest) != 1 {
		return h.usage("files needs an address and a ts")
	}
	t, err := h.address(context.Background(), target)
	if err != nil {
		return h.fail(err)
	}
	return h.fetchFiles(channelRef(t), rest[0])
}

// fetchFiles downloads every file on one message.
//
// The exit status is failure when any file was skipped or failed, so a caller
// that asked for a message's attachments and got fewer can tell. The files that
// did arrive are still written, and still reported.
func (h *hear) fetchFiles(ch render.ChannelRef, ts string) int {
	ctx := context.Background()
	c, err := h.src.get()
	if err != nil {
		return h.fail(err)
	}
	msg, err := c.Message(ctx, ch.ID, ts)
	if err != nil {
		return h.fail(err)
	}
	if len(msg.Files) == 0 {
		return h.fail(fmt.Errorf("%w %s carries no files", errMessage, ts))
	}

	fc, err := h.src.downloader()
	if err != nil {
		return h.fail(err)
	}
	dir := filepath.Join(h.out, messageDir(ch, ts))
	results := fc.Fetch(ctx, msg.Files, h.out, messageDir(ch, ts), h.limit())

	out, incomplete := fileResults(results)

	if h.asJSON {
		code := h.emit(render.Files{
			Kind: render.KindFiles, Channel: ch, TS: ts, Dir: writtenDir(dir, results), Files: out,
		})
		if code == cli.ExitOK && incomplete {
			return cli.ExitFailure
		}
		return code
	}
	err = render.WriteFiles(h.stdout, out)
	if err != nil {
		return h.fail(err)
	}
	if incomplete {
		return cli.ExitFailure
	}
	return cli.ExitOK
}

// fileResults is what fetchFiles reports for each file, and whether any of
// them was not written.
func fileResults(results []slackfiles.Result) ([]render.FileResult, bool) {
	out := make([]render.FileResult, 0, len(results))
	incomplete := false
	for _, r := range results {
		if r.Status != slackfiles.Written {
			incomplete = true
		}
		out = append(out, render.FileResult{
			ID:     r.File.ID,
			Name:   render.FileLabel(r.File),
			Size:   r.File.Size,
			Status: string(r.Status),
			Path:   r.Path,
			Bytes:  r.Bytes,
			Reason: r.Reason,
		})
	}
	return out, incomplete
}

// limit is the per-file ceiling in bytes, --max-size or the default.
func (h *hear) limit() int64 {
	if h.maxSize > 0 {
		return h.maxSize
	}
	return defaultMaxSize
}

// writtenDir names the directory only when something was actually put in it, so
// a consumer is not pointed at a path that was never created.
func writtenDir(dir string, results []slackfiles.Result) string {
	for _, r := range results {
		if r.Status == slackfiles.Written {
			return dir
		}
	}
	return ""
}

// messageDir is the directory one message's files are written to: the
// conversation and the timestamp, which together name the message and cannot
// collide with another one.
//
// The conversation is its name when the cache knows it and its id when it does
// not — a DM has no name at all — and either is reduced to characters that can
// only be a single path element.
func messageDir(ch render.ChannelRef, ts string) string {
	label := ch.Name
	if label == "" {
		label = ch.ID
	}
	return pathSafe(label) + "-" + pathSafe(ts)
}

// pathSafe keeps everything that plainly cannot name somewhere else and
// replaces the rest with "-", so the result is one path element whatever it was
// built from.
func pathSafe(s string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '-'
		}
	}, s)
	// A component of dots alone is the parent directory or the current one.
	if strings.Trim(safe, ".") == "" {
		return "-"
	}
	return safe
}
