// Command hersaid-setup enrols the two Slack credentials the hersaid commands
// read.
//
// It contains no write endpoint, and TestImportGraphExcludesWriteCode holds
// that: keeping write enrolment out of the read artifact is why this is a
// separate binary at all, and the same boundary is worth having here.
//
// Enrolment holds an unverified string rather than a cred.Read or a cred.Write.
// Only Slack can say which of those a string is, and that is the question
// auth.test is asked before anything is stored.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cred"

	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

const name = "hersaid-setup"

const usage = `usage: hersaid-setup [--with-write]

  Enrols the Slack credentials hear, sez and tap read.

  Read-only by default. The write credential is set up only under
  --with-write, so posting stays impossible until someone deliberately asks
  for it — and it needs a SECOND Slack app, because Slack issues one token
  per app per user and one app cannot yield separate read and write tokens.

  Each token is verified against auth.test before it is stored, and the scopes
  it was granted are checked against the role: a token carrying chat:write is
  refused for the read role, and a token without it is refused for the write
  role. That check is what mechanically enforces the two-app split, so a reply
  that does not say which scopes were granted is a refusal rather than a
  warning. A bot token is refused outright: it carries chat:write and would
  pass the scope check, but posts as an app rather than as you.

  Input is masked, one asterisk per character, and the token itself is never
  printed. What is printed is a fingerprint — enough to see that a paste was
  truncated, not enough to be the secret.

  Prompting needs a terminal. With stdin redirected nothing is asked and
  nothing is stored, so this is safe to run from a script.

  Options:
        --with-write  also enrol the write credential, which enables posting
    -h, --help        print this help and exit 0
        --version     print the version and exit 0

  A token is stored in the keychain under service hersaid-read or
  hersaid-write, or, on a host with no keyring, in
  ~/.config/hersaid/{read,write}.token at mode 0600. Where it went is printed;
  a path is not a secret, and it is what someone needs in order to revoke a
  credential later. An existing credential is never replaced without being
  named first and confirmed.

  Enrolling the read credential also caches who it belongs to, in
  ~/.cache/hersaid/identity.json, so hear can mark your own reactions. It holds
  no token, only an id and a digest of the credential it came from, and
  hear refresh clears it.

  Exit: 0 stored, declined or non-interactive; 1 a token was refused; 2 usage.
`

const nonInteractive = `hersaid-setup: stdin is not a terminal, so nothing was asked and nothing was
stored. Enrolment is interactive by design — the token is typed at a masked
prompt and checked against Slack before it is stored. Run it from a terminal,
or place a token yourself:
  keychain:  security add-generic-password -a "$USER" -s hersaid-read -w
  file:      ~/.config/hersaid/read.token (mode 0600)
`

const writeWarning = `The write token enables posting as you. It must come from a SECOND Slack app
carrying chat:write — the read app's token cannot post, and that separation is
what keeps hear incapable of writing.
`

const writeNotConfigured = `
Write token not configured — posting is disabled.
  It stays impossible until you deliberately run: hersaid-setup --with-write
`

// newConsole is a variable so a test can drive the prompts from a script
// instead of a terminal. It returns nil where there is no terminal to prompt
// at, which is what the non-interactive skip is built on.
var newConsole = openTerminal

// The production host is named through slackread's constant, the one route that
// is greppable and reviewable, rather than declared again here. A second copy
// is a second thing to get wrong, and this is the binary where pointing at the
// wrong host would be silent: it would hand a token to whatever answered.
//
// It costs the read client in this binary's import graph, which is a fair
// price — enrolment calls auth.test, a read endpoint, so the dependency is not
// a stretch. What it does not do is call through slackread: New takes a
// cred.Read, and enrolment holds a string that has not been established to be
// one, which is the question auth.test is being asked. The scope check also
// needs a response header no client method returns.
//
// Nothing about the capability split moves. cmd/hear's import graph is
// untouched, and no write endpoint enters this binary either.
func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, slackread.SlackAPI)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, baseURL string) int {
	// First, and with no side effects: --help and --version are answered before
	// the terminal is touched, before the filesystem, and above all before any
	// credential is looked at. Asking a tool what it does must not enrol
	// anyone in anything or raise a keychain prompt.
	if code, handled := cli.Intercept(args, name, usage, stdout); handled {
		return code
	}

	// Arity-strict, like every other verb in this suite: `hersaid-setup
	// --with-write --help` is a misuse, not a help screen, because only the
	// leading word is answered as a shared flag.
	withWrite := false
	switch len(args) {
	case 0:
	case 1:
		if args[0] != "--with-write" {
			return cli.Usage(stderr, name, usage, "unknown option: "+args[0])
		}
		withWrite = true
	default:
		return cli.Usage(stderr, name, usage, "one option at most")
	}

	con := newConsole(stdin, stdout)
	if con == nil {
		fmt.Fprint(stdout, nonInteractive)
		return cli.ExitOK
	}

	s := &setup{con: con, stdout: stdout, stderr: stderr, baseURL: baseURL}

	code := s.enrol(readRole)
	if !withWrite {
		fmt.Fprint(stdout, writeNotConfigured)
		return code
	}
	fmt.Fprintln(s.stdout)
	fmt.Fprint(stderr, writeWarning)
	if c := s.enrol(writeRole); c != cli.ExitOK {
		code = c
	}
	return code
}

// setup is one invocation: where the prompts go, where the answers come from,
// and which Slack to ask about a token.
type setup struct {
	con     *console
	stdout  io.Writer
	stderr  io.Writer
	baseURL string
	// hc is built on first use, so a run in which every prompt is declined
	// makes no client and no request.
	hc *http.Client
}

func (s *setup) client() *http.Client {
	if s.hc == nil {
		// DefaultTimeout rather than HERSAID_TIMEOUT: enrolment makes one call,
		// and a tuning knob for it is one more way to fail before the tool is
		// even usable. The commands that do real work read the variable.
		s.hc = slackhttp.New(slackhttp.DefaultTimeout)
	}
	return s.hc
}

// enrol runs one role's prompt from the question to the stored credential.
//
// The exit status separates two things worth telling apart: a refusal — a bot
// token, a scope mismatch, a token Slack would not accept — is
// ExitFailure, while declining the prompt, pasting nothing, cancelling, or
// keeping an existing credential is ExitOK. Nothing was enrolled in either
// case, but only one of them means something was wrong.
func (s *setup) enrol(r role) int {
	fmt.Fprintln(s.stdout)
	answer, err := s.con.Line(fmt.Sprintf("Set up the %s token now? [y/N] ", r.label))
	if err != nil {
		return s.declined(err)
	}
	if !affirmative(answer) {
		fmt.Fprintln(s.stdout, "  skipped")
		return cli.ExitOK
	}

	fmt.Fprintln(s.stdout, "  paste the User OAuth Token (xoxp-...), input masked:")
	token, err := s.con.Secret("  ")
	if err != nil {
		return s.declined(err)
	}
	token = strip(token)
	if token == "" {
		fmt.Fprintln(s.stderr, "  nothing received, not stored")
		return cli.ExitOK
	}
	fmt.Fprintf(s.stdout, "  received: %s\n", fingerprint(token))

	// Refused before any request: a bot token is recognisable by its prefix, and
	// there is nothing to learn from Slack about one.
	if strings.HasPrefix(token, botPrefix) {
		fmt.Fprint(s.stderr, botPrefixRefusal)
		return cli.ExitFailure
	}
	if !strings.HasPrefix(token, userPrefix) {
		fmt.Fprintln(s.stderr, "  note: expected a user token starting with xoxp-")
	}

	acct, err := verify(context.Background(), s.client(), s.baseURL, token, r)
	// Named before any refusal is printed, when Slack said whose token it is:
	// "it was refused" is far easier to act on beside "and it belongs to this
	// account", which is often the whole explanation.
	if acct.user != "" {
		fmt.Fprintf(s.stdout, "  verified as %s @ %s\n", acct.user, acct.team)
	}
	if err != nil {
		fmt.Fprintf(s.stderr, "%s\n  not stored\n", err)
		return cli.ExitFailure
	}
	fmt.Fprintf(s.stdout, "  scope check passed for the %s role\n", r.name)

	// Checked after verification, not before: nobody should be asked whether to
	// replace a working credential with one that turns out to be invalid.
	if prev, found := lookup(r); found {
		fmt.Fprintf(s.stdout, "  a %s credential is already stored in %s: %s\n", r.name, prev.where, prev.print)
		answer, err := s.con.Line("  Replace it? [y/N] ")
		if err != nil {
			return s.declined(err)
		}
		if !affirmative(answer) {
			fmt.Fprintf(s.stdout, "  kept the existing %s credential, nothing stored\n", r.name)
			return cli.ExitOK
		}
	}

	where, shadowed, err := store(r, token)
	if err != nil {
		fmt.Fprintf(s.stderr, "  could not store the token: %v\n", err)
		return cli.ExitFailure
	}
	fmt.Fprintf(s.stdout, "  stored in %s\n", where)

	// The read role only: hear reads with this credential, and the write token
	// belongs to the other app and to a different account context. auth.test
	// was already paid for above, so the id is free — and seeding it here is
	// what gives a fresh install the reaction marker from its first read rather
	// than after a whoami it may never run. Best-effort: enrolment succeeded,
	// and an unwritable cache is not a reason to say it did not.
	//
	// Skipped when the role's env var is set: cred reads that in preference to
	// anything stored, so hear will read with a credential other than the one
	// just enrolled, and seeding would replace an identity that credential
	// warmed with a key nothing matches — turning a working marker off. Leaving
	// the stored credential to be warmed by a later whoami is recoverable;
	// destroying a correct identity is not.
	_, outrankedByEnv := os.LookupEnv(r.envVar)
	if !r.needsWriteScope && acct.userID != "" && !outrankedByEnv {
		if err := cache.Write(cache.IdentityFile, cache.Identity{
			UserID: acct.userID, User: acct.user, Team: acct.team,
			CredentialKey: cred.KeyFor(token),
		}); err == nil {
			fmt.Fprintln(s.stdout, "  cached who it belongs to, so reads mark your own reactions")
		}
	}

	s.reportWhatOutranksIt(r, shadowed)
	return cli.ExitOK
}

// reportWhatOutranksIt names anything that will still be read in preference to
// what was just stored, or that is now dead material nothing will read again.
//
// Both are the same failure from the operator's chair: enrolment said it worked
// and the commands do not behave as though it did. Nothing is deleted or unset
// here — removing a file or a keychain item nobody asked about is the worse
// surprise, and the old token is somebody's to revoke deliberately.
func (s *setup) reportWhatOutranksIt(r role, shadowed []string) {
	if v, present := os.LookupEnv(r.envVar); present {
		fmt.Fprintf(s.stderr, "  note: %s is set here, and it is read before anything stored.\n", r.envVar)
		if strip(v) == "" {
			// cred tests presence, not value: an exported empty variable means
			// "no credential" and stops resolution rather than falling through.
			fmt.Fprintf(s.stderr, "  It is empty, which means no %s credential at all until it is unset.\n", r.name)
		} else {
			fmt.Fprintln(s.stderr, "  The commands will use it, not what was just stored, until it is unset.")
		}
	}
	for _, path := range shadowed {
		fmt.Fprintf(s.stderr, "  note: %s still holds a %s token that nothing will read now.\n", path, r.name)
	}
	if len(shadowed) > 0 {
		fmt.Fprintln(s.stderr, "  Remove each, and revoke the token in Slack unless it is this one.")
	}
}

// declined reports a prompt that ended without an answer — Ctrl-C, Ctrl-D, or
// the input running out. Choosing to stop is not a failure, so it is ExitOK; a
// terminal that could not be read at all is.
func (s *setup) declined(err error) int {
	if errIsCancelled(err) {
		fmt.Fprintln(s.stderr, "  cancelled, nothing stored")
		return cli.ExitOK
	}
	fmt.Fprintf(s.stderr, "  could not read the answer: %v\n", err)
	return cli.ExitFailure
}

// affirmative matches "y", "yes", "Yep". Anything else, including an empty
// line, is no: the default has to be the one that stores nothing.
func affirmative(answer string) bool {
	answer = strings.TrimSpace(answer)
	return answer != "" && (answer[0] == 'y' || answer[0] == 'Y')
}

// A role is one credential: what to call it, where it is stored, and whether
// its token is required to carry chat:write.
//
// The service names, file names and account key restate internal/cred's, which
// does not export them. Enrolment and resolution have to agree exactly or a
// stored credential is invisible to the commands, so what holds them together
// is a round-trip test — TestStoredCredentialIsResolvableByCred — rather than a
// shared constant.
type role struct {
	name string
	// label names the role in a prompt, alongside the commands that use it.
	label string
	// envVar outranks everything stored, so enrolment has to know it exists.
	envVar string
	// service is the keyring service name.
	service string
	// fileName is the same under both config directories.
	fileName string
	// needsWriteScope is what the scope check compares against: true for the
	// role that must be able to post, false for the one that must not.
	needsWriteScope bool
}

var (
	readRole = role{
		name:     "read",
		label:    "READ (hear)",
		envVar:   "SLACK_READ_TOKEN",
		service:  "hersaid-read",
		fileName: "read.token",
	}
	writeRole = role{
		name:            "write",
		label:           "WRITE (sez, tap)",
		envVar:          "SLACK_WRITE_TOKEN",
		service:         "hersaid-write",
		fileName:        "write.token",
		needsWriteScope: true,
	}
)

const (
	// userPrefix is a user token, which acts as the person who authorised it.
	userPrefix = "xoxp-"
	// botPrefix is a bot token, which acts as the app.
	botPrefix = "xoxb-"
)

const botPrefixRefusal = `  REFUSED: that is a BOT token (xoxb-). It would post as an app, not as you.
  Use the User OAuth Token (xoxp-...).
`

// minFingerprint is the length below which only a length is reported. The
// windows below are 9 and 4 characters, so a shorter string would be printed
// almost in full — and a Slack user token is 50 characters and up, so anything
// this short is already the wrong secret rather than a truncated right one.
const minFingerprint = 20

// fingerprint is what makes a truncated or half-selected paste visible without
// echoing the secret: the leading bytes are a token type and a workspace prefix,
// which are not the credential, and the length is what actually catches the
// mistake.
func fingerprint(token string) string {
	r := []rune(token)
	if len(r) < minFingerprint {
		return fmt.Sprintf("(%d chars)", len(r))
	}
	return fmt.Sprintf("%s...%s (%d chars)", string(r[:9]), string(r[len(r)-4:]), len(r))
}

// strip removes every whitespace character, matching what cred.strip does to
// the token it reads back — the two have to agree, or a token stored here is
// not the token resolved there. A token contains no whitespace, and a trailing
// newline from a paste is the common way one arrives with some; trimming only
// the ends would leave an embedded newline in place.
func strip(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
