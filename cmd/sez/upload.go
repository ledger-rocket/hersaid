package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/humansize"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackupload"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

var (
	errRefusing              = errors.New("refusing")
	errAreSameFile           = errors.New("are the same file")
	errNoSuchFile            = errors.New("no such file")
	errCannotRead            = errors.New("cannot be read")
	errDirectoryNotFile      = errors.New("is a directory, not a file")
	errNotRegularFile        = errors.New("is not a regular file")
	errEmptySlackRefusesZero = errors.New("is empty; Slack refuses a zero-length upload")
	errBytesSlackAccepts     = errors.New("bytes Slack accepts")
)

// uploadHosts is where a file's bytes may be sent. It is a variable so a test
// can point a transfer at a local address, exactly as resolveWrite and
// readMessage are variables; the shipped command never reassigns it.
var uploadHosts = slackupload.SlackHosts

// openFiles turns the --file arguments into what an upload needs, opening each
// one and keeping the descriptor.
//
// The descriptor is held because the length is declared on the first leg and
// the bytes are sent on the second: between a stat and a later open, a symlink
// can be retargeted or a path replaced, and then the file the render described
// is not the file that is sent. The caller closes them.
func openFiles(paths []string) ([]slackupload.Local, error) {
	if len(paths) > slackupload.MaxFiles {
		return nil, fmt.Errorf("%w %d files; at most %d go on one message",
			errRefusing, len(paths), slackupload.MaxFiles)
	}
	out := make([]slackupload.Local, 0, len(paths))
	for _, p := range paths {
		l, err := openFile(p)
		if err != nil {
			closeFiles(out)
			return nil, err
		}
		for _, seen := range out {
			same, serr := sameFile(seen.File, l.File)
			if serr == nil && same {
				l.File.Close()
				closeFiles(out)
				return nil, fmt.Errorf("%s and %s %w",
					termsafe.Text(seen.Path), termsafe.Text(l.Path), errAreSameFile)
			}
		}
		out = append(out, l)
	}
	return out, nil
}

func openFile(p string) (slackupload.Local, error) {
	shown := termsafe.Text(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return slackupload.Local{}, localPathError{shown: shown, err: err}
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return slackupload.Local{}, fmt.Errorf("%s: %w", shown, errNoSuchFile)
		}
		return slackupload.Local{}, fmt.Errorf("%s: %w", shown, errCannotRead)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return slackupload.Local{}, fmt.Errorf("%s: %w", shown, errCannotRead)
	}
	switch {
	case info.IsDir():
		f.Close()
		return slackupload.Local{}, fmt.Errorf("%s %w", shown, errDirectoryNotFile)
	case !info.Mode().IsRegular():
		f.Close()
		return slackupload.Local{}, fmt.Errorf("%s %w", shown, errNotRegularFile)
	case info.Size() == 0:
		f.Close()
		return slackupload.Local{}, fmt.Errorf("%s %w", shown, errEmptySlackRefusesZero)
	case info.Size() > slackupload.MaxSize:
		f.Close()
		// The cap is decimal and humansize is binary, so rendering it would
		// print "953.7 MB" for a limit documented and reasoned about as
		// 1,000,000,000 bytes.
		return slackupload.Local{}, fmt.Errorf("%s is too large: %s exceeds the %d %w",
			shown, humansize.Bytes(info.Size()), slackupload.MaxSize, errBytesSlackAccepts)
	}
	return slackupload.Local{
		Path:     abs,
		Name:     filepath.Base(abs),
		Size:     info.Size(),
		Mimetype: mimetypeOf(abs, f),
		File:     f,
	}, nil
}

func sameFile(a, b *os.File) (bool, error) {
	ai, err := a.Stat()
	if err != nil {
		return false, err
	}
	bi, err := b.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

func closeFiles(files []slackupload.Local) {
	for _, f := range files {
		if f.File != nil {
			f.File.Close()
		}
	}
}

// mimetypeOf is what the transfer declares as its Content-Type. Slack infers a
// file's type from the name it is given, so this is a statement about the
// request rather than a field Slack is asked to honour.
func mimetypeOf(path string, f *os.File) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		base, _, err := mime.ParseMediaType(t)
		if err == nil {
			return base
		}
		return t
	}
	var head [512]byte
	n, _ := io.ReadFull(f, head[:])
	_, err := f.Seek(0, io.SeekStart)
	if err != nil {
		return "application/octet-stream"
	}
	if n == 0 {
		return "application/octet-stream"
	}
	return strings.TrimSpace(strings.SplitN(http.DetectContentType(head[:n]), ";", 2)[0])
}

// auditFiles is what the record carries for each attachment. id fills in as
// each file stages, so the intent line — written before anything leaves — still
// says what was about to go out.
func auditFiles(files []slackupload.Local) []audit.File {
	out := make([]audit.File, 0, len(files))
	for _, f := range files {
		out = append(out, audit.File{
			Path: f.Path, Name: f.Name, Size: f.Size, Mimetype: f.Mimetype,
		})
	}
	return out
}

// phase is how far an upload has got, which is what decides the record a signal
// leaves behind.
type phase int

const (
	// staging covers everything up to the last byte of the last transfer.
	// Nothing is visible to anyone, so an interruption here is a failure and
	// not a doubt.
	staging phase = iota
	// completing begins when the sharing request is on the wire. From here the
	// effect of an interruption is genuinely unknown.
	completing
	// decided begins the moment Slack's answer has been read and classified.
	// The permalink lookup that follows is a whole round trip, and a signal
	// during it must record what Slack said rather than an unknown.
	decided
)

// progress is the one piece of state a signal handler and the normal path both
// touch.
//
// The protocol: both take the mutex, and written decides which of them appends
// the outcome. Whichever arrives first writes; the other returns. Only one
// outcome line can therefore exist for one attempt.
//
// The handler never closes the log. audit.Log.Post is a single unbuffered write
// followed by Sync, so a record is durable as soon as it returns, and closing
// underneath a writer holding the mutex would be the one way to lose it.
type progress struct {
	mu      sync.Mutex
	phase   phase
	staged  []string
	outcome audit.Post
	settled bool
	written bool
}

func (p *progress) stagedFile(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.staged = append(p.staged, id)
}

func (p *progress) enter(ph phase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = ph
}

// settle records the outcome Slack actually gave, and moves to decided. From
// here a signal reports that outcome rather than an unknown.
func (p *progress) settle(rec audit.Post) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase, p.outcome, p.settled = decided, rec, true
}

// writeOutcome appends rec unless an outcome has already been written, holding
// the mutex across the append.
//
// The write is inside the critical section, not merely the decision to make it.
// audit.OnAbort runs the handler and then exits the process, so a handler that
// found the flag set and returned would kill the process while the normal path
// was still writing — losing the only outcome for an attempt whose intent line
// is already on disk. Blocking on the mutex is what makes the loser wait until
// the winner's record is durable.
func (p *progress) writeOutcome(stderr io.Writer, log *audit.Log, rec audit.Post) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written {
		return nil
	}
	p.written = true
	return writeRecord(stderr, log, rec)
}

// writeOnSignal chooses the record an interruption implies and appends it, both
// under one hold of the mutex. Choosing and writing separately would let the
// sharing leg settle in between, so the handler would write a stale unknown for
// an outcome Slack had already given.
func (p *progress) writeOnSignal(stderr io.Writer, log *audit.Log, base audit.Post, sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written {
		return nil
	}
	p.written = true
	return writeRecord(stderr, log, p.recordFor(base, sig))
}

// recordFor is the record an interruption leaves, given how far the attempt
// got. The caller holds the mutex.
func (p *progress) recordFor(base audit.Post, sig os.Signal) audit.Post {
	rec := base
	rec.Files = withIDs(rec.Files, p.staged)
	switch {
	case p.settled:
		// Slack answered. The permalink may be missing; the outcome is not.
		return p.outcome
	case p.phase == completing:
		rec.Phase, rec.OK = audit.Abort, audit.Unknown
		rec.Error = fmt.Sprintf(
			"interrupted by %s while sharing; delivery unknown, check Slack before retrying", sig)
	default:
		rec.Phase, rec.OK = audit.Abort, audit.Failed
		rec.Error = fmt.Sprintf("interrupted by %s before anything was shared", sig)
	}
	return rec
}

// withIDs stamps the ids of the files that staged onto the record's file list.
func withIDs(files []audit.File, ids []string) []audit.File {
	out := make([]audit.File, len(files))
	copy(out, files)
	for i := range out {
		if i < len(ids) {
			out[i].ID = ids[i]
		}
	}
	return out
}

// renderUpload is the dry run for an upload. Paths are absolute because a
// basename cannot distinguish the report.pdf in this directory from the one in
// the last, and a path is not something a reader can proofread the way they
// proofread a sentence. Every one of them goes through termsafe: a filename
// arrives through a shell glob and may hold an ESC or a newline, and the render
// is what a person reads in order to authorise a send.
func renderUpload(stdout io.Writer, target resolve.Target, thread, text string,
	files []slackupload.Local, haveCred bool,
) {
	fmt.Fprintln(stdout, "--- dry run: nothing sent ---")
	if target.ID != target.Input {
		fmt.Fprintf(stdout, "channel : %s -> %s\n", target.Input, target.ID)
	} else {
		fmt.Fprintf(stdout, "channel : %s\n", target.Input)
	}
	if thread != "" {
		fmt.Fprintf(stdout, "thread  : %s\n", thread)
	}
	for i, f := range files {
		label := "files   :"
		if i > 0 {
			label = "         "
		}
		fmt.Fprintf(stdout, "%s %s\n", label, termsafe.Text(f.Path))
		fmt.Fprintf(stdout, "            %s  %s\n", humansize.Bytes(f.Size), f.Mimetype)
	}
	if !haveCred {
		fmt.Fprintln(stdout, "token   : absent (posting currently impossible)")
	}
	fmt.Fprintln(stdout, "--- message as Slack will receive it ---")
	if text == "" {
		fmt.Fprintln(stdout, "(no comment; the files are the message)")
	} else {
		fmt.Fprintln(stdout, text)
	}
	fmt.Fprintln(stdout, "----------------------------------------")
}

// upload is the send path for a message carrying files. It mirrors post — the
// intent record before anything leaves, one attempt, a classified outcome —
// with the difference that most of what can fail here provably posted nothing.
func upload(stdout, stderr io.Writer, log *audit.Log, rec audit.Post, baseURL string,
	c cred.Write, target resolve.Target, thread, text string, files []slackupload.Local,
) int {
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(stderr, log, rec, err.Error())
	}

	// Intent before the request, as a send does: this is the record that
	// survives SIGKILL, and its ok is null because nothing is known yet.
	intent := rec
	intent.Phase, intent.OK = audit.Intent, audit.Unknown
	err = log.Post(intent)
	if err != nil {
		return fail(stderr, "%v — refusing to send", err)
	}

	prog := &progress{}
	pending := rec
	stop := audit.OnAbort(func(sig os.Signal) {
		_ = prog.writeOnSignal(stderr, log, pending, sig)
	})
	defer stop()

	api := slackwrite.New(c, slackhttp.New(timeout), baseURL)
	up := slackupload.New(api, uploadHosts, timeout)
	ctx := context.Background()

	staged, serr := up.Stage(ctx, files, func(s slackupload.Staged) { prog.stagedFile(s.ID) })
	rec.Files = withIDs(rec.Files, idsOf(staged))
	rec.Phase = audit.Outcome

	if serr != nil {
		// Recorded failed, never unknown: until the sharing leg nothing is
		// visible, so there is no doubt to record and no retry to forbid.
		rec.OK, rec.Error = audit.Failed, serr.Error()
		werr := prog.writeOutcome(stderr, log, rec)
		code := fail(stderr, "%s  (logged)", termsafe.Text(serr.Error()))
		if n := len(staged); n > 0 {
			// Named, but not as something to go and deal with: Slack discards
			// an upload whose completion never comes, and asking files.info
			// for one of these ids is answered file_deleted. The ids are here
			// because they say how far the send got, and they are in the record
			// for the same reason.
			fmt.Fprintf(stderr,
				"sez: note — %s reached Slack before this failed (%s), but an upload that is never shared is discarded. Nothing was posted and there is nothing to clean up.\n",
				plural(n, "file"), strings.Join(idsOf(staged), ", "))
		}
		if slackwrite.Code(serr) == slackwrite.CodeMissingScope {
			fmt.Fprint(stderr, missingScopeFor(serr))
		}
		return orAuditFailure(werr, code)
	}

	shared, sherr := up.Share(ctx, slackupload.ShareRequest{
		Files:     staged,
		ChannelID: target.ID,
		ThreadTS:  thread,
		Comment:   text,
	}, slackupload.ShareBoundary{
		Issued: func() { prog.enter(completing) },
		// Settled the moment Slack's answer is classified, which is before the
		// permalink lookup: that is a whole round trip, and a signal during it
		// must record what Slack said rather than an unknown.
		Decided: func(sh slackupload.Shared, err error) { prog.settle(outcomeOf(rec, sh, err)) },
	})

	rec = outcomeOf(rec, shared, sherr)
	if sherr != nil {
		werr := prog.writeOutcome(stderr, log, rec)
		if rec.OK == audit.Unknown {
			return orAuditFailure(werr,
				fail(stderr, "%s (delivery UNKNOWN — logged)", termsafe.Text(sherr.Error())))
		}
		code := fail(stderr, "%s  (logged)", termsafe.Text(sherr.Error()))
		if slackwrite.Code(sherr) == slackwrite.CodeMissingScope {
			fmt.Fprint(stderr, missingScopeFor(sherr))
		}
		return orAuditFailure(werr, code)
	}

	// Best effort, as a send's is: the message is already delivered, so a
	// permalink failure must not turn a success into an error.
	if rec.TS != "" {
		link, perr := api.Permalink(ctx, target.ID, rec.TS)
		if perr == nil {
			rec.Permalink = link
		}
	}
	werr := prog.writeOutcome(stderr, log, rec)

	fmt.Fprintf(stdout, "uploaded to %s", target.Input)
	if thread != "" {
		fmt.Fprintf(stdout, " (thread %s)", thread)
	}
	if rec.TS != "" {
		fmt.Fprintf(stdout, " ts=%s", rec.TS)
	}
	fmt.Fprintln(stdout)
	for i, f := range files {
		id := ""
		if i < len(shared.IDs) {
			id = shared.IDs[i]
		}
		fmt.Fprintf(stdout, "  %-24s %s\n", termsafe.Text(f.Name), id)
	}
	if rec.Permalink != "" {
		fmt.Fprintln(stdout, rec.Permalink)
	}
	return orAuditFailure(werr, 0)
}

// outcomeOf is the record an answered sharing leg implies. It is built in one
// place so that the copy the signal handler holds and the copy the normal path
// writes cannot disagree.
func outcomeOf(rec audit.Post, shared slackupload.Shared, err error) audit.Post {
	rec.Phase = audit.Outcome
	if err != nil {
		switch slackupload.ClassifyShare(err) {
		case slackwrite.Rejected:
			rec.OK, rec.Error = audit.Failed, err.Error()
		default:
			rec.OK = audit.Unknown
			rec.Error = err.Error() + " — delivery unknown, check Slack before retrying"
		}
		return rec
	}
	rec.OK, rec.TS = audit.Succeeded, shared.TS
	rec.Files = withIDs(rec.Files, shared.IDs)
	return rec
}

func idsOf(staged []slackupload.Staged) []string {
	out := make([]string, 0, len(staged))
	for _, s := range staged {
		out = append(out, s.ID)
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s was", word)
	}
	return fmt.Sprintf("%d %ss were", n, word)
}

// writtenAlready reports whether an outcome is already on disk. It exists for a
// test that needs to tell a winner from a loser; the protocol itself never asks.
func (p *progress) writtenAlready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written
}

// recordForLocked is recordFor with the mutex taken. The production paths build
// the record inside a hold they already own; this is for a test that exercises
// the read side on its own.
func (p *progress) recordForLocked(base audit.Post, sig os.Signal) audit.Post {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recordFor(base, sig)
}

// localPathError is a path that could not be made absolute, as the user wrote
// it. It does not wrap err: the message carries err's text only.
type localPathError struct {
	shown string
	err   error
}

func (e localPathError) Error() string { return e.shown + ": " + e.err.Error() }
