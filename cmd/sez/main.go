// Command sez renders a Slack message, and delivers it only when told to.
//
// The ordering in run is the load-bearing part: the credential gate, the
// render-only default, the write allowlist, an intent record before the
// request, one attempt, and an outcome that distinguishes "Slack refused it"
// from "it may have gone".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/allowlist"
	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/mrkdwn"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
	"github.com/ledger-rocket/hersaid/internal/termsafe"
)

var (
	errHear               = errors.New("hear")
	errHearNoAnswerWithin = errors.New("hear: no answer within")
	errThisHearTooOld     = errors.New("this hear is too old to say what the message")
	errHearReportsMessage = errors.New("hear reports the message")
	errMessage            = errors.New("the message")
	errHearNoMessageTs    = errors.New("hear: no message with ts")
)

const name = "sez"

const usage = `usage: sez [--send] [--thread TS] [--md] [--file PATH]... <#chan|@person|ID> [text]
       sez [--send] [--md] --edit <#chan|@person|ID> <ts> <new text>
       sez [--send] --delete <#chan|@person|ID> <ts>

  Renders the message WITHOUT sending unless --send is given. Read the rendered
  text first; deliver it second.

  Text may be given as an argument, or piped on stdin (use "-" or omit it).
  With --file the text becomes the message carried alongside the files; omitting
  it means no comment, and stdin is not read. "-" still reads it.
  --edit and --delete rewrite or destroy what people have already read, so the
  render shows the current text first — read from the hear binary — and the
  change beside it. They are mutually exclusive with each other and with
  --thread, and --send is the only thing that acts.

  Options:
        --send        actually deliver it (default is render-only)
    -n, --dry-run     render only; the default, accepted for explicitness
        --md          convert Markdown to Slack mrkdwn before sending
        --thread TS   reply inside the thread with this parent ts
        --file PATH   attach a file; repeatable, and the order is kept
        --edit        rewrite the message at the given ts; the render shows both
        --delete      delete the message at the given ts; the render shows what goes
        --            stop reading options; what follows is the target and text
    -h, --help        print this help and exit 0
        --version     print the version and exit 0

  Environment:
    SLACK_WRITE_TOKEN       write credential; set but empty means "absent"
    HERSAID_WRITE_CHANNELS  comma-separated ids and/or #names that may be
                            written to; unset means unrestricted
    HERSAID_TIMEOUT         request timeout in whole seconds (default 20)

  Exit: 0 sent or rendered, 1 refused or failed, 2 usage,
        3 the attempt could not be recorded in the audit log.

  Audit log: ~/.local/state/hersaid/posts.jsonl
`

const noCredential = `sez: no write credential found (keychain service: hersaid-write).
  Posting is disabled by design until one exists:
    security add-generic-password -a "$USER" -s hersaid-write -w
  Slack issues one token per app, so this needs a SECOND Slack app carrying
  chat:write — the read app's token cannot post.
  Rendering needs no credential — omit --send.
`

// missingScopeFor names the scope the failing endpoint needs. The scope is read
// from a table in slackwrite rather than from Slack's reply, so nothing a
// response says can steer instructions about what to grant.
func missingScopeFor(err error) string {
	scope := slackwrite.Scope(err)
	if scope == "" {
		scope = "the scope this endpoint needs"
	}
	return fmt.Sprintf(`  The write app needs %s. Add it under User Token Scopes, reinstall
  the app, then enrol the new token:
    hersaid-setup --with-write
`, scope)
}

// Defence in depth: the form encoder already escapes every field.
var (
	channelRe = regexp.MustCompile(`^#?[A-Za-z0-9._-]+$`)
	threadRe  = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
)

// resolveWrite is a variable so a test can observe whether the credential was
// consulted at all. Asking for help or for a version must never reach a
// keychain: on macOS that is a modal unlock prompt, and on Linux a D-Bus call
// that hangs where there is no Secret Service.
var resolveWrite = cred.ResolveWrite

// readMessage is a variable so a test can substitute it, exactly as resolveWrite
// is. sez holds the write credential and the write app has no history scopes,
// so it cannot read a message itself — it runs the hear binary, which holds its
// own credential. readMessage returns the text of the message at ts, or an
// error if hear is absent, fails, or does not list the message.
var readMessage = viaHear

// current is a message as it stands, read before anything changes it.
type current struct {
	// Text is the whole of what the message says.
	Text string
	// Files are the attached files as Slack holds them, name and id both. They
	// are kept raw: termsafe is for display, and the record is supposed to say
	// what Slack has rather than what was printable. The id is what reconciles
	// a delete against files.info afterwards.
	Files []audit.File
}

// chooseVerb picks what the invocation does to a message, or returns the usage
// refusal for a combination of flags that names no one thing.
func chooseVerb(f flags, thread string) (verb, string) {
	refusal := flagConflict(f, thread)
	if refusal != "" {
		return verbSend, refusal
	}
	if f.edit {
		return verbEdit, ""
	}
	if f.del {
		return verbDelete, ""
	}
	return verbSend, ""
}

// flagConflict is the usage refusal for --edit or --delete combined with a
// flag that addresses, or adds, something else; empty when there is none.
// Every conflict involves --edit or --delete, so without either there is none.
func flagConflict(f flags, thread string) string {
	switch {
	case !f.edit && !f.del:
		return ""
	// --edit and --delete name one message, as --thread does; they exclude each
	// other and the thread parent, all three addressing a different message.
	case f.edit && f.del:
		return "--edit and --delete name one message; give one"
	case thread != "":
		return "--edit and --delete name a message; --thread excludes them"
	// Refused rather than ignored, as every other meaningless combination here
	// is: a flag accepted and silently dropped teaches that it did something.
	case f.del && f.md:
		return "--delete sends no text, so --md has nothing to convert"
	// Slack offers no way to attach a file to a message that already exists, so
	// --file substitutes the send endpoint rather than adding to it.
	case len(f.files) > 0:
		return "--file attaches files to a new message; it cannot combine with --edit or --delete"
	}
	return ""
}

// messageText is the text the invocation sends: none for a delete or a bare
// upload, otherwise the arguments or stdin, with --md applied. done reports
// that the invocation ends here with code.
func messageText(stderr io.Writer, f flags, v verb, rest []string, stdin io.Reader) (string, int, bool) {
	if v == verbDelete {
		if len(rest) > 0 {
			return "", cli.Usage(stderr, name, usage, "--delete takes a target and a ts and nothing else"), true
		}
		return "", 0, false
	}
	if len(f.files) > 0 && len(rest) == 0 {
		// The files are the message. Reading stdin here would block on a
		// terminal for input nobody is going to type.
		return "", 0, false
	}
	text, err := readText(rest, stdin)
	if err != nil {
		return "", fail(stderr, "%v", err), true
	}
	// A comment asked for and answered with blank space is a mistake.
	// Requesting no comment is what omitting the argument does.
	if strings.TrimSpace(text) == "" {
		return "", fail(stderr, "refusing to send an empty message"), true
	}
	// Converted here, so that everything downstream — the render, the audit
	// record and the wire — sees the one text Slack will be given. Rendering
	// what was typed while sending something else is the thing --send exists to
	// prevent.
	//
	// Trailing newlines go too: an editor's final newline is not a blank line
	// somebody meant to send.
	if f.md {
		text = trimTrailingNewlines(mrkdwn.Convert(text))
	}
	return text, 0, false
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, slackwrite.SlackAPI))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, baseURL string) int {
	// First, and with no side effects of its own: --help and --version are
	// answered here, before stdin is read, before the audit log is created and
	// before any credential is resolved.
	f, done, code := parseFlags(args, stdout, stderr)
	if done {
		return code
	}
	send, thread, rest := f.send, f.thread, f.rest
	if len(rest) == 0 {
		return cli.Usage(stderr, name, usage, "")
	}
	verb, refusal := chooseVerb(f, thread)
	if refusal != "" {
		return cli.Usage(stderr, name, usage, refusal)
	}

	channel := rest[0]
	rest = rest[1:]
	if strings.HasPrefix(channel, "-") {
		return fail(stderr, "refusing a channel that looks like a flag: %s", channel)
	}

	var ts string
	if verb != verbSend {
		if len(rest) == 0 {
			return cli.Usage(stderr, name, usage, "--edit and --delete need a message ts")
		}
		ts = rest[0]
		rest = rest[1:]
	}

	text, code, done := messageText(stderr, f, verb, rest, stdin)
	if done {
		return code
	}

	// Resolved once: a second lookup would mean a second keyring access, which on
	// a locked keychain is a second prompt for the same invocation.
	wcred, credErr := resolveWrite()

	// The log opens once the invocation is a real attempt, so `sez --help`
	// neither creates the file nor writes a record.
	log, err := audit.Open(audit.DefaultPath())
	if err != nil {
		return fail(stderr, "%v", err)
	}
	defer log.Close()
	for _, w := range log.Warnings() {
		fmt.Fprintf(stderr, "sez: warning — %s\n", w)
	}

	action := verbAction(verb)
	if len(f.files) > 0 {
		action = audit.Upload
	}
	rec := audit.Post{
		Channel: channel, ThreadTS: thread, DryRun: !send, Text: text,
		Action: action,
	}

	// The gate: still before any client exists, so a refusal cannot issue a
	// request — but after the log, because a refused send is an attempt and the
	// log holds every attempt. Reading the log to ask "did anything try to go
	// out" must not answer no here.
	if send && credErr != nil {
		fmt.Fprint(stderr, noCredential)
		rec.Phase, rec.OK, rec.Error = audit.Abort, audit.Failed, "no write credential"
		return orAuditFailure(writeRecord(stderr, log, rec), cli.ExitFailure)
	}

	can := resolve.ForPost
	if verb != verbSend {
		can = resolve.ForEdit
	}
	// completeUploadExternal takes a conversation id, resolving no #name and
	// opening no DM. Resolving as a send does would stage every byte of every
	// file and then fail on the last leg.
	if len(f.files) > 0 {
		can = resolve.ForUpload
	}
	target, err := resolve.Resolve(context.Background(), cache.Offline(), can, channel)
	if err != nil {
		return abort(stderr, log, rec, coldCache(err).Error())
	}
	rec.ChannelID = target.ID

	// After resolution, because #eng and its C… id are the same conversation;
	// before any request, so a refusal costs nothing and reveals nothing.
	list := allowlist.FromEnv()
	err = list.Check(target)
	if err != nil {
		rec.Phase, rec.OK, rec.Error = audit.Refused, audit.Failed, err.Error()
		werr := writeRecord(stderr, log, rec)
		return orAuditFailure(werr, fail(stderr, "%v", err))
	}

	if !channelRe.MatchString(target.ID) {
		return abort(stderr, log, rec, "refusing suspicious channel: "+target.ID)
	}
	if thread != "" && !threadRe.MatchString(thread) {
		return abort(stderr, log, rec, "refusing suspicious thread ts: "+thread)
	}
	if verb != verbSend && !threadRe.MatchString(ts) {
		return abort(stderr, log, rec, "refusing suspicious message ts: "+ts)
	}

	// Slack has no table syntax, so a pasted Markdown table arrives as the
	// pipes and dashes that were typed. Warned about rather than refused: it is
	// still a legible message, and the sender is the one who decides.
	if looksLikeTable(text) {
		fmt.Fprintln(stderr,
			"sez: warning — this looks like a Markdown table; Slack renders it as literal text.")
	}

	a := attempt{stdout: stdout, stderr: stderr, log: log, baseURL: baseURL, cred: wcred, target: target}
	p := preview{stdout: stdout, target: target, haveCred: credErr == nil}

	if verb != verbSend {
		// The read gates the write: an edit or delete rewrites or destroys what
		// people have already read, so the render must show it as it stands. If
		// the current text cannot be read, nothing changes — not under --send
		// either. A tool that could not show its own change has not earned the
		// send, and a warning is something people learn to scroll past.
		// Stamped before the read, so the intent line that outlives a kill, the
		// abort written when the read fails, and every failure outcome all name
		// the message that was addressed. Without it a delete interrupted before
		// its outcome cannot be reconciled against Slack at all.
		rec.TS = ts
		now, readErr := readMessage(context.Background(), target.ID, ts, verb)
		if readErr != nil {
			return abort(stderr, log, rec, "cannot read the current message: "+readErr.Error())
		}
		// For a delete the record carries what was read, so the log says what
		// was destroyed rather than merely that something was.
		if verb == verbDelete {
			rec.Text = now.Text
			// The record names every file whose share the delete removes.
			// Without this they would survive in neither the message nor the
			// log, and the log is supposed to say what a delete took.
			rec.Files = append(rec.Files, now.Files...)
		} else {
			// An edit's Text is the replacement, so the wording it replaced has
			// nowhere else to go, and sez has just read it.
			rec.PreviousText = now.Text
		}
		if !send {
			renderChange(p, verb, ts, now, text)
			rec.Phase, rec.OK = audit.DryRun, audit.Succeeded
			return orAuditFailure(writeRecord(stderr, log, rec), 0)
		}
		return change(a, rec, verb, ts)
	}

	if len(f.files) > 0 {
		files, ferr := openFiles(f.files)
		if ferr != nil {
			return abort(stderr, log, rec, ferr.Error())
		}
		defer closeFiles(files)
		rec.Files = auditFiles(files)
		if !send {
			renderUpload(p, thread, text, files)
			rec.Phase, rec.OK = audit.DryRun, audit.Succeeded
			return orAuditFailure(writeRecord(stderr, log, rec), 0)
		}
		return upload(a, rec, thread, text, files)
	}

	if !send {
		render(stdout, target, thread, text, credErr == nil)
		rec.Phase, rec.OK = audit.DryRun, audit.Succeeded
		return orAuditFailure(writeRecord(stderr, log, rec), 0)
	}
	return post(a, rec, thread, text)
}

func post(a attempt, rec audit.Post, thread, text string) int {
	stdout, stderr, log, baseURL, c, target := a.stdout, a.stderr, a.log, a.baseURL, a.cred, a.target
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(stderr, log, rec, err.Error())
	}

	// Intent is recorded BEFORE the request, so an interrupted or hung send
	// still leaves evidence of what was about to go out. ok is null: unknown,
	// not success. This is the one record that survives SIGKILL.
	intent := rec
	intent.Phase, intent.OK = audit.Intent, audit.Unknown
	err = log.Post(intent)
	if err != nil {
		return fail(stderr, "%v — refusing to send", err)
	}

	// A catchable signal anywhere in the window still leaves an outcome. The
	// window ends when the outcome record is on disk, not when the response
	// arrives: the permalink fetch after it is a whole round trip, and a guard
	// released before that would leave only an intent line for a message that
	// had definitely gone. SIGKILL cannot be caught, which is why the intent
	// above precedes the request.
	//
	// pending is a snapshot. The handler runs on its own goroutine, so reading
	// the record the outcome path is midway through filling in would be a race
	// as well as a worse record.
	pending := rec
	stop := audit.OnAbort(func(sig os.Signal) {
		pending.Phase, pending.OK = audit.Abort, audit.Unknown
		pending.Error = fmt.Sprintf("interrupted by %s before the outcome was recorded; delivery unknown", sig)
		// Reported, not swallowed. The exit status stays the conventional
		// 128+signal — that is what a caller reads to know the process was
		// killed — so stderr is the only place this can be said.
		_ = writeRecord(stderr, log, pending)
		_ = log.Close()
	})
	defer stop()

	client := slackwrite.New(c, slackhttp.New(timeout), baseURL)
	resp, err := client.PostMessage(context.Background(), target.ID, text, thread)

	rec.Phase = audit.Outcome
	// Only Delivered reaches the success path. Every other outcome — including
	// one a later version of Classify adds — is a refusal here, because resp is
	// nil whenever err is not, and "not a failure I recognise" must never be
	// read as "sent".
	switch outcome := slackwrite.Classify(err); {
	case outcome == slackwrite.Ambiguous:
		// Never recorded as failed and never retried: a retry after a transport
		// failure duplicates a message that was in fact delivered.
		rec.OK = audit.Unknown
		rec.Error = err.Error() + " — delivery unknown, check Slack before retrying"
		werr := writeRecord(stderr, log, rec)
		return orAuditFailure(werr, fail(stderr, "%v (delivery UNKNOWN — logged)", err))
	case outcome != slackwrite.Delivered:
		rec.OK, rec.Error = audit.Failed, err.Error()
		werr := writeRecord(stderr, log, rec)
		code := fail(stderr, "%v  (logged)", err)
		// Named only for this refusal. It is the one Slack sends that the
		// reader can act on, and the bare code says neither which scope is
		// missing nor where it is granted; printing it for every failure would
		// send someone editing OAuth scopes over a wrong channel name.
		if slackwrite.Code(err) == slackwrite.CodeMissingScope {
			fmt.Fprint(stderr, missingScopeFor(err))
		}
		return orAuditFailure(werr, code)
	}

	rec.OK, rec.TS, rec.SlackText = audit.Succeeded, resp.TS, resp.Text
	// Best-effort: the message is already delivered, so a permalink failure
	// must not turn a success into an error. Fetched for every target: knowing
	// the channel precisely is not the same as having a link to the message,
	// which is the only field in the record that points at what was sent.
	link, perr := client.Permalink(context.Background(), target.ID, resp.TS)
	if perr == nil {
		rec.Permalink = link
	}
	werr := writeRecord(stderr, log, rec)

	// Delivery is reported even when the record was lost. The message did go
	// out, and the exit status below says only that it cannot be accounted for.
	fmt.Fprintf(stdout, "sent to %s", target.Input)
	if thread != "" {
		fmt.Fprintf(stdout, " (thread %s)", thread)
	}
	fmt.Fprintf(stdout, " ts=%s\n", resp.TS)
	if rec.Permalink != "" {
		fmt.Fprintln(stdout, rec.Permalink)
	}
	return orAuditFailure(werr, 0)
}

func render(stdout io.Writer, target resolve.Target, thread, text string, haveCred bool) {
	fmt.Fprintln(stdout, "--- dry run: nothing sent ---")
	if target.ID != target.Input {
		fmt.Fprintf(stdout, "channel : %s -> %s\n", target.Input, target.ID)
	} else {
		fmt.Fprintf(stdout, "channel : %s\n", target.Input)
	}
	if thread != "" {
		fmt.Fprintf(stdout, "thread  : %s\n", thread)
	}
	if !haveCred {
		fmt.Fprintln(stdout, "token   : absent (posting currently impossible)")
	}
	fmt.Fprintln(stdout, "--- message as Slack will receive it ---")
	fmt.Fprintln(stdout, text)
	fmt.Fprintln(stdout, "----------------------------------------")
}

// renderChange is the dry run for an edit or a delete. What those act on is
// something people have already read, so the render's job is to show it as it
// stands; an edit also shows the replacement beside it, so what changes is
// visible rather than asserted.
func renderChange(p preview, v verb, ts string, now current, text string) {
	stdout, target, haveCred := p.stdout, p.target, p.haveCred
	fmt.Fprintln(stdout, "--- dry run: nothing sent ---")
	if target.ID != target.Input {
		fmt.Fprintf(stdout, "channel : %s -> %s\n", target.Input, target.ID)
	} else {
		fmt.Fprintf(stdout, "channel : %s\n", target.Input)
	}
	// The address, not only the channel: a ts copied from the wrong line names
	// a real message whose text reads plausibly, and the reader would have
	// nothing to check it against.
	fmt.Fprintf(stdout, "message : %s\n", ts)
	if !haveCred {
		fmt.Fprintln(stdout, "token   : absent (posting currently impossible)")
	}
	fmt.Fprintln(stdout, "--- current message ---")
	fmt.Fprintln(stdout, now.Text)
	// A delete takes the files out of the message along with it, so what goes
	// has to be shown rather than merely counted.
	for i, f := range now.Files {
		label := "files   :"
		if i > 0 {
			label = "         "
		}
		fmt.Fprintf(stdout, "%s %s\n", label, fileLabel(f))
	}
	// A message is a share of a file, not the file. Deleting it takes the share
	// and leaves the file in the workspace, reachable by id and counted against
	// storage — observed to be true through Slack's own interface as well as
	// through chat.delete, so this is not a difference between this tool and
	// Slack. It is said here because it is the moment somebody is deciding, and
	// almost nobody expects it.
	if len(now.Files) > 0 {
		fmt.Fprintln(stdout, "note    : the files stay in Slack; deleting a message never removes them")
	}
	if v == verbEdit {
		fmt.Fprintln(stdout, "--- replacement ---")
		fmt.Fprintln(stdout, text)
	}
	fmt.Fprintln(stdout, "----------------------------------------")
}

// attempt is what every send path shares: where it reports, where it records,
// and how it reaches the one conversation addressed.
type attempt struct {
	stdout, stderr io.Writer
	log            *audit.Log
	baseURL        string
	cred           cred.Write
	target         resolve.Target
}

// preview is what every dry run shares: where it prints, what it addresses and
// whether a write credential is there to send with.
type preview struct {
	stdout   io.Writer
	target   resolve.Target
	haveCred bool
}

// change is the send path for an edit or a delete. It mirrors post's shape —
// intent before the request, a catchable-signal outcome, one attempt, a
// classified outcome — because an edit and a delete deserve the same accounting
// as a send: the read has already happened and gated this call.
func change(a attempt, rec audit.Post, v verb, ts string) int {
	stdout, stderr, log, baseURL, c, target := a.stdout, a.stderr, a.log, a.baseURL, a.cred, a.target
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(stderr, log, rec, err.Error())
	}

	// Intent is recorded BEFORE the request, so an interrupted attempt still
	// leaves evidence of what was about to change. ok is null: unknown, not
	// success. This is the one record that survives SIGKILL.
	intent := rec
	intent.Phase, intent.OK = audit.Intent, audit.Unknown
	err = log.Post(intent)
	if err != nil {
		return fail(stderr, "%v — refusing to send", err)
	}

	// A catchable signal anywhere in the window still leaves an outcome, exactly
	// as it does for a send.
	pending := rec
	stop := audit.OnAbort(func(sig os.Signal) {
		pending.Phase, pending.OK = audit.Abort, audit.Unknown
		pending.Error = fmt.Sprintf("interrupted by %s before the outcome was recorded; delivery unknown", sig)
		_ = writeRecord(stderr, log, pending)
		_ = log.Close()
	})
	defer stop()

	client := slackwrite.New(c, slackhttp.New(timeout), baseURL)
	ctx := context.Background()

	switch v {
	case verbEdit:
		resp, err := client.Update(ctx, target.ID, ts, rec.Text)
		if err != nil {
			return changeOutcome(stderr, log, rec, err)
		}
		rec.OK, rec.SlackText = audit.Succeeded, resp.Text
		// rec.TS stays the addressed ts and is never taken from the reply. It is
		// the argument the caller gave and the intent line already carries it,
		// so an echo that was empty — a shape envelope accepts — or that
		// differed would leave the two lines naming different messages and send
		// the permalink lookup after the wrong one.
		// An edit changes what a message says, and the ts and channel are both
		// in hand: the same reasoning that makes a send fetch one.
		link, perr := client.Permalink(ctx, target.ID, rec.TS)
		if perr == nil {
			rec.Permalink = link
		}
	case verbDelete:
		err := client.Delete(ctx, target.ID, ts)
		if err != nil {
			return changeOutcome(stderr, log, rec, err)
		}
		rec.OK = audit.Succeeded
	}

	werr := writeRecord(stderr, log, rec)

	// The change is reported even when the record was lost: it went through, and
	// the exit status below says only that it cannot be accounted for.
	if v == verbEdit {
		fmt.Fprintf(stdout, "edited to %s ts=%s\n", target.Input, rec.TS)
		// The link is what lets a reader confirm the edit landed on the message
		// they meant. A send prints it for the same reason; an edit paid the
		// same round trip for it.
		if rec.Permalink != "" {
			fmt.Fprintln(stdout, rec.Permalink)
		}
	} else {
		fmt.Fprintf(stdout, "deleted from %s ts=%s\n", target.Input, ts)
	}
	return orAuditFailure(werr, 0)
}

// changeOutcome records the failure of an edit or a delete. It mirrors post's
// classification: ambiguous stays unknown, a refusal is failed, and the one
// Slack error a reader can act on names its missing scope.
func changeOutcome(stderr io.Writer, log *audit.Log, rec audit.Post, err error) int {
	rec.Phase = audit.Outcome
	switch outcome := slackwrite.Classify(err); {
	case outcome == slackwrite.Ambiguous:
		rec.OK = audit.Unknown
		rec.Error = err.Error() + " — delivery unknown, check Slack before retrying"
		werr := writeRecord(stderr, log, rec)
		return orAuditFailure(werr, fail(stderr, "%v (delivery UNKNOWN — logged)", err))
	default:
		rec.OK, rec.Error = audit.Failed, err.Error()
		werr := writeRecord(stderr, log, rec)
		code := fail(stderr, "%v  (logged)", err)
		if slackwrite.Code(err) == slackwrite.CodeMissingScope {
			fmt.Fprint(stderr, missingScopeFor(err))
		}
		return orAuditFailure(werr, code)
	}
}

// viaHear reads a message by running the hear binary, which holds the read
// credential sez does not. It is the production readMessage.
//
// hear is located beside the running executable first — install.sh puts all four
// commands in one directory — then on PATH. hear thread returns a transcript
// JSON of the thread rooted at ts, whose first message is the one we asked for.
// hearTimeout bounds the read. hear can block rather than fail — a locked
// keychain prompts, a stalled connection waits — and an unbounded read leaves
// sez waiting with it, silent, before a change the human is expecting to be
// shown.
var hearTimeout = 30 * time.Second

// hearBound follows hear's own request timeout. hear makes up to a few requests
// per read, each bounded by HERSAID_TIMEOUT, and a thread pages to its end — so
// a caller who raised that timeout for a slow link would otherwise have its
// child killed mid-request, and the read gates the write.
func hearBound() time.Duration {
	t, err := slackhttp.TimeoutFromEnv()
	if err == nil && 2*t > hearTimeout {
		return 2 * t
	}
	return hearTimeout
}

func viaHear(ctx context.Context, channelID, ts string, v verb) (current, error) {
	bin, err := findHear()
	if err != nil {
		return current{}, err
	}
	bound := hearBound()
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "thread", channelID, ts, "--json")
	// The write credential is withheld from the child. Running hear is what
	// keeps the read off sez's credential; handing over the whole environment
	// would give it straight back — and findHear falls back to PATH, so the
	// binary receiving it is not necessarily the one that was installed.
	cmd.Env = withoutWriteToken(os.Environ())
	// The context kills hear, but Output waits for the stdout pipe to close,
	// and anything that outlived the kill still holds it. Without this the
	// bound is a property of the child's process tree rather than of this code.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		// hear's own stderr, when it left any: this path fails closed, and a
		// refusal that cannot say why leaves nothing to act on. An ExitError
		// carries it because Output captures stderr.
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return current{}, fmt.Errorf("%w: %s", errHear, bytes.TrimSpace(ee.Stderr))
		}
		if ctx.Err() != nil {
			return current{}, fmt.Errorf("%w %s", errHearNoAnswerWithin, bound)
		}
		return current{}, fmt.Errorf("hear: %w", err)
	}
	var tr struct {
		Messages []struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
			// Partial is what every hear has always emitted; Blocks is a
			// pointer so that a hear too old to emit it is distinguishable from
			// one saying there are none.
			Partial bool  `json:"partial"`
			Blocks  *bool `json:"blocks"`
			Files   []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"files"`
		} `json:"messages"`
	}
	err = json.Unmarshal(out, &tr)
	if err != nil {
		return current{}, fmt.Errorf("parsing hear output: %w", err)
	}
	for _, m := range tr.Messages {
		if m.TS != ts {
			continue
		}
		// Fails closed on a message whose text is not the whole of it. A
		// blocks-built message reads as fallback text or as nothing, so the
		// render would show something the author never wrote — and Slack drops
		// existing blocks from a text-only chat.update, so the replacement
		// would not be what the message became either.
		// A hear that does not report blocks is one sez will not change anything
		// on. findHear falls back to PATH, so the two binaries are not always
		// upgraded together, and an older hear is wrong in both directions: it
		// says partial without saying why, and — older still, before it knew
		// files existed — it calls a message carrying them plain. The second is
		// the dangerous one, because it looks exactly like a message with
		// nothing to show. Neither can be told apart from a sound reply by
		// reading the reply, so the field's absence is the refusal.
		if m.Blocks == nil {
			return current{}, fmt.Errorf(
				"%w at %s carries; update hear, or change the message in Slack", errThisHearTooOld, ts)
		}
		// The other direction of the same problem. A later hear could set partial
		// for a kind of content this sez has never heard of, with no blocks and
		// no files to explain it — and that is the same unshowable message the
		// gate exists to refuse, arriving from the future rather than the past.
		if m.Partial && !*m.Blocks && len(m.Files) == 0 {
			return current{}, fmt.Errorf(
				"%w at %s as carrying something this sez does not know about; update sez, or change the message in Slack", errHearReportsMessage, ts)
		}
		if *m.Blocks {
			return current{}, fmt.Errorf(
				"%w at %s carries blocks that cannot be shown here; change it in Slack", errMessage, ts)
		}
		names := make([]audit.File, 0, len(m.Files))
		for _, f := range m.Files {
			// Neither a printable name nor an id is nothing to show, and a
			// delete must not offer a blank line as its account of what goes.
			if strings.TrimSpace(termsafe.Text(f.Name)) == "" && f.ID == "" {
				return current{}, fmt.Errorf(
					"%w at %s carries a file that cannot be identified", errMessage, ts)
			}
			names = append(names, audit.File{Name: f.Name, ID: f.ID})
		}
		// A file can be shown by name, and a delete removes the message and its
		// share of the file rather than the file itself, so a delete may
		// proceed. An edit may not: chat.update carries text alone, and whether
		// the shares survive it is not established — an edit that silently
		// detached them would be the very thing this gate exists to prevent.
		if len(names) > 0 && v == verbEdit {
			return current{}, fmt.Errorf(
				"%w at %s carries files; sez cannot edit a message with files, only delete it", errMessage, ts)
		}
		return current{Text: m.Text, Files: names}, nil
	}
	return current{}, fmt.Errorf("%w %s", errHearNoMessageTs, ts)
}

// findHear names the hear binary to run, preferring the one installed beside
// sez so that a version mismatch between the two is less likely.
// withoutWriteToken returns env with the write credential's variable removed,
// leaving everything else the child needs — HOME, PATH and the XDG
// directories among them — in place.
func withoutWriteToken(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); key == cred.WriteEnvVar {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func findHear() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "hear")
		fi, err := os.Stat(candidate)
		if err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	p, err := exec.LookPath("hear")
	if err != nil {
		return "", fmt.Errorf("the hear binary is not installed: %w", err)
	}
	return p, nil
}

// coldCache says which command fills an empty cache. Naming it is the whole of
// the fix: this command holds a write credential only, so it cannot fetch the
// list itself and a retry will find the cache just as empty.
//
// Only a person can get this far. chat.postMessage resolves a #name itself, so
// a cold conversation list is not fatal for a channel — and `hear dms` fills
// both lists, which is what a person needs.
func coldCache(err error) error {
	if !errors.Is(err, cache.ErrCold) {
		return err
	}
	return fmt.Errorf("%w — run 'hear dms' first; %s has no read credential to fill it", err, name)
}

// abort records a refusal that happened before any request and exits non-zero.
func abort(stderr io.Writer, log *audit.Log, rec audit.Post, msg string) int {
	rec.Phase, rec.OK, rec.Error = audit.Abort, audit.Failed, msg
	werr := writeRecord(stderr, log, rec)
	return orAuditFailure(werr, fail(stderr, "%s", msg))
}

// writeRecord appends rec, reporting a failure on stderr. The error is returned
// rather than swallowed because it outranks whatever the record was describing.
func writeRecord(stderr io.Writer, log *audit.Log, rec audit.Post) error {
	err := log.Post(rec)
	if err != nil {
		fmt.Fprintf(stderr, "sez: AUDIT WRITE FAILED — %v\n", err)
		return err
	}
	return nil
}

// orAuditFailure reports a lost record in place of code, which is only ever a
// widening: what the record was going to say has already been printed. The
// intent path does not use it and stays at ExitFailure — it refuses to send, so
// there is no attempt left unaccounted for.
func orAuditFailure(werr error, code int) int {
	if werr != nil {
		return cli.ExitAuditFailure
	}
	return code
}

// looksLikeTable reports the separator row of a Markdown table, which is the
// one part of the syntax that is unmistakable — a row of cells could be any
// prose containing pipes.
func looksLikeTable(text string) bool {
	return strings.Contains(text, "|--") || strings.Contains(text, "| ---")
}

// verb names which message operation an invocation performs.
type verb int

const (
	verbSend verb = iota
	verbEdit
	verbDelete
)

// verbAction is the audit Action an attempt is recorded with. Every post record
// names its action, ordinary sends included.
func verbAction(v verb) audit.Action {
	switch v {
	case verbEdit:
		return audit.Update
	case verbDelete:
		return audit.Delete
	default:
		return audit.Send
	}
}

// flags is what the option block sets.
type flags struct {
	send   bool
	md     bool
	edit   bool
	del    bool
	thread string
	// files are the --file arguments, in the order given.
	files []string
	// rest is everything after the options: the target, then the text.
	rest []string
}

// parseFlags reads the leading options. done reports that there is nothing left
// to do — help or a version was printed, or the invocation was wrong — and code
// is what the command should exit with.
//
// The shared flags are answered here rather than before this function, because
// only a parser knows which options take a value: in `sez --thread --help`,
// "--help" is the thread timestamp. This runs before anything is read, created
// or resolved, so answering them here still costs nothing.
func parseFlags(args []string, stdout, stderr io.Writer) (f flags, done bool, code int) {
	for len(args) > 0 {
		switch args[0] {
		case "--send":
			f.send, args = true, args[1:]
		case "--dry-run", "-n":
			f.send, args = false, args[1:]
		case "--md":
			f.md, args = true, args[1:]
		case "--edit":
			f.edit, args = true, args[1:]
		case "--delete":
			f.del, args = true, args[1:]
		case "--thread":
			if len(args) < 2 || args[1] == "" {
				return f, true, cli.Usage(stderr, name, usage, "--thread needs a parent ts")
			}
			f.thread, args = args[1], args[2:]
		case "--file":
			if len(args) < 2 || args[1] == "" {
				return f, true, cli.Usage(stderr, name, usage, "--file needs a path")
			}
			// stdin is the text channel, and a file has to be seekable: the
			// length is declared before the bytes are sent.
			if args[1] == "-" {
				return f, true, cli.Usage(stderr, name, usage, "--file needs a path; stdin carries the text, not a file")
			}
			f.files, args = append(f.files, args[1]), args[2:]
		case "-h", "--help":
			return f, true, cli.Help(stdout, usage)
		case "--version":
			return f, true, cli.Version(stdout, name)
		case "--":
			f.rest = args[1:]
			return f, false, 0
		default:
			if strings.HasPrefix(args[0], "-") && args[0] != "-" {
				return f, true, cli.Usage(stderr, name, usage, "unknown flag: "+args[0])
			}
			f.rest = args
			return f, false, 0
		}
	}
	return f, false, 0
}

func readText(rest []string, stdin io.Reader) (string, error) {
	if len(rest) == 0 || rest[0] == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		// Trailing newlines are dropped. Keeping them would put a blank line at
		// the end of every piped message, on the wire and in the record — a
		// heredoc and an editor both end with one.
		return trimTrailingNewlines(string(b)), nil
	}
	return strings.Join(rest, " "), nil
}

// trimTrailingNewlines drops newlines at the end and nothing else: a message
// deliberately ending in spaces still does, and interior blank lines are the
// paragraph breaks somebody typed.
func trimTrailingNewlines(s string) string {
	return strings.TrimRight(s, "\n")
}

func fail(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "sez: "+format+"\n", a...)
	return cli.ExitFailure
}

// fileLabel is what to call a file on a terminal: its name made safe to print,
// or its id when nothing printable is left. The raw name stays in the record —
// termsafe is for display, and the log should say what Slack has.
func fileLabel(f audit.File) string {
	if name := strings.TrimSpace(termsafe.Text(f.Name)); name != "" {
		return name
	}
	return f.ID
}
