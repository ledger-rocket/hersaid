# The safety model

Read this before wiring any of it into something that runs unattended,
especially an AI agent with shell access.

## Two Slack apps, two tokens

Reading and writing are different Slack apps with different tokens — this is
not optional. Slack issues one user token per app per user, and that token's
scopes are the union of everything configured on the app. There is no way to
get a read-scoped and a write-scoped token out of a single app: putting
`chat:write` on the read app would mean the "read-only" token could post.

A Slack token is a bearer credential: possession is capability, and nothing
obliges a caller to go through these commands. If one token carried both scope
sets, any process running as you could post with a single HTTP request and
never touch `sez`. **The scopes are the real separation**; the command and
package split guards against accident, not intent.

The separation keeps earning its keep after posting is enabled, because the
read path is the exposed one: `hear` ingests display names and message text
that any workspace member — including an external Slack Connect guest —
controls. That is where hostile input arrives, so it holds the token that
cannot write. It also means write access can be revoked without losing read.

## Credential resolution

Both roles resolve the same way, first hit wins:

1. `$SLACK_READ_TOKEN` / `$SLACK_WRITE_TOKEN` — presence, not non-emptiness
2. keychain service `hersaid-read` / `hersaid-write`, account `$USER`
3. `~/.config/hersaid/{read,write}.token`

Three tiers, in that order: the keychain is preferred over a file on disk, and
the file exists for hosts with no keyring at all. A resolution order short
enough to hold in your head is the point — every additional place a credential
can come from is another way the wrong account posts.

Every keychain error falls through to the next source, not only "not found": on
a headless Linux host there is no Secret Service, and the D-Bus failure is not
distinguishable from a genuine miss. The file fallback is a first-class path,
not a degraded one.

Whitespace is stripped from a token from every source, anywhere in it, not just
at the ends.

The service names are constants in the binaries, never read from the
environment. An override would let a caller quietly point the write gate at a
different credential and walk past it.

## `hear` cannot post

The token `hear` uses has no `chat:write` scope, and `hear` has no write verb:
no `post`, no `--send`, nothing that reaches `chat.postMessage`. The write
endpoints live in a separate package that `hear` does not import, so the code
path to post is not in that binary at all. Two tests hold it —
`TestImportGraphExcludesWriteCode` walks the import graph, and
`TestBuiltBinaryHasNoWriteEndpoints` scans the built artifact.

## The credential gate

`sez --send` and `tap` resolve the write credential before any client is
built, so a refusal cannot issue a request. If none resolves they print how to
create one and exit 1, and **no HTTP request is issued at all** — not one Slack
rejects, none.

Both resolve it on every run, which is how a render can say whether posting is
possible at all; only delivering *requires* one. Without `--send` an absent
credential is a line in the render and nothing more, so drafting works with all
five sources empty. (The flip side: on a locked macOS keychain even a render
can raise the unlock prompt.)

The gate is the token, not a policy setting, and the difference matters:

- A permission rule — an allowlist, a wrapper, a "don't call chat.postMessage"
  instruction — constrains *one command string*. Anything that can reach the
  network with the raw token posts right past it.
- An absent token constrains *the capability*. Nothing can post as you without
  it, whatever is making the call.

So the read-only period isn't "please don't post"; it's "posting is not
possible", which is a much stronger property to hand to something you don't
fully trust yet.

## Render-only is the default; delivering needs `--send`

**This is the single most important thing to know about `sez`.** It prints the
exact payload — resolved channel, resolved channel id, thread target, and the
message text after any `--md` conversion — and calls nothing, unless `--send`
is given. `--dry-run` and `-n` name that default explicitly. They are
last-wins against `--send`: `sez --send -n …` renders, `sez -n --send …`
sends.

This is a second, independent layer, not a restatement of the gate. The gate
governs *whether posting is possible at all*; the `--send` default governs
*whether this particular text was reviewed*. An instruction to "post X"
authorises the action, never the unseen wording — which matters most when an
agent composed the message and a human is approving a command rather than a
payload.

## The per-channel write allowlist

`HERSAID_WRITE_CHANNELS` restricts what `sez` and `tap` may touch:

```sh
export HERSAID_WRITE_CHANNELS='#general,C0123ABCD,D0456EFGH'
```

- Unset or empty means unrestricted. It is opt-in.
- The check runs **after** the channel is resolved — `#general` and its
  `C…` id are the same conversation — and **before any request**, so a refusal
  costs no network call and reveals nothing.
- Resolution here is against the local cache only, since `sez` and `tap` hold
  no read credential. With a cold cache a `#name` reaches the check unresolved,
  and an id-form entry will not match it: list the `#name` as well, or run
  `hear channels` once.
- Matching is exact and case-sensitive in both directions. Allowlisting a name
  and then passing the raw id is refused: nothing here knows they are the same
  conversation. That is the fail-closed direction.
- A person allowlisted by their `U…` id stays allowed once a DM exists and
  resolution starts yielding the `D…` id.
- It applies to a render too, not only to `--send`: a target outside the list
  cannot even be rendered.

It is stronger than the credential gate, which is binary — a fully authorised
write token still cannot reach a conversation outside the list.

## `tap` is a separate command, not a `sez` flag

- A reaction is fully described by its own arguments — channel, target ts,
  emoji — with no prose hidden inside them, so there is nothing to read before
  committing to it that the command line doesn't already show. It therefore
  acts by default, and `--dry-run` is the opt-out: the inverse of `sez`.
- A separate command name is a separate string to permission. A policy that
  permits `tap` while keeping `sez` behind review is only expressible if the
  two are different commands.

`tap` shares `sez`'s write credential — one write token covers every write
operation — so the write app needs `reactions:write` alongside `chat:write`.
The gate applies identically.

## Untrusted input is data, never code

Display names and message text are attacker-controlled: any workspace member,
including an external Slack Connect guest, can set a profile name to arbitrary
text. Mention substitution uses a replacement function whose return value is
used literally, so a display name containing `$1` or `${x}` is printed, not
expanded. The Markdown conversion does the same for the same reason.

The same text is printed to your terminal, so none of it reaches the terminal
raw. `hear` removes control characters, escape sequences and bidi overrides
from every name, message, filename and canvas it prints. A line break inside a
message is kept and indented to the text column, so it does not start a line of
its own. A name that would print differently is listed by its id instead, so
what `hear channels` and `hear dms` print still resolves. `--json` carries the
text exactly as Slack sent it.

## Parameter smuggling is impossible

Every request is built from endpoint-specific typed arguments and encoded as a
form; no caller-supplied field set ever reaches the wire. A crafted channel or
thread value cannot splice in an extra parameter — notably `blocks=`, which
Slack renders *instead of* `text`, making the message on screen differ from the
one in the audit log.

`sez` additionally validates the resolved channel against `^#?[A-Za-z0-9._-]+$`
and `--thread` against `^[0-9]+\.[0-9]+$` before sending, as defence in depth on
top of the encoding. `tap` validates its ts the same way and its emoji against
`^[a-z0-9_+-]+$`.

## No ambient configuration can alter a request

The HTTP client is built explicitly and is never `http.DefaultClient`:

- `Proxy` is nil, not `ProxyFromEnvironment`. `HTTP_PROXY` / `HTTPS_PROXY` /
  `NO_PROXY` cannot route a request carrying a bearer token through a host of
  their choosing.
- Redirects to the API are refused rather than followed.
- TLS is pinned to a 1.2 minimum.
- Every request carries its own deadline as well as the client timeout, so a
  hung connection cannot block a command indefinitely.

A file download is the one request that may be redirected, because Slack serves
file bytes from a host that can hand off to storage it does not serve itself. It
is the credential that is refused rather than the hop:

- The bearer token is dropped as soon as the host changes. A pre-signed URL
  carries its own authorisation in the query string, so the token is both
  unnecessary there and exactly what must not leak. Go's own rule compares
  domains and would carry the header across a change of port; this drops it on
  any change of host, and a test holds it to that.
- The target must be a host Slack serves files from, or the download is refused
  before the request is made.
- A downgrade from `https` is refused, and the chain is bounded at three hops so
  a loop cannot be spun.
- A refused redirect names a host and never a URL. `url.URL.Redacted` masks a
  userinfo password and leaves the query string alone, so naming the target
  would put a storage signature into the output and into `--json`.

An upload is the same problem in reverse, and takes the stricter line. Slack
hands back a pre-signed URL to send the bytes to, so:

- The write token is never attached to that request. The package that makes it
  holds no credential at all, so there is nothing to attach — the rule is a
  property of what the code contains rather than of what it remembers not to do.
- The URL must be on a host Slack takes uploads at, checked before the request
  exists.
- No redirect is followed. Nothing suggests Slack redirects an upload, and if it
  turns out to, that is something to learn from a refusal rather than by having
  already sent the file somewhere.
- No error, log line or message ever contains that URL. Its query string is the
  credential, and `sez` writes error text into the audit log — so the errors are
  built from a filename, a host and a class rather than wrapped, since
  `url.Parse`, `http.NewRequest` and the transport all quote the URL back. A
  test asserts a sentinel in the query string is absent from stdout, stderr,
  every audit line and every error string, across every way the transfer can
  fail.

## One attempt, and an outcome that admits doubt

A write is attempted **at most once** per invocation. Nothing is retried
automatically: a retry after a transport failure duplicates a message that was
in fact delivered.

Outcomes are recorded in three states, not two — Slack refused it (`ok:false`),
Slack confirmed it (`ok:true`), or the request left and no usable answer came
back (`ok:null`). An unrecognised failure is recorded as unknown rather than
failed, which is the safe direction: an unnecessary "unknown" asks a human to
look, while a possible delivery recorded as a definite failure invites the
duplicate.

## Every attempt is logged

Every `sez` and `tap` invocation that gets as far as being a real attempt
appends at least one JSON line to `~/.local/state/hersaid/posts.jsonl`, created
`0600`. A delivered write produces **two** lines: an `intent` line written and
flushed *before* the request leaves, and an `outcome` line after.

`SIGINT`, `SIGTERM` and `SIGHUP` still leave an `abort` line anywhere between
the intent record and the outcome record — including during the permalink fetch
that follows a delivered message — and the process still exits with the
conventional 128+signal. `SIGKILL` cannot be caught, which is exactly why the
intent line precedes the request: it is the record that survives.

A `--send` or a `tap` refused by the credential gate is logged too, as an
`abort` line: nothing was resolved and no request was made, but delivery was
asked for and declined, and a log read to answer "did anything try to go out"
must not answer no.

Things that are deliberately *not* logged, because they are not attempts: a
leading `--help` or `--version`, a usage error, an empty message, a schedule
time outside the window Slack takes, a channel argument that looks like a
flag, and `sez --scheduled`, which only reads.

See [Audit log](audit-log.md) for the field-by-field breakdown.
