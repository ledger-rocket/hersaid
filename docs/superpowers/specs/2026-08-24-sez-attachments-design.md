# `sez --file` — attach files to a message

## Goal

`sez` can say anything and carry nothing. A trace, a screenshot or a log has to
leave the terminal, go through a browser and come back into the same
conversation the message was already addressed to.

`sez --file PATH` attaches files to the message being sent. It is the same
command, the same gate and the same audit log: the render still shows what will
go out, `--send` is still the only thing that acts, and the attempt is still one
line per phase in `posts.jsonl`.

## What Slack actually offers

`files.upload` is retired. Uploading is three legs:

1. `files.getUploadURLExternal` — `filename` and `length` in, an `upload_url`
   and a `file_id` back.
2. A POST of the bytes to that `upload_url`. It is not an API call: no envelope,
   no form, and **the URL carries its own credential in its query string**.
3. `files.completeUploadExternal` — the file ids, `channel_id`, `thread_ts` and
   `initial_comment` in; this is the leg that makes anything visible.

Two consequences run through the whole design.

**A message with a file is not a `chat.postMessage`.** The text becomes
`initial_comment` on leg 3. So `--file` substitutes the send endpoint rather
than adding to it.

**Nothing is visible until leg 3.** Legs 1 and 2 stage a file nobody can see.
That is what makes the all-or-nothing rule below cheap rather than aspirational.

## CLI surface

```
sez [--send] [--thread TS] [--md] [--file PATH]... <#chan|@person|ID> [text]
```

- `--file PATH` is repeatable and order is preserved into the message.
- `--thread` and `--md` compose. Both are arguments of leg 3 — a thread parent
  and a mrkdwn-converted comment.
- `--file` with `--edit` or `--delete` is a usage error, and not by policy:
  Slack offers no way to attach a file to a message that already exists.

### Text, and what stdin means

Today, omitting the text argument means "read stdin to EOF"; `-` means the same
thing explicitly. Making text optional under `--file` collides with that: a bare
`sez --file trace.pdf '#eng'` would block on a terminal waiting for input nobody
is going to type.

So `--file` changes the rule, explicitly and in the usage text:

- with `--file` and no text argument — **no comment**, and stdin is not read
- with `--file` and `-` — the comment is read from stdin, as it always was
- with `--file` and a text argument — that argument is the comment
- without `--file` — unchanged in every respect, including the refusal of an
  empty message

A comment that is present but blank is refused under `--file` as it is without
it: `-` given deliberately and answered with whitespace is a mistake, not a
request for no comment. Requesting no comment is what omitting the argument
does.

## Pre-flight

### The target must already be a conversation

`sez` resolves with `resolve.ForPost`, which declares `OpensDM` and
`PassThroughName` because `chat.postMessage` accepts a `#name` it resolves
itself and a `U…` id whose DM it opens. `files.completeUploadExternal` does
neither: `channel_id` is a conversation id. An unresolved `#eng` or a bare `U…`
would therefore stage every byte and fail on the last leg.

So uploads resolve with a new `resolve.ForUpload`, both capabilities false —
the same shape as `ForReact` and `ForEdit`, under its own name because it is its
own reason. A `#name` the cache cannot resolve, or a person with no open DM,
fails before leg 1, naming what to do: `hear dms` fills the cache, and
`ErrNoOpenDM` already says "open one in Slack first".

`@handle` needs nothing new. `resolvePerson` looks an IM up by user id and
returns its `D…` whatever the capability says, which is how `tap` addresses a
person today.

A **bare `U…` id does**, and fixing it is part of this change. `resolveChannel`
admits a `U…` as an id only when the caller opens DMs; for everyone else it
falls through to matching channel names, which a user id never does, and reports
`no such channel: U0123ABCD` — even when that person's DM is sitting in the
cache. So a bare `U…` under a capability that does not open DMs now looks the
IM up by user, returns its `D…`, and reports `ErrNoOpenDM` when there is none.

That is shared code, so `tap` and `sez --edit` get the same fix, and both get
tests for it. It is in scope because the alternative is an upload advertising
`<#chan|@person|ID>` and rejecting one of the three with a message about
channels.

### Each file is opened and kept open

Every local check runs before any request is built, and each file is opened and
the descriptor held for the life of the send:

- the path opens, and `f.Stat()` on the descriptor says it is a regular file
- it is not empty. Slack rejects a zero-length upload and there is no reason to
  spend three round trips learning that
- it is at most **1,000,000,000 bytes**. Slack documents the limit as "1 GB"
  without saying which one; the decimal reading is the smaller, so it is the one
  that cannot let through a file Slack will refuse
- at most ten files, and no two descriptors that `os.SameFile` reports as the
  same file. That catches the same path twice and the same file reached by two
  paths, which a string comparison does not
- the mimetype is resolved once, from the extension via `mime.TypeByExtension`,
  falling back to `http.DetectContentType` over the first 512 bytes of the open
  descriptor (seeking back afterwards), falling back to
  `application/octet-stream`

Descriptors are held rather than reopened because `length` is declared on leg 1
and the bytes are sent on leg 2. Between a `stat` and a later `open`, a symlink
can be retargeted or a path replaced, and then the file the render described is
not the file that is sent.

**What that does and does not guarantee.** Holding the descriptor fixes the
*identity* of the file: no later lookup of the path can be steered elsewhere.
It does not freeze the *contents*. Another process can still truncate, extend or
overwrite the same inode while it is being read. A length change is caught —
leg 2 streams exactly the declared count and treats both a short read and an
unexpected extra byte as a failure — but a same-length overwrite is not, and
would be uploaded as a mix of old and new bytes.

Snapshotting each file to a private temporary before sending would close that,
at the cost of copying up to a gigabyte on every send. It is not worth it for a
race that needs another process on the sender's own machine writing to the exact
file being sent, so the guarantee is stated as it is: path identity, not content
immutability. Concurrent in-place modification of a file being sent is
unsupported.

A pre-flight refusal costs zero requests and stages nothing.

The filename given to Slack is `filepath.Base` of the path. Two files with the
same basename from different directories are allowed through; Slack shows both,
and inventing a disambiguation Slack does not ask for would be this tool
renaming someone's file.

### A glob that expands wrongly

`sez --file *.log #eng text` expands to `--file a.log b.log c.log #eng text`,
because `--file` takes one value. `b.log` then arrives where the channel is
expected and resolution fails on a conversation nobody has. The message that
failure prints names the glob as the likely cause when `--file` was given at
all: the alternative is a person reading "no such channel: b.log" and looking
for a channel.

## Classification: what each phase may claim

This is the core of the design, and it is **not** `slackwrite.Classify` reused
unchanged. That function sends `TransportError` and every unrecognised error to
`Ambiguous` through its default branch — the right default for
`chat.postMessage`, where one irreversible call either happened or did not.
Applied here it would report "delivery unknown" for a connection that failed
while asking for an upload URL, when no message could possibly exist yet.

`slackupload` classifies by **phase first, error second**:

- **Staging** — everything up to and including the last byte of leg 2. Every
  failure, of every kind, means *not posted*. This is true by construction: a
  staged file is not a message, and no `initial_comment` has been submitted
  anywhere. `sez` records it as `failed`, not `unknown`.
- **Completing** — from the moment the leg 3 request is issued. Here the
  existing philosophy applies and the safe direction is unknown: a transport
  error, a malformed reply, `fatal_error` and `internal_error` are all
  `Ambiguous`.

An API error from leg 3 is `Rejected` only where the code names something Slack
must have evaluated *before* sharing anything:

- `missing_scope` — the token carries no authority to share anything, so no
  file can have been
- `posting_to_channel_denied` — permission to post to the target is the
  operation; denied, it did not happen

`channels_limit_exceeded` and `invalid_blocks` are not on the list even though
they look like request validation, because this code sends one `channel_id` and
no blocks: neither can occur, and a set that lists unreachable codes invites the
next one to be added by analogy rather than by evidence.

Everything else is `Ambiguous`, and that deliberately includes `file_not_found`.
It says one upload ticket could not be found; in a multi-file request it says
nothing about whether the other files were shared first. Slack does not document
leg 3 as transactional across the `files` array, so "the API returned an error,
therefore nothing was shared" is not a claim this design is entitled to make in
general. A table-driven test carries the reason for each code, so adding one
later means writing down the evidence.

The payoff is real: in a plain `sez` send the ambiguous window is the whole
request. Here it is leg 3 alone, and everything before it — which is where the
bytes, the time and most of the failure modes are — reports the truth.

## Interruption

`sez` gives its signal handler an immutable snapshot of the record, so that the
handler never races the outcome path. A multi-file upload cannot use that
unchanged: the snapshot is taken before the first request and can neither gain
the ids of files that staged nor know whether leg 3 has begun.

So the upload path carries one mutex, guarding one value:

```go
type progress struct {
	phase   phase    // staging, completing, decided
	staged  []string // file ids, appended as each transfer finishes
	outcome *audit.Post
	written bool
}
```

`Stage` reports each file as it finishes leg 2, so `staged` grows during the
send rather than only at the end.

The protocol is that **both writers take the same lock, and `written` decides
who writes**. The normal path locks, appends the outcome, sets `written`, and
unlocks; the handler locks, returns immediately if `written` is set, and
otherwise writes the record its phase implies and sets `written` itself. Only
one outcome line can therefore be appended, whichever of the two gets there
first. `Log.Post` is already one unbuffered `write(2)` followed by `Sync`, so
the handler does not close the log to make its record durable — closing stays
with the normal path's `defer`, and nothing closes the file underneath a writer
holding the lock.

The phases and what each records:

- `staging` — `failed`. Nothing was posted, and the staged ids go in the record.
- `completing` — `unknown`. The leg 3 response has not been read; check Slack
  before retrying.
- `decided` — the outcome Slack actually returned. Entered the moment the leg 3
  reply is parsed and classified, which is *before* the permalink lookup. That
  lookup is a whole round trip, and a signal during it must not turn a delivery
  Slack confirmed into "unknown"; it costs the permalink, not the truth.

The same argument applies to a plain `sez` send, which today records `unknown`
for a signal during its permalink fetch. That is a separate change and is not
made here.

## Staged files

Bytes that reached leg 2 but were never completed are staged uploads. Their ids
are named in the record so an interrupted send can be reconciled — but only for
an interruption this process survives long enough to write about. After SIGKILL
the intent line is what remains, and it carries the paths, names and sizes that
were about to go out and no ids at all.

That is accepted rather than fixed. Checkpointing each id to the log before its
transfer would make SIGKILL reconcilable, at a line per file on every send; what
it would reconcile is a set of files nobody can see, which Slack's own upload
documentation says are discarded when completion never comes. The cost is
certain and the benefit is close to nothing.

What Slack does with them is not otherwise asserted here: no retention window is
documented, and no observation has tested one. So the record calls them staged
rather than orphaned, and the spec promises no cleanup.

`sez` does not delete them. `files.delete` would in fact be available — it takes
the same `files:write` this feature already needs, not a separate scope — but
destroying something automatically in response to a failure is the wrong
instinct in a tool whose whole shape is "show it first", and it would mean a
second irreversible call on the failure path of the first.

## Never logging the upload URL

The `upload_url` from leg 1 is a presigned URL: its query string is a
credential. Go returns transport failures as `*url.Error`, whose `Error()`
contains the full URL, and `sez` writes `err.Error()` into `posts.jsonl` and to
stderr. Wrapping a leg 2 failure the ordinary way would therefore persist that
credential to disk.

So **every leg 2 error passes through one sanitiser**, and it is the only thing
permitted to build them. Leg 2 is the whole of the sanitiser's scope: it begins
when leg 1 hands back an `upload_url` and ends when the transfer does, and it is
the only stretch in which any code here holds that URL.

Leg 3 errors are ordinary `slackwrite` errors and stay typed. They must, because
the classification above reads the method and the code off them, and a sanitiser
that rebuilt them as filename-and-class would erase exactly what tells
`missing_scope` from `file_not_found`. They never carry the upload URL: leg 3
is a form POST to the API that has never been given one. They carry the filename, the phase, the host, and
a class — `timeout`, `dns`, `tls`, `refused`, `http 413` — and nothing else. The
URL never reaches an error string, a log line, the audit record, or stdout.

One function rather than a rule about `%w`, because the transport is not the
only thing that can quote a URL back. `url.Parse` puts the whole string in its
error; so does `http.NewRequest`. Those run before any connection is attempted,
so a rule covering only network failures would leave the earliest failures — a
malformed URL, an invalid port, an unsupported scheme — as the ones that leak.

A test pins this with a canary: the fake upload URL carries a sentinel query
parameter, and every failure mode is asserted to leave that sentinel absent from
stdout, stderr, every audit line, and the error's own `Error()` string. The
matrix is DNS failure, TLS failure, timeout, a refused redirect, a non-2xx
status, a malformed URL, an invalid port, an unsupported scheme, a foreign host,
and a request that fails to construct.

## Where the code lives

`internal/slackupload`, a new package holding the flow and taking `cred.Write`.

The two API legs go into `slackwrite` as typed methods — `GetUploadURL` and
`CompleteUpload` — because they are exactly what that package already
guarantees: a request built from endpoint-specific arguments, never a
caller-supplied `url.Values`, answered by an envelope.

Leg 2 is the one that does not fit, and it is why there is a new package at all.
A streaming body to a host that is not the API, with no envelope to check and no
credential to attach, is the same mismatch that put `slackfiles` outside
`slackread` instead of inside it.

```go
func (c *Client) Stage(ctx context.Context, files []Local, done func(Staged)) ([]Staged, error)
func (c *Client) Share(ctx context.Context, r ShareRequest, at ShareBoundary) (Shared, error)

type ShareBoundary struct {
	Issued  func()        // the leg 3 request is on the wire
	Decided func(Outcome) // Slack's reply has been read and classified
}
```

`done` is what makes the signal handler's progress value true rather than
decorative: it fires as each file finishes leg 2, so the ids exist in `sez`
while the send is still running instead of only when `Stage` returns. A callback
rather than splitting ticket allocation from transfer, because the two legs are
one operation per file and nothing between them is a decision `sez` would make
differently.

`ShareBoundary` exists because the phases the signal protocol names happen
inside `Share`, not at its edges. Without them `sez` could only enter
`completing` before the call and `decided` after it returns, so a signal
arriving between the reply being classified and the function returning would
record `unknown` for an outcome already known. That window is a function return
rather than the round trip the permalink lookup costs, but closing it is two
callbacks, and a guarantee with an unstated exception is worse than a smaller
guarantee.

The `Stage`/`Share` split is not cosmetic. `sez` owns the ordering — the intent
record before anything leaves, the progress value the handler reads, the outcome
record after — and `Share` runs only when `Stage` returned for every file. A
single `Post(files...)` would put those decisions inside a package that cannot
write the audit log.

`cmd/sez` gains an upload path beside `post` and `change`, mirroring their
shape: intent, a signal-safe outcome, one attempt, a classified result.

### Two extractions

Both are forced by rules the repo already enforces, not by taste.

`slackfiles.Hosts` — the two-tier bearer/redirect allowlist — becomes
`internal/slackhosts`, unchanged, and both the download and the upload use it.
The upload URL is the same problem in reverse: a Slack-supplied URL carrying its
own credential, which must never see the bearer. Two copies of that policy is
how they drift.

`render.HumanSize` becomes `internal/humansize`. `cmd/sez` cannot import
`internal/render`, which links `slackread`, and an import-graph test holds it to
that. `hear files` and `sez` print the same sizes, and a second formatter beside
the first would eventually print them differently.

## Host policy and the transfer bound

`slackupload` checks the `upload_url` before building a request: HTTPS, and a
host on the bearer tier — even though it will not be given a bearer, because a
URL Slack did not serve is not one to send a file to either. `Authorize` is
never called on leg 2, and a test observes the sink receiving no `Authorization`
header. That test is mutation-checked, as the download's version of it was.

Redirects on leg 2 are refused. A real upload took no hop, so nothing here has
been observed to need one. There is no evidence Slack redirects an upload,
and the download's allowlist exists because redirects were *observed* to matter
there. If a live upload proves otherwise it loosens under the same two-tier
rule, not by switching to a client that follows anything.

The transfer bound is size-aware. A flat ten times `HERSAID_TIMEOUT` is 200
seconds by default, which moves the largest permitted file only above about
40 Mbit/s — so the flat factor would fail large uploads on size alone over a
link that is working, which is exactly what it was supposed to prevent. Instead:

```
deadline = max(10 × HERSAID_TIMEOUT, HERSAID_TIMEOUT + size / minRate)
```

with `minRate` at 1,000,000 bytes a second, an assumed floor slow enough to be
nearly always true, giving about seventeen minutes for the largest permitted
file. The addition, the multiplication and the rounding-up division are all
checked, and the assumed floor is stated in the README so a person on a slow
link knows which number to raise.

Checked arithmetic here is not enough on its own, because the input can already
be nonsense. `TimeoutFromEnv` multiplies an `Atoi` result by `time.Second` and
guards only against zero and negatives, so `HERSAID_TIMEOUT` set high enough
overflows into a negative duration — and a negative timeout means every request
fails instantly. That is a bug in shipped code.

Capping the parser at `math.MaxInt64 / int64(time.Second)` would fix only the
conversion. Every consumer multiplies again: `hear` takes ten times it for a
download, `sez` takes twice it for its `hear` subprocess, and this formula takes
ten times it as a floor. A cap that is safe for the conversion still overflows
all three.

So the parser caps `HERSAID_TIMEOUT` at **86400 seconds**, one day, and says so
in the refusal. Every multiplier in the codebase is then trivially safe with
room to spare, and a per-request timeout beyond a day is not a configuration
anyone means — where the intent is "wait a long time for a big upload", the
size-aware deadline above is what already provides it.

## The dry run

```
--- dry run: nothing sent ---
channel : #eng -> C123
thread  : 1785708834.862009
files   : /Users/you/Projects/hersaid/trace.pdf
            1.2 MB  application/pdf
          /Users/you/out.log
            14.0 KB  text/plain
--- message as Slack will receive it ---
here's the repro
----------------------------------------
```

Paths are absolute. That is the whole point of the section: a basename cannot
distinguish the `report.pdf` in this directory from the one in the last, and a
path is not something a reader can proofread the way they proofread a sentence.

A path is displayed escaped, not raw. A Unix filename may contain a newline, an
ESC or invalid UTF-8, so a glob can expand into something that restructures the
render or drives the terminal — and the render is what the sender reads before
authorising a send, which makes it exactly the wrong surface to be forgeable.
`render.FileLabel` already takes this line for names Slack supplies; a name the
shell supplies is no more trustworthy for the purpose.

**One function does it, and everything that prints a path calls it**: the dry
run, every pre-flight refusal, every sanitised transfer error, and the glob
hint. Escaping only the render would leave the same escape sequence a clear path
to the terminal through the error that rejected it — and a hostile name is far
likelier to appear in a refusal than in a successful render. The escaping is
display-only; the filesystem operations use the raw bytes.

With files and no comment the message section reads `(no comment; the files are
the message)`, so the render never leaves a reader guessing whether the text
went missing or was never given.

## Audit record

- A new action, `message.upload`.
- `postWire` gains one key, `files`, present on every post record and `null`
  when there are none. The field set is deliberately fixed, as `previous_text`
  already is.

```json
"files": [
  {
    "path": "/Users/you/Projects/hersaid/trace.pdf",
    "name": "trace.pdf",
    "size": 1258291,
    "mimetype": "application/pdf",
    "id": "F07ABC123"
  }
]
```

Objects rather than bare ids, because the intent line is written before any id
exists and must still say what was about to go out; `id` is `null` there and
fills in as files stage. `path` is carried because the render's whole argument
is that the absolute path is the identity of what is being sent, and the record
should name what the render showed.

`path` holds the raw bytes only insofar as they are valid UTF-8. A Unix path is
a byte string and a JSON string is Unicode, so `encoding/json` — which is what
writes every record here — replaces an invalid sequence with U+FFFD. That is
accepted rather than worked around: a second base64 field on every file of every
record, to represent a filename nobody can type, costs more than it returns. The
loss is documented here and in the test, which asserts the replacement rather
than pretending the bytes survive. `mimetype` is carried because the render shows
it and the record should say what the render claimed.

The mimetype is a statement about the transfer, not a field Slack is told to
honour: `files.getUploadURLExternal` takes only `filename` and `length`, and
Slack infers the type from the name. It is what `sez` sets as `Content-Type` on
leg 2, and the render and the record say so rather than implying Slack was
asked for it.

The `upload_url` is never recorded, in any phase, for the reason given above.

### `ts` and the permalink

`files.completeUploadExternal` returns file objects carrying `shares`, and a
share for the target channel carries the message `ts`. When it is present,
`Share` returns it, `sez` fills `rec.TS` and fetches `chat.getPermalink` exactly
as a send does, and an upload is as reconcilable as any other message.

A file object splits shares into `shares.public` and `shares.private`, keyed by
channel. **Both maps are read**: which one a share lands in depends on the
channel, not on anything this code chose, and reading only `public` would lose
the timestamp for every private channel and DM — which is most of what this tool
posts to.

Every timestamp found for the target channel, across both maps and across every
returned file, must agree. One value is then used; differing values mean the
reply does not describe a single message, and that makes the completion
`Ambiguous` rather than a success reported against an arbitrary one of them.

**Observed, once.** A real upload to a DM returned no `shares` at all: the
message existed at `1787666090.753389`, and the reply named only the file id.
So the fallback below is the path that actually runs, and the consensus rule
above did not fire. The method reference page, which documents `{id, title}`,
matches reality here; the upload guide's fuller example does not. One
observation, of one DM — a public channel may differ, and reading both maps
costs nothing when they are absent.

The response shape is treated as something to read rather than to rely on. When
no share timestamp for the target channel is present at all, `ts` and
`permalink` stay `null` and the file ids are the anchor — a file id finds the file in Slack,
which is what reconciling the log against reality needs. Both shapes are tested.

Reconciliation is set equality, in both directions and without duplicates: the
ids returned must be exactly the ids requested, each once. A reply that omits
one, repeats one, or names one that was never staged is an error rather than a
success with a quietly different list. An empty share timestamp counts as
absent, never as a value to agree with.

Leg 1 is checked the same way. An `ok: true` carrying an empty `file_id` or an
empty `upload_url` satisfies the envelope and nothing else; it is a malformed
staging reply, refused before any bytes move, rather than a confusing failure
two legs later.

## Scope

The write app needs `files:write` under User Token Scopes.

`hersaid-setup`'s `checkScopes` gates on `chat:write` alone and stays that way,
so every enrolment that exists keeps working. A token without `files:write` is
diagnosed at send time by `missing_scope`, exactly as `reactions:write` is.

That obliges two fixes to shipped text. The `missingScope` help hardcodes "The
write app needs chat:write", which would send someone to re-grant a scope they
already hold. `slackwrite` gains a method-to-scope table and the help names the
scope the failing endpoint needs. The table is used rather than the `needed`
field Slack returns, because a value from a response should not be interpolated
into instructions telling somebody what to grant.

The `noCredential` help has the same shape of problem one step earlier: it tells
anyone with no write credential at all to build an app carrying `chat:write`,
which for an upload is half the answer. It names the scopes the attempted action
needs. The README's write-app scope list gains `files:write` beside it.

## Failure

All or nothing. Any failure before leg 3 aborts the send: nothing is posted,
exit 1, and the record names the file that failed and the ids of anything
already staged.

The reasoning is that this is an outward message. `hear files` is best-effort
because it writes to the sender's own disk and a partial result is still a
result. A message going out under someone's name, referring to an attachment
that is not there, is worse than no message.

Leg 3 is classified by the phase rules above: a refusal Slack's contract proves
is `failed`, everything else is recorded `unknown` and never retried
automatically, because a retry duplicates a message that may well have been
delivered.

## Testing

`slacktest` grows the two API methods and a transfer sink that records the
headers, the byte count and the body it received.

**The successful wire contract**, which a failure-only suite would never pin:

- one end-to-end multi-file success: the exact JSON encoding of the `files`
  argument, file order preserved, the full set of form fields on both API legs,
  `thread_ts` and `initial_comment` present when given and absent when not
- the sink receives exactly the declared byte count, and the bytes match
- the completion reply is read under a bound, and reconciled against the ids
  that were requested
- reply shapes: a `ts` under `shares.public`, a `ts` under `shares.private`, a
  reply carrying neither, and one whose files disagree about the timestamp —
  which is `unknown`, not a success
- no `chat.postMessage` request is made on an upload — asserted on the fake
  server, not inferred

**Security:**

- leg 2 carries no `Authorization` — mutation-checked
- the presigned-URL canary is absent from stdout, stderr, every audit line and
  every error string, across DNS, TLS, timeout, refused-redirect, non-2xx,
  malformed-URL, invalid-port, unsupported-scheme, foreign-host and
  request-construction failures
- an `upload_url` on a foreign host, or on plain HTTP, is refused before a
  request is built

**Semantics:**

- file two of three fails: no share is attempted, exit 1, staged ids named
- a staging transport failure records `failed`, never `unknown`
- a leg 3 transport failure records `unknown`, never `failed`
- the classification table: every code listed as a proven refusal records
  `failed`, and every other code — `file_not_found` among them — records
  `unknown`
- signals during each API leg, during a byte transfer, at each `ShareBoundary`
  callback, and during the permalink lookup, which must record the known outcome
  rather than `unknown`
- only ever one outcome line: a signal delivered while the normal path is
  writing must not append a second, exercised under the race detector
- the file is replaced or truncated after pre-flight: the held descriptor means
  the declared length and the sent bytes still agree, or the send fails. A
  same-length overwrite is documented as unsupported and is not tested as
  though it were caught
- resolution: a `#name` the cache cannot resolve, a bare `U…` with no open DM,
  an `@handle` with no open DM, and a cold cache each fail before leg 1 and cost
  zero requests; an `@handle` with an open DM, a bare `U…` with an open DM, a
  private channel and an MPIM each resolve to an id leg 3 accepts. The bare-`U…`
  cases are asserted for `tap` and `sez --edit` too, since the fix is shared
- pre-flight refusals cost zero requests: missing path, directory, empty file,
  oversize, eleven files, the same file twice by two paths
- a display path carrying a newline, an ESC or invalid UTF-8 cannot restructure
  the render, while the audit record and the filesystem still see the raw bytes
- `HERSAID_TIMEOUT` at the cap and one above it, and every consumer of the
  result — the download's ten times, the `hear` subprocess's twice, the upload
  deadline's ten times — still positive at the cap
- a leg 1 reply with `ok: true` and an empty `file_id`, and one with an empty
  `upload_url`, are refused before any transfer
- reconciliation: a reply omitting an id, repeating one, and naming an unstaged
  one are each an error
- a path that is not valid UTF-8 is recorded with the replacement character, and
  the test asserts that rather than asserting the bytes survive
- a filename carrying ESC or a newline is escaped in the dry run, in the
  pre-flight refusal that names it, in a sanitised transfer error, and in the
  glob hint
- `--file` with no text sends no comment and does not read stdin; `-` reads it;
  whitespace-only input is refused
- `--file` with `--edit` or `--delete` is refused
- `--md` converts the comment and nothing else
- the scope help names `files:write` for an upload and `chat:write` for a send
- `cmd/sez` still does not import `slackread`, and `cmd/hear` still reaches no
  write endpoint
