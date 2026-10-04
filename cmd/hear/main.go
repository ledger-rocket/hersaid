// Command hear reads Slack. It imports slackread and never slackwrite, so no
// write endpoint is in this binary's import graph — verified by
// TestImportGraphExcludesWriteCode and by the artifact scan beside it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ledger-rocket/hersaid/internal/cache"
	"github.com/ledger-rocket/hersaid/internal/cli"
	"github.com/ledger-rocket/hersaid/internal/cred"
	"github.com/ledger-rocket/hersaid/internal/resolve"
	"github.com/ledger-rocket/hersaid/internal/slackfiles"
	"github.com/ledger-rocket/hersaid/internal/slackhosts"
	"github.com/ledger-rocket/hersaid/internal/slackhttp"
	"github.com/ledger-rocket/hersaid/internal/slackread"
)

const name = "hear"

const usage = `usage: hear <command> [options]

  Reads Slack with the read credential. This binary contains no write
  endpoint at all, so no argument to it can post, react or delete.

  Commands:
    whoami                        confirm which account the read token belongs to
    channels [filter]             list channels you can read
    dms [filter]                  list people you have a 1:1 DM with
    read <address> [n]            last n messages (default 20)
    thread <address> <ts>         replies in a thread
    files <address> <ts>          download the files attached to one message
    canvas <link|F…|#chan>        print a canvas as Markdown
    search <query...>             search messages across the workspace
    refresh                       clear the channel/user cache

  An address is #channel, @person, a Slack id, or a message permalink —
  https://…/archives/C0123ABCD/p1785708834862009 — which carries its own
  channel and timestamp, so a link someone pasted can be read as it was given.
  A filter is a case-insensitive substring; matching nothing is an empty
  result, not an error.

  Options:
        --ts          prefix each message with its timestamp, which is what
                      sez --thread and tap take; accepted in any position
        --json        write one structured JSON object instead of a transcript
        --html        canvas: print the HTML Slack served, not Markdown
        --since D     read a window rather than a count: 30m, 2h, 7d, 1w.
                      Pages to the end of the window, or to the newest 5000
                      messages in it, whichever comes first — --json reports
                      which by "complete". Read only, and not combinable with n.
        --out DIR     where files writes; the message's own directory is made
                      inside it. Default: the current directory
        --max-size N  largest file to fetch, e.g. 512K, 100M, 2G. Default 100M.
                      A file above it is skipped and named, never truncated
        --            stop reading options; what follows is literal
    -h, --help        print this help and exit 0
        --version     print the version and exit 0

  Environment:
    SLACK_READ_TOKEN   read credential; set but empty means "absent"
    HERSAID_TIMEOUT    request timeout in whole seconds (default 20)

  Credential sources, first hit wins: $SLACK_READ_TOKEN, keychain service
  hersaid-read, then ~/.config/hersaid/read.token (mode 0600). A command
  answered entirely from the cache — channels, dms, refresh — resolves no
  credential at all.

  files writes each attachment into <out>/<channel>-<ts>/, refusing to overwrite
  and never taking a name Slack sent as a path. A file with no bytes behind it —
  a link to storage elsewhere, a deleted one — is skipped and said so. Exit is 1
  when any file was skipped or failed, whatever else was written. Needs the
  files:read scope on the read app.

  JSON: one object, tagged with "kind": identity, channels, dms, transcript,
  search, files or refresh. A transcript carries "channel", "messages" and
  "complete"; "complete": false means Slack held more messages than were read.
  Each message has ts, time (RFC3339), author, user, text, and text_rendered —
  the same text with <@U…> mentions resolved. Diagnostics stay on stderr, so
  stdout is parseable whatever happens.

  Exit: 0 fine, 1 refused or failed, 2 usage.
`

const noCredential = `hear: no read token found. Provide one of:
  $SLACK_READ_TOKEN
  keychain:  security add-generic-password -a "$USER" -s hersaid-read -w
  file:      ~/.config/hersaid/read.token (mode 0600)
  or run: hersaid-setup
`

// defaultCount bounds a read that names no count.
const defaultCount = 20

// searchCount is fixed rather than a flag because
// search results are ranked rather than chronological, so a bigger number
// answers a different question than reading more of a channel does.
const searchCount = 20

// maxLimit is the largest limit conversations.history accepts; it rejects 1000.
const maxLimit = 999

// pageSize is what a walk asks for per page, matching slackread's own paging.
const pageSize = 200

// maxPages bounds every walk. A server that returns the same non-empty cursor
// forever — broken, hostile, or something in between — would otherwise spin
// here until the process is killed.
const maxPages = 1000

// downloadTimeoutFactor is how much longer a file may take than an API call.
// HERSAID_TIMEOUT bounds a request that returns a small JSON document; a body
// of up to --max-size held to the same bound would fail on size alone, on a
// link that is working perfectly well.
const downloadTimeoutFactor = 10

// maxWindow bounds what --since may collect. Unlike a count, a duration says
// nothing about how much there is: a busy channel and a wide window can name
// far more than a transcript is any use as, and all of it is held in memory
// before a single line is printed. Hitting it is reported — "complete": false —
// rather than silently truncating, which is the thing the cursor was followed
// to avoid in the first place.
const maxWindow = 5000

// resolveRead is a variable so a test can observe whether the credential was
// consulted at all. Asking for help or for a version must never reach a
// keychain: on macOS that is a modal unlock prompt, and on Linux a D-Bus call
// that hangs where there is no Secret Service.
var resolveRead = cred.ResolveRead

// endpoints is everywhere a command may send the read credential: the API, and
// the hosts a file may be downloaded from. The two travel together so that
// naming one cannot leave the other to a default, which is the same reason
// slackread.SlackAPI is a parameter rather than a package default.
type endpoints struct {
	api   string
	files slackhosts.Hosts
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, endpoints{
		api: slackread.SlackAPI, files: slackfiles.SlackHosts,
	}))
}

func run(args []string, stdout, stderr io.Writer, where endpoints) int {
	// First, and with no side effects: --help and --version are answered here,
	// before the filesystem, the cache or any credential. They are handled by
	// the parser rather than by cli.Intercept because hear has options of its
	// own now, and only a parser knows that in `--since --help` the second word
	// is a duration.
	f, done, code := parseArgs(args, stdout, stderr)
	if done {
		return code
	}
	if len(f.rest) == 0 {
		return cli.Usage(stderr, name, usage, "")
	}

	verb, rest := f.rest[0], f.rest[1:]
	if f.since != 0 && verb != "read" {
		return cli.Usage(stderr, name, usage, "--since applies to read")
	}
	if (f.out != "" || f.maxSize != 0) && verb != "files" {
		return cli.Usage(stderr, name, usage, "--out and --max-size apply to files")
	}
	// Gated like every other verb-specific flag: a flag accepted and silently
	// dropped teaches that it did something.
	if f.canvasHTML && verb != "canvas" {
		return cli.Usage(stderr, name, usage, "--html applies to canvas")
	}

	// The client is built on first use rather than up front, so the commands
	// the cache can answer by itself — channels, dms with a warm cache, and
	// refresh — never resolve a credential, and a warm cache makes no call.
	src := &source{where: where}
	h := &hear{
		flags:  f,
		store:  cache.Filling(src),
		src:    src,
		stdout: stdout,
		stderr: stderr,
	}

	switch verb {
	case "whoami":
		if len(rest) > 0 {
			// Arity-strict, and that is what keeps "asking what a tool does
			// never prompts for a credential" true one level down from the
			// shared flags: a verb that ignored its trailing arguments would
			// answer `hear whoami --version` by opening the keychain.
			return cli.Usage(stderr, name, usage, "whoami takes no arguments")
		}
		return h.whoami()
	case "channels":
		filter, err := optionalFilter(rest)
		if err != nil {
			return cli.Usage(stderr, name, usage, err.Error())
		}
		return h.channels(filter)
	case "dms":
		filter, err := optionalFilter(rest)
		if err != nil {
			return cli.Usage(stderr, name, usage, err.Error())
		}
		return h.dms(filter)
	case "read":
		return h.read(rest)
	case "thread":
		return h.thread(rest)
	case "files":
		return h.files(rest)
	case "canvas":
		return h.canvas(rest)
	case "search":
		if len(rest) == 0 {
			return cli.Usage(stderr, name, usage, "search needs a query")
		}
		return h.search(strings.Join(rest, " "))
	case "refresh":
		if len(rest) > 0 {
			return cli.Usage(stderr, name, usage, "refresh takes no arguments")
		}
		return h.refresh()
	default:
		return cli.Usage(stderr, name, usage, "unknown command: "+verb)
	}
}

// hear is one invocation: what was asked for, where the answer goes, and the
// lazily-built way to reach Slack.
type hear struct {
	flags

	store  *cache.Store
	src    *source
	stdout io.Writer
	stderr io.Writer
}

func optionalFilter(rest []string) (string, error) {
	switch len(rest) {
	case 0:
		return "", nil
	case 1:
		return rest[0], nil
	default:
		return "", fmt.Errorf("one filter at most, got %d", len(rest))
	}
}

// fail prints err and returns the failure status. A missing credential is
// answered with the enrolment instructions rather than the bare error, because
// it is the one failure a first-time user is guaranteed to hit.
func (h *hear) fail(err error) int {
	if errors.Is(err, cred.ErrNotFound) {
		fmt.Fprint(h.stderr, noCredential)
		return cli.ExitFailure
	}
	fmt.Fprintf(h.stderr, "hear: %v\n", err)
	return cli.ExitFailure
}

// source builds the read client the first time something needs the network.
//
// It is memoised, so an invocation resolves the credential at most once: on a
// locked keychain a second resolution is a second modal prompt for one command.
type source struct {
	where  endpoints
	client *slackread.Client
	files  *slackfiles.Client
	// key identifies the credential the client was built from, so a fact
	// cached under a different one can be told apart from a current fact.
	key  string
	err  error
	done bool
}

// ConversationsList and UsersList make source a cache.Source, so a cold cache
// fills itself from Slack at the point of use and a warm one costs nothing.
func (s *source) ConversationsList(ctx context.Context) ([]cache.Conversation, error) {
	c, err := s.get()
	if err != nil {
		return nil, err
	}
	return c.ConversationsList(ctx)
}

func (s *source) UsersList(ctx context.Context) ([]cache.User, error) {
	c, err := s.get()
	if err != nil {
		return nil, err
	}
	return c.UsersList(ctx)
}

func (s *source) get() (*slackread.Client, error) {
	s.resolve()
	return s.client, s.err
}

// downloader is the file client, built from the same credential as the read
// client and at the same moment, so a command that reads a message and then
// fetches its attachments still resolves once.
func (s *source) downloader() (*slackfiles.Client, error) {
	s.resolve()
	return s.files, s.err
}

// credentialKey is the key of the credential already resolved, or empty when
// none has been. It never resolves one itself: a caller asking which credential
// is in use must not be the reason one gets loaded.
func (s *source) credentialKey() string { return s.key }

func (s *source) resolve() {
	if s.done {
		return
	}
	s.done = true
	c, err := resolveRead()
	if err != nil {
		s.err = err
		return
	}
	timeout, err := slackhttp.TimeoutFromEnv()
	if err != nil {
		s.err = err
		return
	}
	s.client = slackread.New(c, slackhttp.New(timeout), s.where.api)
	s.files = slackfiles.New(c, timeout*downloadTimeoutFactor, s.where.files)
	s.key = c.Key()
}

// address resolves target to the conversation to read.
//
// The refresh hint and the dms hint are appended here rather than in resolve,
// which has no business naming a command. They are the difference between an
// error a reader can act on and one they cannot.
func (h *hear) address(ctx context.Context, target string) (resolve.Target, error) {
	t, err := resolve.Resolve(ctx, h.store, resolve.ForRead, target)
	switch {
	case err == nil:
		return t, nil
	case errors.Is(err, resolve.ErrNoSuchChannel):
		return t, fmt.Errorf("%w  (try: hear refresh)", err)
	case errors.Is(err, resolve.ErrAmbiguousUser):
		return t, fmt.Errorf("%w (see: hear dms)", err)
	default:
		return t, err
	}
}

// flags is what the option scan sets.
type flags struct {
	showTS bool
	asJSON bool
	// canvasHTML prints a canvas as the HTML Slack served rather than as
	// Markdown, for a caller that would rather render it itself.
	canvasHTML bool
	// since is the width of the window to read, zero when unset.
	since time.Duration
	// out is the directory files writes into, empty when unset.
	out string
	// maxSize is the largest file to fetch in bytes, zero when unset.
	maxSize int64
	// rest is everything that was not an option, in order: the verb, then its
	// arguments.
	rest []string
}

// parseArgs reads the options, which are accepted in any position — `hear --ts
// read #eng` and `hear read #eng --ts` are the same command. done reports that
// there is nothing left to do:
// help or a version was printed, or the invocation was wrong.
//
// An unrecognised long option is refused rather than passed through as text. A
// single-dash word is not: Slack's search syntax spells exclusion "-term", so
// `hear search foo -bar` must reach Slack as written. Anything after "--" is
// literal, which is how a query that really does begin with two dashes is
// searched for.
func parseArgs(args []string, stdout, stderr io.Writer) (f flags, done bool, code int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			f.rest = append(f.rest, args[i+1:]...)
			return f, false, 0
		case a == "--ts":
			f.showTS = true
		case a == "--json":
			f.asJSON = true
		case a == "--html":
			f.canvasHTML = true
		case a == "--since" || strings.HasPrefix(a, "--since="):
			v, ok := strings.CutPrefix(a, "--since=")
			if !ok {
				if i+1 >= len(args) {
					return f, true, cli.Usage(stderr, name, usage, "--since needs a duration")
				}
				i++
				v = args[i]
			}
			d, err := parseSince(v)
			if err != nil {
				return f, true, cli.Usage(stderr, name, usage, err.Error())
			}
			f.since = d
		case a == "--out" || strings.HasPrefix(a, "--out="):
			v, ok := strings.CutPrefix(a, "--out=")
			if !ok {
				if i+1 >= len(args) {
					return f, true, cli.Usage(stderr, name, usage, "--out needs a directory")
				}
				i++
				v = args[i]
			}
			if v == "" {
				return f, true, cli.Usage(stderr, name, usage, "--out needs a directory")
			}
			f.out = v
		case a == "--max-size" || strings.HasPrefix(a, "--max-size="):
			v, ok := strings.CutPrefix(a, "--max-size=")
			if !ok {
				if i+1 >= len(args) {
					return f, true, cli.Usage(stderr, name, usage, "--max-size needs a size")
				}
				i++
				v = args[i]
			}
			n, err := parseSize(v)
			if err != nil {
				return f, true, cli.Usage(stderr, name, usage, err.Error())
			}
			f.maxSize = n
		// Answered only while they lead, which is what cli.Intercept does for a
		// command with no options of its own. Past the verb they are arguments
		// like any other, and the verb refuses them by arity: `hear whoami
		// --help` is a misuse, not a help screen.
		case a == "-h" || a == "--help":
			if len(f.rest) > 0 {
				f.rest = append(f.rest, a)
				continue
			}
			return f, true, cli.Help(stdout, usage)
		case a == "--version":
			if len(f.rest) > 0 {
				f.rest = append(f.rest, a)
				continue
			}
			return f, true, cli.Version(stdout, name)
		case strings.HasPrefix(a, "--"):
			return f, true, cli.Usage(stderr, name, usage, "unknown option: "+a)
		default:
			f.rest = append(f.rest, a)
		}
	}
	return f, false, 0
}
