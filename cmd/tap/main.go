// Command tap adds or removes an emoji reaction as the user.
//
// The ordering is sez's, on the reaction lane: the credential gate, resolution,
// the write allowlist, an intent record before the request, one attempt, and an
// outcome that distinguishes "Slack refused it" from "it may have been applied".
// The intent record and the unknown outcome hold by construction, and the tests
// are what keep them holding.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/ledger-rocket/hersaid/internal/allowlist"
	"github.com/ledger-rocket/hersaid/internal/audit"
	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackwrite"
)

const name = "tap"

const usage = `usage: tap [--remove] [--dry-run] <#chan|@person|ID> <ts> <emoji>

  ts      the target message timestamp, e.g. 1785712596.256909
          (sez prints it on send; hear shows it on threaded messages)
  emoji   name with or without colons, e.g. :+1: or thumbsup

  A reaction is fully described by its own arguments — there is no prose to
  review — so unlike sez it needs no render step, and it can carry a different
  permission policy from messages.

  Options:
    -r, --remove      remove the reaction instead of adding it
    -n, --dry-run     resolve and validate without calling Slack
        --            stop reading options; what follows are the arguments
    -h, --help        print this help and exit 0
        --version     print the version and exit 0

  Environment:
    SLACK_WRITE_TOKEN       write credential; set but empty means "absent".
                            The write app needs reactions:write as well as
                            chat:write.
    HERSAID_WRITE_CHANNELS  comma-separated ids and/or #names that may be
                            written to; unset means unrestricted
    HERSAID_TIMEOUT         request timeout in whole seconds (default 20)

  Exit: 0 applied, already applied, or dry run; 1 refused or failed; 2 usage;
        3 the attempt could not be recorded in the audit log.

  Audit log: ~/.local/state/hersaid/posts.jsonl
`

const noCredential = `tap: no write credential found (keychain service: hersaid-write).
  Reacting is disabled by design until one exists:
    security add-generic-password -a "$USER" -s hersaid-write -w
  The write app also needs the reactions:write scope alongside chat:write.
`

const missingScope = `  The write app needs reactions:write as well as chat:write. Add it under
  User Token Scopes, reinstall the app, then enrol the new token:
    hersaid-setup
`

var (
	tsRe    = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	emojiRe = regexp.MustCompile(`^[a-z0-9_+-]+$`)
)

// resolveWrite is a variable so a test can observe whether the credential was
// consulted at all. Asking for help or for a version must never reach a
// keychain: on macOS that is a modal unlock prompt, and on Linux a D-Bus call
// that hangs where there is no Secret Service.
var resolveWrite = cred.ResolveWrite

// onAbort is a variable so a test can watch when the guard is released. Unlike
// sez, whose permalink fetch leaves a round trip inside the window that a test
// can hold open and signal into, everything between the reply and the record
// here is local — so the ordering is asserted rather than provoked.
var onAbort = audit.OnAbort

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, slackwrite.SlackAPI))
}

func run(args []string, stdout, stderr io.Writer, baseURL string) int {
	// First, and with no side effects of its own: --help and --version are
	// answered here, before the audit log is created and before any credential
	// is resolved.
	f, done, code := parseFlags(args, stdout, stderr)
	if done {
		return code
	}
	remove, dry, rest := f.remove, f.dry, f.rest
	if len(rest) != 3 {
		return cli.Usage(stderr, name, usage, "")
	}
	channel, targetTS, emoji := rest[0], rest[1], rest[2]

	if strings.HasPrefix(channel, "-") {
		return fail(stderr, "refusing a channel that looks like a flag: %s", channel)
	}
	if !tsRe.MatchString(targetTS) {
		return fail(stderr, "not a message timestamp: %s  (expected e.g. 1785712596.256909)", targetTS)
	}
	emoji = strings.TrimSuffix(strings.TrimPrefix(emoji, ":"), ":")
	if !emojiRe.MatchString(emoji) {
		return fail(stderr, "not a valid emoji name: %s  (lowercase letters, digits, _ + - only)", emoji)
	}

	wcred, credErr := resolveWrite()

	log, err := audit.Open(audit.DefaultPath())
	if err != nil {
		return fail(stderr, "%v", err)
	}
	defer log.Close()
	// The same file sez writes, so a mode it cannot enforce must be as visible
	// here as it is there: whichever command happened to open it is not
	// something the person reading the warning chose.
	for _, w := range log.Warnings() {
		fmt.Fprintf(stderr, "tap: warning — %s\n", w)
	}

	action := audit.Add
	if remove {
		action = audit.Remove
	}
	rec := audit.Reaction{Action: action, Channel: channel, TS: targetTS, Emoji: emoji, DryRun: dry}

	// The gate: still before any client exists, so a refusal cannot issue a
	// request — but after the log, because a refused reaction is an attempt and
	// the log holds every attempt.
	if !dry && credErr != nil {
		fmt.Fprint(stderr, noCredential)
		rec.Phase, rec.OK, rec.Error = audit.Abort, audit.Failed, "no write credential"
		return orAuditFailure(writeRecord(stderr, log, rec), cli.ExitFailure)
	}

	// reactions.add needs a real conversation id, so unlike chat.postMessage a
	// bare #name cannot be passed through.
	target, err := resolve.Resolve(context.Background(), cache.Offline(), resolve.ForReact, channel)
	if err != nil {
		return abort(stderr, log, rec, coldCache(channel, err).Error())
	}
	rec.ChannelID = target.ID

	err = allowlist.FromEnv().Check(target)
	if err != nil {
		rec.Phase, rec.OK, rec.Error = audit.Refused, audit.Failed, err.Error()
		werr := writeRecord(stderr, log, rec)
		return orAuditFailure(werr, fail(stderr, "%v", err))
	}

	if dry {
		fmt.Fprintln(stdout, "--- dry run: nothing sent ---")
		fmt.Fprintf(stdout, "action  : %s\n", action)
		fmt.Fprintf(stdout, "channel : %s -> %s\n", target.Input, target.ID)
		fmt.Fprintf(stdout, "message : %s\n", targetTS)
		fmt.Fprintf(stdout, "emoji   : :%s:\n", emoji)
		if credErr != nil {
			fmt.Fprintln(stdout, "token   : absent (reacting currently impossible)")
		}
		rec.Phase, rec.OK = audit.DryRun, audit.Succeeded
		return orAuditFailure(writeRecord(stderr, log, rec), 0)
	}
	return react(stdout, stderr, log, rec, baseURL, wcred, remove, target, targetTS, emoji)
}

func react(stdout, stderr io.Writer, log *audit.Log, rec audit.Reaction, baseURL string,
	c cred.Write, remove bool, target resolve.Target, targetTS, emoji string,
) int {
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		return abort(stderr, log, rec, err.Error())
	}

	// Written before the request, so a kill mid-flight still leaves evidence a
	// reaction was attempted.
	intent := rec
	intent.Phase, intent.OK = audit.Intent, audit.Unknown
	err = log.Reaction(intent)
	if err != nil {
		return fail(stderr, "%v — refusing to send", err)
	}

	// The window ends when the outcome record is on disk, not when Slack
	// answers: a guard released in between would leave a reaction Slack had
	// applied with nothing but an intent line to account for it.
	//
	// pending is a snapshot. The handler runs on its own goroutine, so reading
	// the record the outcome path is midway through filling in would be a race
	// as well as a worse record.
	pending := rec
	stop := onAbort(func(sig os.Signal) {
		pending.Phase, pending.OK = audit.Abort, audit.Unknown
		pending.Error = fmt.Sprintf("interrupted by %s before the outcome was recorded; outcome unknown", sig)
		// Reported, not swallowed. The exit status stays the conventional
		// 128+signal — that is what a caller reads to know the process was
		// killed — so stderr is the only place this can be said.
		_ = writeRecord(stderr, log, pending)
		_ = log.Close()
	})
	defer stop()

	client := slackwrite.New(c, slackhttp.New(timeout), baseURL)
	if remove {
		err = client.RemoveReaction(context.Background(), target.ID, targetTS, emoji)
	} else {
		err = client.AddReaction(context.Background(), target.ID, targetTS, emoji)
	}

	rec.Phase = audit.Outcome
	// Only Delivered and AlreadyDone reach the success path. Every other
	// outcome — including one a later version of Classify adds — is reported,
	// because "not a failure I recognise" must never be read as "applied".
	switch outcome := slackwrite.Classify(err); {
	case outcome == slackwrite.AlreadyDone:
		// The end state is the one that was asked for, so it is a success
		// carrying the code as context rather than an error.
		code := slackwrite.Code(err)
		rec.OK, rec.Error = audit.Succeeded, code
		werr := writeRecord(stderr, log, rec)
		what := "was not on that message"
		if code == slackwrite.CodeAlreadyReacted {
			what = "already on that message"
		}
		fmt.Fprintf(stdout, ":%s: %s\n", emoji, what)
		return orAuditFailure(werr, 0)
	case outcome == slackwrite.Ambiguous:
		// Slack may have applied the reaction, so this is unknown rather than
		// failed, and never retried automatically.
		rec.OK = audit.Unknown
		rec.Error = err.Error() + " — outcome unknown, check Slack before retrying"
		werr := writeRecord(stderr, log, rec)
		return orAuditFailure(werr, fail(stderr, "%v (outcome UNKNOWN — logged)", err))
	case outcome != slackwrite.Delivered:
		rec.OK, rec.Error = audit.Failed, err.Error()
		werr := writeRecord(stderr, log, rec)
		code := fail(stderr, "%v  (logged)", err)
		// The one Slack refusal with a fix the user can act on. Nothing about
		// this invocation was wrong, so without the scope named here the reading
		// is "tap is broken" rather than "the app was installed without
		// reactions:write".
		if slackwrite.Code(err) == slackwrite.CodeMissingScope {
			fmt.Fprint(stderr, missingScope)
		}
		return orAuditFailure(werr, code)
	}

	rec.OK = audit.Succeeded
	werr := writeRecord(stderr, log, rec)
	fmt.Fprintf(stdout, "%s :%s: on %s (%s)\n",
		strings.TrimPrefix(string(rec.Action), "react."), emoji, target.Input, targetTS)
	return orAuditFailure(werr, 0)
}

// coldCache says which command fills an empty cache. Naming it is the whole of
// the fix: this command holds a write credential only, so it cannot fetch the
// list itself and a retry will find the cache just as empty.
//
// Which list is missing follows from how the target was addressed: a person
// needs the user list, which `hear dms` fills, and a channel needs the
// conversation list, which `hear channels` fills.
func coldCache(target string, err error) error {
	if !errors.Is(err, cache.ErrCold) {
		return err
	}
	verb := "hear channels"
	if strings.HasPrefix(target, "@") {
		verb = "hear dms"
	}
	return fmt.Errorf("%w — run '%s' first; %s has no read credential to fill it", err, verb, name)
}

func abort(stderr io.Writer, log *audit.Log, rec audit.Reaction, msg string) int {
	rec.Phase, rec.OK, rec.Error = audit.Abort, audit.Failed, msg
	werr := writeRecord(stderr, log, rec)
	return orAuditFailure(werr, fail(stderr, "%s", msg))
}

// writeRecord appends rec, reporting a failure on stderr. The error is returned
// rather than swallowed because it outranks whatever the record was describing.
func writeRecord(stderr io.Writer, log *audit.Log, rec audit.Reaction) error {
	err := log.Reaction(rec)
	if err != nil {
		fmt.Fprintf(stderr, "tap: AUDIT WRITE FAILED — %v\n", err)
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

// flags is what the option block sets.
type flags struct {
	remove bool
	dry    bool
	// rest is everything after the options: channel, ts, emoji.
	rest []string
}

// parseFlags reads the leading options. done reports that there is nothing left
// to do — help or a version was printed, or the invocation was wrong — and code
// is what the command should exit with.
//
// The shared flags are answered here, mirroring sez: a parser is the only thing
// that knows which options take a value, and this runs before anything is
// created or resolved.
func parseFlags(args []string, stdout, stderr io.Writer) (f flags, done bool, code int) {
	for len(args) > 0 {
		switch args[0] {
		case "--remove", "-r":
			f.remove, args = true, args[1:]
		case "--dry-run", "-n":
			f.dry, args = true, args[1:]
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

func fail(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "tap: "+format+"\n", a...)
	return cli.ExitFailure
}
