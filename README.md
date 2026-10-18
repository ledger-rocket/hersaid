# hersaid

A small command-line suite that reads Slack and posts as you, with write
capability held in a separate credential and nothing delivered without an
explicit `--send`.

- `hear` — read: channels, DMs, threads, search.
- `sez` — post a message as yourself. Renders by default; `--send` delivers.
  `--edit <target> <ts> <text>` rewrites one and `--delete <target> <ts>`
  removes one, both rendering the message as it currently stands first.
  `--at` or `--in` schedules one for later, `--scheduled` lists what is
  waiting, and `--unschedule <target> <id>` cancels one.
- `tap` — add or remove an emoji reaction as yourself.
- `hersaid-setup` — enrol the credentials the other three read.

`say` is `/usr/bin/say` on macOS, hence `sez`.

Each command is a single Go binary: no interpreter, no `jq`, no SDK, nothing
fetched while it runs, nothing to install alongside it. A Linux build at
`CGO_ENABLED=0` is fully static. **A macOS build is not, and cannot be** — a
darwin binary links `libSystem` and `Security.framework` even at
`CGO_ENABLED=0`. They exec three things: `/usr/bin/security`, on macOS, to
read the keychain; `dbus-launch`, found on `PATH`, on a Linux host with no
session bus, when the keyring lookup tries to start one; and `hear`, which
`sez --edit` and `sez --delete` run to read the message they change.

```console
$ hear read '#general' 5          # read
$ sez '#general' 'deploy is done' # renders it, sends nothing
$ sez --send '#general' 'deploy is done'
```

![A terminal. hear read #eng prints three messages from alice and bob, one with an eyes reaction and one with a hint to open its thread. Then sez #eng with a reply prints a dry run: nothing sent, the channel resolved to its id, and the message as Slack will receive it.](docs/images/terminal.png)

*Example output with made-up people, channel and messages, not a capture from a real workspace.*

## Install

macOS and Linux, on amd64 or arm64. Each route installs all four commands;
none of them sets up a credential. [Credentials](#credentials) does that.

### From a release

No Go toolchain needed. Download the latest archive for your platform from
[Releases](https://github.com/ledger-rocket/hersaid/releases), check it against
`checksums.txt`, unpack it and run the installer inside it:

```sh
cd "$(mktemp -d)"
os=$(uname -s | tr '[:upper:]' '[:lower:]')                   # darwin or linux
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')    # amd64 or arm64
base=https://github.com/ledger-rocket/hersaid/releases/latest/download
curl -fsSLO "$base/checksums.txt"
tarball=$(grep -o "hersaid_[^ ]*_${os}_${arch}\.tar\.gz" checksums.txt) ||
  echo "no release archive for ${os}/${arch}; use go install or build from source" >&2
curl -fsSLO "$base/$tarball"
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
grep " $tarball\$" checksums.txt | sha256 -c - &&
  mkdir -p hersaid && tar -xzf "$tarball" -C hersaid &&
  ./hersaid/install.sh
```

The last command stops at a checksum mismatch, so a corrupted or incomplete
download is never installed. `checksums.txt` comes from the same release and is
not signed, so it does not prove who built the archive.

### With `go install`

```sh
go install github.com/ledger-rocket/hersaid/cmd/hear@latest
go install github.com/ledger-rocket/hersaid/cmd/sez@latest
go install github.com/ledger-rocket/hersaid/cmd/tap@latest
go install github.com/ledger-rocket/hersaid/cmd/hersaid-setup@latest
```

This puts the commands in `$(go env GOBIN)`, or in `$(go env GOPATH)/bin` when
that is empty; make sure that directory is on `PATH`. It needs the Go version
in `go.mod`. A Go 1.21 or later toolchain downloads it for itself unless
`go env GOTOOLCHAIN` prints `local`. Install
all four: `sez --edit` and `sez --delete` run `hear` to read the message they
change.

### From source

```sh
git clone https://github.com/ledger-rocket/hersaid.git && cd hersaid
./install.sh                      # build into ./dist, install onto PATH
PREFIX=/usr/local/bin ./install.sh
```

`install.sh`:

- builds `hear`, `sez`, `tap` and `hersaid-setup` into `./dist` when it runs
  in a source checkout with a Go toolchain; in a release archive, or without
  Go, it installs the commands already there
- picks an install directory: `$PREFIX` if set, else `~/.local/bin` if already
  on `PATH`, else `~/bin` if already on `PATH`, else `~/.local/bin`; warns if
  the chosen directory still isn't on `PATH`
- copies the four commands into it, and is safe to re-run

The commands are copied rather than symlinked because `dist` is build output
and gitignored, so a link into it dangles as soon as the tree is cleaned, and
a command that disappears reports only `command not found`. Re-run the
installer to pick up a rebuild.

It enrols nothing. `hersaid-setup` does that, and it is the only thing that
checks a token against Slack, and the scopes it was granted against the role,
before storing it. An installer that prompted as well would be free to disagree
with the commands about where a credential lives.

## Editing and deleting

`sez --edit` and `sez --delete` show the message as it stands before changing
it — an edit renders the current text beside the replacement, a delete renders
what is about to be destroyed — and act only under `--send`, like a post.

`sez` holds the write credential, and the write app has no history scopes, so
it cannot read that message itself. It runs the `hear` binary, which holds its
own: a separate process, no credential boundary crossed, and the read code
still absent from `sez`'s import graph.

**If the message cannot be read, the change is refused.** Not warned about —
refused, under `--send` too. The render exists so a human sees what changes; a
tool that could not show it has not earned the send. So `--edit` and `--delete`
need `hear` installed and its credential enrolled, and both fail closed when
`hear` is missing, refused, or slow to answer.

That pairing is also what lets the audit log record *what* a delete destroyed
rather than only that something was.

## Scheduling

`sez --at TIME` or `sez --in DURATION` schedules the message instead of posting
it. Slack holds it and posts it at that time, as you. Everything else is as for
a send: the render shows exactly what Slack will receive, `--send` is the only
thing that acts, `--thread` and `--md` mean the same, the target resolves the
same way, and the write allowlist applies.

- `--at` takes RFC 3339 with an offset (`2026-10-13T09:00:00Z`), or local time
  as `2026-10-13T09:00`, `2026-10-13 09:00` or `09:00` (today). The two dated
  local forms also take seconds. A time today that has already passed is
  refused, not moved to tomorrow: give the date. A local time that a
  daylight-saving change skips or shows twice is refused too. For a time in
  the repeated hour, give RFC 3339 with an offset.
- `--in` takes a Go duration of at least a second: `90m`, `2h30m`, `36h`.
- The render adds a `post at :` line with the local time and the UTC time.
  Slack takes whole seconds, so the time is truncated to the second.
- `--in` counts from when `sez` runs, so the `--send` that follows a render
  resolves it again, a little later. Use `--at` when the reviewed time must be
  the sent time.
- Slack takes a time up to 120 days ahead. A time in the past, or further
  ahead than that, is refused before anything is opened or logged, as an empty
  message is.
- A scheduled message carries text only. `--file` with `--at` or `--in` is a
  usage error, because Slack shares an upload at once.

```console
$ sez --at 2026-10-13T09:00 '#general' 'standup moved to 10:00 today'
--- dry run: nothing sent ---
channel : #general -> C0000000000
post at : Tue 2026-10-13 09:00:00 EDT (2026-10-13 13:00:00 UTC)
--- message as Slack will receive it ---
standup moved to 10:00 today
----------------------------------------

$ sez --send --at 2026-10-13T09:00 '#general' 'standup moved to 10:00 today'
scheduled to #general id=Q0000000001 channel=C0000000000
post at : Tue 2026-10-13 09:00:00 EDT (2026-10-13 13:00:00 UTC)

$ sez --scheduled
Q0000000001  C0000000000  Tue 2026-10-13 09:00:00 EDT (2026-10-13 13:00:00 UTC)
  standup moved to 10:00 today

$ sez --unschedule C0000000000 Q0000000001          # renders it
$ sez --send --unschedule C0000000000 Q0000000001   # cancels it
unscheduled Q0000000001 from C0000000000
```

`--scheduled [target]` lists the scheduled messages that have not posted yet,
soonest first, in one conversation or in all of them. It is a read: it renders
nothing first, writes no audit record, and the write allowlist does not limit
it. It needs the write credential all the same, the only one `sez` holds.
Slack lists only messages scheduled through its API, so a message scheduled in
the Slack client does not appear.

`--unschedule <target> <id>` cancels one. Like `--delete`, it reads the message
first and renders it, and it acts only under `--send`. If the list does not
hold that id in that conversation, the cancellation is refused, under `--send`
too: the message may have posted already. The read uses the write credential,
so unlike any other render this one makes a request to Slack, and it fails
without a write credential. Slack refuses to cancel a message within 60
seconds of its time.

## Credentials

### 1. Create the Slack app(s)

At [api.slack.com/apps](https://api.slack.com/apps), create a **read** app now
— e.g. name it `hersaid-read`. Create the **write** app later, only when you're
ready to enable posting — e.g. `hersaid-write`. Under **OAuth & Permissions**,
add scopes under **User Token Scopes**, not **Bot Token Scopes**: a bot-scoped
app shows up as a bot user and posts *as the bot*; a user-scoped app has no bot
identity and posts *as you*.

Read app — **User Token Scopes**:

- `channels:history`, `channels:read`
- `groups:history`, `groups:read`
- `im:history`, `im:read`
- `mpim:history`, `mpim:read`
- `users:read`
- `search:read`
- `files:read`

Write app — **User Token Scopes**:

- `chat:write` (this also covers scheduling and cancelling a scheduled message)
- `reactions:write`
- `files:write`

If the write app existed before `reactions:write` was added, adding the scope
alone isn't enough: Slack only issues a token carrying it on the *next*
install. Add the scope, reinstall the app to your workspace, then re-run
`hersaid-setup --with-write` — the token already stored still lacks it.

Install each app to your workspace and copy the **User OAuth Token**
(`xoxp-...`). Depending on workspace settings, an admin may need to approve the
install before the token works.

### 2. Enrol

```sh
hersaid-setup                # the read credential; posting stays impossible
hersaid-setup --with-write   # also the write credential, enabling posting
```

What it does, in order, per credential:

- asks `[y/N]` before anything; anything but `y…` skips that role
- reads the token at a **masked** prompt — one asterisk per character, so a
  truncated paste is visible — and never prints the token itself, only a
  fingerprint (`xoxp-1234...cdef (56 chars)`)
- **refuses a bot token** (`xoxb-`) outright: it carries `chat:write` and would
  pass the scope check below, but posts as an app rather than as you
- calls `auth.test`, prints the account the token resolves to, and **checks the
  granted scopes against the role**: a token carrying `chat:write` is refused
  for the read role, one without it is refused for the write role
- **fails closed when the scope header is absent.** An unanswerable question is
  a refusal, not a warning — the check is the only thing standing between a
  read credential and one that can post
- names an existing credential and asks before replacing it
- prints where the token was stored, and names anything that outranks it — a
  set `SLACK_*_TOKEN`, or a `~/.config/hersaid/{read,write}.token` the keychain
  now shadows. Nothing is deleted for you; the old token is yours to revoke

With stdin redirected it asks nothing and stores nothing, and exits 0, so it is
safe in a script. Exit 1 means a token was refused.

Manual alternative, which **skips the scope check**:

```sh
# keychain (macOS); omit the value after -w and it prompts, keeping the token
# out of shell history. Add -U to replace an existing item.
security add-generic-password -a "$USER" -s hersaid-read -w
security add-generic-password -a "$USER" -s hersaid-write -w

# or a file, mode 0600 (any host without a keyring). Paste the token in an
# editor rather than echoing it, which would put it in shell history.
mkdir -p ~/.config/hersaid && chmod 700 ~/.config/hersaid
(umask 077; : > ~/.config/hersaid/read.token)
"$EDITOR" ~/.config/hersaid/read.token
```

### Where things live

- credentials: keychain services `hersaid-read` / `hersaid-write` (account
  `$USER`), or `~/.config/hersaid/{read,write}.token` at mode `0600`
- audit log: `~/.local/state/hersaid/posts.jsonl`, mode `0600`
- cache: `~/.cache/hersaid/{chans.json,users.json,identity.json}`, mode `0600`

`XDG_CONFIG_HOME`, `XDG_STATE_HOME` and `XDG_CACHE_HOME` are honoured.

## Environment

- `SLACK_READ_TOKEN` — read credential. **Presence** is tested, not
  non-emptiness: exporting it empty means "no credential", and stops resolution
  rather than falling through to the keychain.
- `SLACK_WRITE_TOKEN` — write credential, same rule.
- `HERSAID_WRITE_CHANNELS` — comma-separated conversation ids and/or `#names`
  that `sez` and `tap` may write to. Unset means unrestricted.
- `HERSAID_TIMEOUT` — request timeout in whole seconds, default 20. Unset or
  empty means the default; any other value that is not a positive whole number
  is an error, not a disabled timeout. Unlike the token variables, an empty one
  is *not* special here. `hersaid-setup` ignores it and always uses 20 seconds.

The environment-variable route is the weakest of the credential sources on
Linux, where any process running as the same user can read
`/proc/<pid>/environ`. It exists for CI and containers, not as the everyday
path on a workstation.

## The safety model

Read this before wiring any of it into something that runs unattended,
especially an AI agent with shell access.

### Two Slack apps, two tokens

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

### Credential resolution

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

### `hear` cannot post

The token `hear` uses has no `chat:write` scope, and `hear` has no write verb:
no `post`, no `--send`, nothing that reaches `chat.postMessage`. The write
endpoints live in a separate package that `hear` does not import, so the code
path to post is not in that binary at all. Two tests hold it —
`TestImportGraphExcludesWriteCode` walks the import graph, and
`TestBuiltBinaryHasNoWriteEndpoints` scans the built artifact.

### The credential gate

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

### Render-only is the default; delivering needs `--send`

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

### The per-channel write allowlist

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

### `tap` is a separate command, not a `sez` flag

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

### Untrusted input is data, never code

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

### Parameter smuggling is impossible

Every request is built from endpoint-specific typed arguments and encoded as a
form; no caller-supplied field set ever reaches the wire. A crafted channel or
thread value cannot splice in an extra parameter — notably `blocks=`, which
Slack renders *instead of* `text`, making the message on screen differ from the
one in the audit log.

`sez` additionally validates the resolved channel against `^#?[A-Za-z0-9._-]+$`
and `--thread` against `^[0-9]+\.[0-9]+$` before sending, as defence in depth on
top of the encoding. `tap` validates its ts the same way and its emoji against
`^[a-z0-9_+-]+$`.

### No ambient configuration can alter a request

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

### One attempt, and an outcome that admits doubt

A write is attempted **at most once** per invocation. Nothing is retried
automatically: a retry after a transport failure duplicates a message that was
in fact delivered.

Outcomes are recorded in three states, not two — Slack refused it (`ok:false`),
Slack confirmed it (`ok:true`), or the request left and no usable answer came
back (`ok:null`). An unrecognised failure is recorded as unknown rather than
failed, which is the safe direction: an unnecessary "unknown" asks a human to
look, while a possible delivery recorded as a definite failure invites the
duplicate.

### Every attempt is logged

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

See [Audit log](#audit-log) for the field-by-field breakdown.

## Usage

Every command answers `--help` and `--version` **when the flag leads** —
before any filesystem work and before any credential lookup, so asking a tool
what it does never raises a keychain prompt. They go to stdout and exit 0. A
*usage error* is the opposite case: stderr and exit 2.

Put the flag first. Once a command's own arguments have begun, `--help` is an
argument like any other: `hear whoami --help` is a usage error, `hear read
--help` looks up a channel called `--help`, and `sez '#general' --help`
renders `--help` as the message text and logs the render.

Exit statuses, shared by all four commands:

- `0` — did what was asked, including a render or dry run that sent nothing
- `1` — refused, or Slack refused
- `2` — the invocation was wrong
- `3` — (`sez`, `tap`) something was attempted and the record of it could not
  be written

### hear (read-only)

```
usage: hear <command> [options]

  Commands:
    whoami                        confirm which account the read token belongs to
    channels [filter]             list channels you can read
    dms [filter]                  list people you have a 1:1 DM with
    read <address> [n]            last n messages (default 20)
    thread <address> <ts>         replies in a thread
    files <address> <ts>          download the files attached to one message
    canvas <link|F…|#chan>        print a canvas as Markdown
    search <query...>             search messages across the workspace
    inbox                         what is new for me: DMs and group DMs,
                                  mentions, and new replies in threads I
                                  started or replied in
    refresh                       clear the channel/user cache

  Options:
        --ts          prefix each message with its timestamp, which is what
                      sez --thread and tap take; accepted in any position
        --json        write one structured JSON object instead of a transcript
        --since D     read a window rather than a count: 30m, 2h, 7d, 1w.
                      Pages to the end of the window, or to the newest 5000
                      messages in it, whichever comes first — --json reports
                      which by "complete". Read only, not combinable with n.
                      For inbox it is the window to report; default 24h.
        --max-calls N inbox: the most requests one run makes to Slack, from
                      1 to 1000. Default 60. A cache fill is counted apart
                      and not capped by it
        --out DIR     where files writes; the message's own directory is made
                      inside it. Default: the current directory
        --max-size N  largest file to fetch, e.g. 512K, 100M, 2G. Default 100M.
                      A file above it is skipped and named, never truncated
        --            stop reading options; what follows is literal
    -h, --help        print this help and exit 0
        --version     print the version and exit 0
```

Abridged: `hear --help` also lists the environment variables, the credential
sources and the JSON shape.

An **address** is `#channel`, `@person`, a raw Slack id (`C…`, `D…`, `G…`), or
a message permalink:

```sh
hear read 'https://yourteam.slack.com/archives/C0123ABCD/p1785708834862009'
```

A permalink carries its own channel and timestamp, so a link someone pasted can
be read as it was given; a `thread_ts=` in the URL is honoured. It names one
message, so a count or `--since` alongside it is a usage error. Raw ids bypass
the cache entirely, so a channel absent from the cache is still reachable.

A **filter** is a case-insensitive substring, not a pattern. Matching nothing
is an empty result and exit 0, not an error.

`--ts` shows each message's raw Slack timestamp, for `read`, `thread` and
`search`. It exists because reading doesn't need a timestamp but *acting* on a
message does: it is the `<ts>` `tap` takes and the value `sez --thread` takes.
It is accepted in any position — `hear read --ts '#general'` and
`hear --ts read '#general'` are the same command.

`--since` reads a window instead of a count, and pages until the whole window
is covered rather than stopping at Slack's first page. A duration says nothing
about how much there is, so it stops after 5000 messages — the newest of the
window — and reports the window as incomplete rather than holding a channel's
whole history in memory. It applies to `read` and `inbox` only, and not
together with a count. `30m`, `2h`, `7d` and `1w` are the forms worth remembering; anything
Go's own duration parser takes works too, so `1h30m` is a window as well.

Examples:

```console
$ hear whoami
jane @ some-workspace  (U0123ABCDE)

$ hear channels gen
#general
#general-notes

$ hear read '#general' 5
08-01 09:03  jane  good morning
08-01 09:04  bot  deploy finished
                    :white_check_mark: 2 (jane, john)  :eyes: 1 (jane)
08-01 09:10  jane  @john can you take a look at this?
                    it's blocking the release
                    ⤷ 2 replies — hear thread #general 1722500000.123456

$ hear read --ts '#general' 1
1722500000.123456  08-01 09:03  jane  good morning

$ hear read '#alerts' --since 2h
$ hear thread '#general' 1722500000.123456
$ hear search deploy failed
#general  jane  looks like the deploy failed on the last run

$ hear read '#eng' --ts 1
1785708834.862009  08-01 09:12  jane  here's the trace
                                        ⤷ 2 files: trace.pdf (1.2 MB), out.log (14.0 KB)

$ hear files '#eng' 1785708834.862009
wrote   eng-1785708834.862009/trace.pdf (1.2 MB)
wrote   eng-1785708834.862009/out.log (14.0 KB)
```

`<@U…>` mentions are rewritten to `@display-name` (or the real name, or the
handle). An id nobody knows renders as `@U0123ABCDE` rather than `?`, so a
mention of an external or deleted account stays traceable.

A search query is passed through as written, so Slack's own syntax works —
including `-term` exclusion, since a single-dash word is not read as an option.
`--` stops option parsing for a query that really does begin with two dashes.

`hear files` writes each attachment into `<out>/<channel>-<ts>/`, one directory
per message, so two messages' files cannot collide. It is best effort and says
what happened to each file:

- A file with no bytes behind it is **skipped** with the reason named — a link
  to Google Drive or another external store, a file that has been deleted, an
  email body. So is one larger than `--max-size`.
- Anything skipped or failed makes the **exit status 1**, whatever else was
  written, so a script can tell it got less than it asked for.
- A name Slack sent is never taken as a path. It is reduced to its last element
  and replaced by the file id if what is left could not be a filename, so an
  upload called `../../.ssh/authorized_keys` writes `authorized_keys` inside the
  message directory and nowhere else.
- An existing file is **never overwritten**; that file fails and the one on disk
  is left alone. Two attachments sharing a name both land, the second gaining
  its file id before the extension.
- Bytes go to a temporary file and are linked into place only once the whole
  body has arrived, so an interrupted download leaves nothing that looks
  complete.

Downloading is the one request that may be redirected — Slack serves file bytes
from a host that can hand off to storage. The bearer token does not go with it:
it is dropped the moment the host changes, the target must be a host Slack
serves files from, and the chain is bounded. See
[No ambient configuration can alter a request](#no-ambient-configuration-can-alter-a-request).

Without `files:read` Slack answers a file request with HTTP 200 and a sign-in
page rather than an error. `hear files` refuses a web page where a file was
expected and names the missing scope, so that page never lands as a `.pdf`.

`hear refresh` deletes every `*.json` in `~/.cache/hersaid/`; the next command
that needs the lists rebuilds them. `channels`, `dms` and `refresh` are
answered from the cache when it is warm, and resolve no credential at all.

### hear inbox

`hear inbox` answers "what is new for me since T" in one read: messages in your
DMs and group DMs, messages that mention you, and new replies in threads you
started or replied in, however old the thread. T is `--since` ago, 24 hours by
default.

```console
$ hear inbox --since 14h
@jane  (dm)
08-02 07:58  jane  are you around later?
thread 07-21 16:02  sam  the plan for the release …  (hear thread C0123ABCD 1721577720.000100)
    08-02 08:15  john  done, see the notes
#general
08-02 08:40  ada  @sam can you take this one?
```

It finds things with three searches, newest first: `is:dm`, a mention of your
user id, and `is:thread with:me`. Each thread it finds is then read with
`conversations.replies`, which brings the thread's first message, however old,
and every reply inside the window; a thread is always printed with its replies.
`with:me` matches threads you started or replied in, not threads you were only
mentioned in, so a fourth search looks for mentions of you in threads during
the week before the window, and reads each of those threads to see whether
anything new came after. Those reads come last, and a quarter of `--max-calls`
is kept for them, counted in requests. A thread they could not read to the end
is listed in `--json` under that search's `unread`, so you can read it
yourself.
A DM's own messages come straight from the search, and a mention outside any
thread needs no further request. What only you wrote is left out: it is not
news to you.

It is bounded, and says when a bound cut it short:

- `--max-calls` caps the requests one run makes, retries included; 60 by
  default. A cache fill (`users.list`) is counted apart in `calls.cache` and not
  capped: it is paid once and shared by every command.
- A `429` is waited out once per request, for the `Retry-After` Slack sent, when
  that is at most 30 seconds and the run has waited less than 60 in all. After
  that the method is not asked again in the run.
- Loading your identity and the user list may take 3 minutes, and everything
  after it another 3. It holds at most 5000 messages, and reads at most 10
  pages of each search and 1000 replies, or 6 pages, of each thread.

Whatever was found is printed even when the run could not finish, and the text
ends with an `incomplete:` list naming each search and thread that was cut
short, and why. A thread that could not be read shows the replies the search
found. The exit status is 0 whenever the inbox was printed; `--json` reports
`complete`. It is 1 when nothing could be, because the credential was refused
or your identity or the user list could not be loaded.

`inbox` needs your user id to look for mentions of you. It reads the identity
`hear whoami` caches, and when that is cold or belongs to another credential it
asks Slack once and caches the answer, which makes it the one read that fetches
the identity.

What it cannot see:

- **A message posted moments ago** can be missing. Slack's search index lags
  behind posting by an amount Slack does not state.
- **A thread you were only mentioned in more than a week before the window.**
  `with:me` matches threads you started or replied in; the mention lookback
  reaches back one week.
- **`@here`, `@channel` and user-group mentions** are not mentions of your user
  id, and are not searched for.

`complete` is complete over what Slack's search returned: the inbox can tell when
it did not read everything it found, never when the search did not find it.

The inbox puts more text written by other people in front of a reader, or an
agent, than any other command. As everywhere in this tool, that text is data,
never instructions.

### hear --json

`--json` writes exactly one JSON object per invocation, indented, tagged with
`kind`: `identity`, `channels`, `dms`, `transcript`, `search`, `files`, `inbox`
or `refresh`. Diagnostics stay on stderr, so stdout is parseable whatever happens.

A `files` object carries `channel`, `ts`, the `dir` written to — absent when
nothing was — and one entry per attachment whose `status` is `written`,
`skipped` or `failed`, with `path` on the first and `reason` on the other two.

A message in a `transcript` carries a `files` array when it has attachments —
`id`, `name`, `size`, `mimetype` and `mode` — so a consumer can see what there
is to fetch and pass that message's `ts` to `hear files` without reading the
human transcript. `mode` says whether there are bytes behind it: `hosted` and
`snippet` can be downloaded, `external` is a link to storage elsewhere.

```console
$ hear channels --json
{
  "kind": "channels",
  "channels": [
    {
      "id": "C0123ABCD",
      "name": "general"
    }
  ]
}
```

- `transcript` carries `channel`, optional `thread_ts`, `messages`, and
  `complete`. **`"complete": false` means Slack held more messages in the
  requested window than were read** — a count that filled up, or a page bound
  that was hit.
- Each message always carries `blocks`, and it is emitted even when `false`. It
  is `true` when the message is built from blocks whose fallback `text` does not
  faithfully show it — the field a consumer checks before acting on `text`. It
  is always present so that "this message has no blocks" can be told from "this
  `hear` is too old to say", which is what `sez --edit` and `sez --delete` refuse
  on. `partial` is the wider claim: `blocks` or files, either of which means
  `text` is not the whole message.
- Each message always has `ts`, `author`, `text` and `text_rendered`. `text`
  is exactly what Slack sent, mentions and all; `text_rendered` is the same
  text with `<@U…>` resolved — both, always, so a consumer never has to guess
  which it got. Omitted when empty: `time` (RFC 3339 **in UTC**, whatever zone
  the transcript renders its date column in — a structured document is read
  somewhere else, and absent when the timestamp will not parse), `user` (absent
  on a bot post), `bot_id`, `reply_count` and `reactions`.
- A reaction carries `name`, `count` and the raw `users` ids. **`count` is
  authoritative and `users` is not**: Slack truncates the array on a
  heavily-reacted message, so the two genuinely differ. The transcript names the
  first three reactors and reconciles the remainder against `count`, as
  `(you, jane, ada +47)`. Your own reaction renders as **`you`** and is promoted
  to the front, so it is never pushed into the overflow on a busy message.

  That marker needs to know which account you are, which `hear` learns from the
  cache and never fetches: a read makes no request it would not otherwise make.
  `hersaid-setup` seeds it when it enrols the read credential, and `hear whoami`
  refreshes it — both already ask Slack who you are, so neither costs a request
  of its own. After `hear refresh` clears the cache, one `whoami` restores it.
  Until it is warm, reactions render with
  your display name instead, and on a message with more than three reactors it
  may fall into the overflow, which is exactly what the marker prevents. The
  identity records which credential it came from and is ignored under any other,
  so enrolling a different account leaves reactions unmarked rather than marked
  wrongly. On a heavily-reacted
  message Slack truncates the users array, and a reader outside that window
  cannot be found in it; the marker is then absent rather than wrong.

  Emoji are named rather than rendered as characters,
  because custom workspace emoji have no character form at all.
- `search` carries `query` and `matches`; each match has `ts`, `time`,
  `channel`, `author`, `text`, `text_rendered` and a **`permalink`** — the one
  Slack returned with the hit, which `hear read` accepts straight back as an
  address.
- `inbox` carries `since` and `until` (RFC 3339, UTC), `user_id`, `complete`,
  `calls` (`used`, `max`, `cache`), one entry per search in `sources` (`name`
  is `dms`, `mentions`, `threads` or `mention_threads`, then `query`,
  `complete`, `hits`, `reason` when not complete, and `unread`, the threads the
  mention lookback could not read to the end), and
  `conversations`. A conversation has `channel`, `kind` (`dm`, `group_dm` or
  `channel`), `user` for a 1:1 DM, `name` (that person's label, as
  `hear dms --json` gives a person the user list knows; absent when it does
  not know them, where `hear dms` gives the id), `source`, `complete`,
  `messages` and `threads`. A thread has `thread_ts`, `parent` (null when Slack
  did not return it), `replies`, `source` (`replies` when it was read, `search`
  when it was not), `complete` and `reason`. Messages are transcript messages,
  `blocks` included, plus `mentions_me`. A message appears once: a thread's
  first message is its `parent` and not also one of the conversation's
  `messages`.
- HTML escaping is off here, unlike in the audit log, so `<@U…>` and
  `<https://…>` arrive as themselves rather than as `\u003c…`.

### sez (post, with audit log)

```
usage: sez [--send] [--thread TS] [--md] [--file PATH]... <#chan|@person|ID> [text]
       sez [--send] [--thread TS] [--md] (--at TIME | --in DURATION) <#chan|@person|ID> [text]
       sez [--send] [--md] --edit <#chan|@person|ID> <ts> <new text>
       sez [--send] --delete <#chan|@person|ID> <ts>
       sez --scheduled [<#chan|@person|ID>]
       sez [--send] --unschedule <#chan|@person|ID> <id>

  Renders the message WITHOUT sending unless --send is given. Read the rendered
  text first; deliver it second.

  Text may be given as an argument, or piped on stdin (use "-" or omit it).
  With --file, omitting the text means no comment and stdin is not read; "-"
  still reads it.

  Options:
        --send        actually deliver it (default is render-only)
    -n, --dry-run     render only; the default, accepted for explicitness
        --md          convert Markdown to Slack mrkdwn before sending
        --thread TS   reply inside the thread with this parent ts
        --file PATH   attach a file; repeatable, and order is kept
        --at TIME     schedule for TIME: RFC 3339 (2026-10-13T09:00:00Z), or
                      local YYYY-MM-DDTHH:MM[:SS], or local HH:MM today
        --in DURATION schedule for this long from now, e.g. 90m or 2h30m
        --edit        rewrite the message at the given ts; the render shows both
        --delete      delete the message at the given ts; the render shows what goes
        --scheduled   list scheduled messages, in one conversation if one is given
        --unschedule  cancel the scheduled message with the given id
        --            stop reading options; what follows is the target and text
    -h, --help        print this help and exit 0
        --version     print the version and exit 0
```

Abridged: `sez --help` also explains `--edit`, `--delete` and scheduling, and
lists the environment variables, the exit statuses and the audit-log path.

```console
# default: renders, sends nothing
$ sez '#general' 'deploy is done'
--- dry run: nothing sent ---
channel : #general -> C0123ABCD
--- message as Slack will receive it ---
deploy is done
----------------------------------------

# --send delivers it
$ sez --send '#general' 'deploy is done'
sent to #general ts=1722500000.123456
https://yourteam.slack.com/archives/C0123ABCD/p1722500000123456

# text on stdin — still render-only without --send
$ echo 'deploy is done' | sez '#general'

# thread reply, sent
$ sez --send --thread 1722500000.123456 '#general' 'fixed in the follow-up'

# attach files; the text becomes the message alongside them
$ sez --send --file ./trace.pdf --file ./out.log '#eng' "here's the repro"
uploaded to #eng ts=1722500000.123456
  trace.pdf  F0123ABCD
  out.log    F0123ABCE
https://yourteam.slack.com/archives/C0123ABCD/p1722500000123456

# files with no message of their own
$ sez --send --file ./chart.png '#eng'

# Markdown conversion, rendered
$ sez --md '#general' '**deploy** done, see [changelog](https://example.com)'
--- dry run: nothing sent ---
channel : #general -> C0123ABCD
--- message as Slack will receive it ---
*deploy* done, see <https://example.com|changelog>
----------------------------------------

# no write credential yet — rendering still works, and says so
$ sez '#general' 'test'
--- dry run: nothing sent ---
channel : #general -> C0123ABCD
token   : absent (posting currently impossible)
--- message as Slack will receive it ---
test
----------------------------------------

# --send with no write credential — refused, nothing sent, no request made
$ sez --send '#general' 'test'; echo "exit=$?"
sez: no write credential found (keychain service: hersaid-write).
  Posting is disabled by design until one exists:
    security add-generic-password -a "$USER" -s hersaid-write -w
  Slack issues one token per app, so this needs a SECOND Slack app carrying
  chat:write — the read app's token cannot post.
  Rendering needs no credential — omit --send.
exit=1
```

Details:

- The rendered `channel :` line shows `input -> id` once resolution has a real
  id, and the input alone when a `#name` is passed through unresolved.
  `chat.postMessage` resolves a `#name` server-side, so `sez` works with a cold
  cache; it also accepts a `U…` id and opens the DM itself.
- Text comes from stdin when the argument is omitted or is `-`; later arguments
  after a `-` are ignored. Multiple text arguments are joined with spaces.
  Trailing newlines are dropped, as `$(cat)` dropped them: a heredoc and an
  editor both end with one, and neither means a blank line at the end of the
  message.
- An empty or whitespace-only message is refused before any credential is
  resolved or the log is opened. A channel argument starting with `-` is
  refused rather than swallowed — as an unknown flag (exit 2), or, after `--`,
  as a suspicious channel (exit 1).
- `--md` converts once, before the render, the audit record and the wire all
  see it — rendering what you typed while sending something else is precisely
  what `--send` exists to prevent.
- On a successful send the permalink is fetched best-effort. A permalink
  failure never turns a delivered message into an error; the record just
  carries a null `permalink`.

### tap (reactions, with audit log)

```
usage: tap [--remove] [--dry-run] <#chan|@person|ID> <ts> <emoji>

  ts      the target message timestamp, e.g. 1785712596.256909
          (sez prints it on send; hear shows it on threaded messages)
  emoji   name with or without colons, e.g. :+1: or thumbsup

  Options:
    -r, --remove      remove the reaction instead of adding it
    -n, --dry-run     resolve and validate without calling Slack
        --            stop reading options; what follows are the arguments
    -h, --help        print this help and exit 0
        --version     print the version and exit 0
```

Abridged, as above; `tap --help` prints the rest.

```console
$ tap '#general' 1722500000.123456 +1
add :+1: on #general (1722500000.123456)

$ tap --remove '#general' 1722500000.123456 ':+1:'
remove :+1: on #general (1722500000.123456)

# reacting twice is a no-op, not an error
$ tap '#general' 1722500000.123456 +1
:+1: already on that message

$ tap --dry-run '#general' 1722500000.123456 eyes
--- dry run: nothing sent ---
action  : react.add
channel : #general -> C0123ABCD
message : 1722500000.123456
emoji   : :eyes:
```

- The emoji may carry surrounding colons or not; otherwise it must be
  lowercase letters, digits, `_`, `+` or `-`.
- `already_reacted` on add and `no_reaction` on remove are **successes**: the
  state you asked for already holds. `tap` prints a note, exits 0, and the log
  records `ok:true` with the Slack code in `error`.
- Unlike `sez`, `tap` needs a real conversation id — `reactions.add` will not
  take a bare `#name` — and it cannot fill the cache itself, since it holds no
  read credential. Run `hear channels` (or `hear dms`, for `@person`) once
  first.

## Markdown vs Slack mrkdwn

Slack does not speak Markdown; it has its own `mrkdwn`. `--md` runs a
best-effort conversion. Fenced and inline code are matched first and passed
through byte for byte — the tool is routinely used to paste code and logs, and
converting inside a code block would corrupt it. A converted link is protected
the same way, so no later rule can rewrite its destination. Outside those, in
this order:

- `[text](url)` → `<url|text>`
- `# heading` … `###### heading` → `*heading*` (any `**` inside the heading text
  is stripped first, so it doesn't nest)
- `-`, `+` or `*` bullets at the start of a line → `•`
- `**bold**` → `*bold*`, never spanning a line break

`__bold__` is deliberately **not** converted: double underscores appear in
`__init__` and every other dunder in pasted Python, and mangling those is worse
than leaving emphasis unconverted.

This is a handful of rules, not a Markdown parser. Slack has no table syntax at
all; if the text looks like a Markdown table (`|--` or `| ---`), `sez` warns on
stderr, because it will otherwise post as literal pipes and dashes. The warning
is advisory and is not part of the rendered payload — it doesn't change whether
the run renders or sends. Review the render before adding `--send`.

## Audit log

`~/.local/state/hersaid/posts.jsonl`, mode `0600`, one JSON object per line,
shared by `sez` and `tap`. Each record is a single unbuffered append, flushed
before the process can move on, so concurrent invocations interleave whole
lines rather than fragments. If the mode cannot be enforced on an existing
file, both commands warn rather than refusing — losing the record is worse.

Every record carries `at`, `by`, `phase`, `channel`, `channel_id`, `dry_run`,
`ok` and `error`. The rest of the field set differs by which command wrote it.

`phase` is one of:

- `intent` — written and flushed before the request leaves, so a hung,
  interrupted or killed attempt still leaves evidence of what was about to go
  out. `ok` is `null` here: the outcome is not yet known.
- `outcome` — an attempt that reached the API.
- `dry-run` — a render or validation that deliberately sent nothing.
- `refused` — the target was outside `HERSAID_WRITE_CHANNELS`. A distinct phase
  so a policy refusal is greppable without disturbing consumers filtering on
  `outcome`.
- `abort` — any other refusal or failure, including a caught signal.

`ok` is **tri-state**: `true`, `false`, or `null` for "the request left and the
result is unknown". Consumers must handle `null` — treating it as false is the
mistake that causes a duplicate message.

### sez records

- `at` — UTC, `YYYY-MM-DDTHH:MM:SSZ`
- `by` — `$USER`, falling back to the current user, then `unknown`
- `phase`, `channel` (as given)
- `channel_id` — the resolved id; the `#name` itself when `sez` passed a name
  through unresolved; `null` when resolution never happened. On a schedule's
  outcome it is the conversation id Slack reported, which is the one
  `--unschedule` needs when the target was a `#name` or a person.
- `thread_ts` — from `--thread`, or `null`
- `dry_run`, `ok`, `error`
- `action` — `message.send`, `message.update`, `message.delete`,
  `message.upload`, `message.schedule` or `message.unschedule`
- `ts` — for a send, the timestamp the send produced, known only afterwards;
  for an edit or a delete, the `<ts>` argument, so the intent line carries it
  too; `null` for a schedule or a cancellation, which name no posted message
- `permalink` — link to the sent message, or `null`
- `text` — for a send, a schedule or an edit, the message text after `--md`
  conversion; for a delete, the text that was destroyed; for a cancellation,
  the text that will not now be posted
- `previous_text` — the wording an edit replaced, or `null`. An edit destroys
  the previous wording as surely as a delete destroys the message, and this is
  the only record of it.
- `slack_text` — what Slack actually stored, which can differ: it sanitises and
  truncates. Logging both makes the difference auditable.
- `files` — for an upload, one object per attachment: `path`, `name`, `size`,
  `mimetype` and `id`. For a delete, one object per attachment the message
  carried, with `name` and `id` only — the rest are empty, and the files
  themselves stay in the workspace. The name is as Slack holds it, not as it
  printed. `id` is `null` on the intent line, which is written
  before any file has been staged, and fills in as each one lands. `null` on
  every record that carries no files. A path that is not valid UTF-8 is
  recorded with the replacement character, JSON strings being Unicode.
- `post_at` — when a scheduled message posts, in UTC in the same format as
  `at`: the time asked for on a schedule, the time Slack held on a
  cancellation. `null` on every other record.
- `scheduled_message_id` — Slack's id for a scheduled message, the one
  `--unschedule` takes. A schedule learns it from Slack's reply, so it is
  `null` on that schedule's intent line; a cancellation has it from its
  arguments on every line. `null` on every other record.

### tap records

Same leading fields, plus:

- `action` — `react.add` or `react.remove`. Every record either command writes
  names an action, so `select(.action // "" | startswith("message."))` isolates
  `sez` lines and `startswith("react.")` isolates `tap`'s. The key is present on
  every line; on a `sez` record built without an action it is `null`.
- `ts` — the *target* message's timestamp, i.e. the `<ts>` argument, known from
  the arguments rather than from the reply. `sez`'s `ts` means the same for an
  edit or a delete; only for a send is it what the send produced.
- `emoji` — the name with colons stripped, or `null`
- no `thread_ts`, `permalink`, `text` or `slack_text`

`tap` writes an `intent` line before its request, as `sez` does, so a kill
mid-request still leaves evidence a reaction was attempted.

### Reading it

```sh
LOG=~/.local/state/hersaid/posts.jsonl

# messages actually delivered today
jq -c --arg d "$(date -u +%F)" \
  'select(.phase == "outcome" and .action == "message.send" and .ok == true
          and (.at | startswith($d)))' "$LOG"

# messages scheduled, with the id and conversation that cancel them
jq -c 'select(.phase == "outcome" and .action == "message.schedule" and .ok == true)
       | {post_at, channel_id, scheduled_message_id, text}' "$LOG"

# everything destroyed or rewritten, with what it used to say
jq -c 'select(.phase == "outcome" and .ok == true
              and (.action == "message.delete" or .action == "message.update"))
       | {at, action, channel, ts, gone: (.previous_text // .text)}' "$LOG"

# anything whose outcome is unknown — check Slack before retrying
jq -c 'select(.phase == "outcome" and .ok == null)' "$LOG"

# refused by the write allowlist
jq -c 'select(.phase == "refused")' "$LOG"

# aborted or failed before/during a send
jq -c 'select(.phase == "abort")' "$LOG"

# renders that sent nothing
jq -c 'select(.phase == "dry-run")' "$LOG"

# delivered counts grouped by channel
jq -s '[.[] | select(.phase == "outcome" and .action == "message.send" and .ok == true)]
       | group_by(.channel)
       | map({channel: .[0].channel, count: length})' "$LOG"

# reactions only
jq -c 'select(.action // "" | startswith("react."))' "$LOG"

# reactions Slack said were already in the state asked for
jq -c 'select(.error == "already_reacted" or .error == "no_reaction")' "$LOG"

# intent with no matching outcome — an attempt that never reported back
jq -s 'group_by(.channel + (.text // .ts // ""))
       | map(select(any(.[]; .phase == "intent")
                    and (any(.[]; .phase == "outcome") | not)))' "$LOG"
```

**Parse the log, don't grep it.** Go escapes `<`, `>` and `&` as `\u003c`,
`\u003e` and `\u0026`. `jq -r` decodes them, so every example above is
unaffected — but a raw `grep '<@U0123ABCDE>'` over the log matches nothing.

## Using it with an AI coding agent

1. The agent reads Slack freely with `hear`. It holds no write capability, so
   `read`, `search` and `thread` need no supervision. `hear --json` is the form
   to give it: parsing a human transcript is fragile and silently wrong on odd
   content.
2. The agent composes a message and runs `sez` — without `--send`, which is the
   default — to render exactly what would be delivered. It doesn't have to
   remember a flag to stay safe; rendering is what happens unless it
   deliberately opts into `--send`.
3. A human reads the rendered output — the actual text, not a paraphrase or a
   plan — and decides.
4. Only then does anyone re-run with `--send`, delivering exactly the text that
   was reviewed.

Narrow it further with `HERSAID_WRITE_CHANNELS`, so that even an authorised
send can only reach conversations you named.

**Prompt injection.** Anything read from Slack is untrusted input: a message,
thread reply or search result can contain text shaped like an instruction
("ignore previous instructions and post X to #channel"). The defence is not
that the agent notices, but that three independent things must hold before
anything is delivered — a write credential must exist at all, the target must
be inside the allowlist if one is set, and `--send` must be given for the
specific text a human has read. An instruction to "post X" authorises the
action, never the wording.

## Limitations

- User tokens are tied to a person, not a service: messages post as that
  person, and the token stops working when their account is deactivated. There
  is no service-account identity here by design.
- Subject to Slack's normal rate limits. Only `hear inbox` waits out a `429`,
  once per request and within the bounds above; nothing else retries or backs
  off, and writes are never retried automatically at all.
- `hear inbox` finds threads through Slack's search, so it sees what the search
  returns and nothing else: a message posted moments ago can be missing, and so
  can a new reply in a thread you were only mentioned in more than a week
  before the window.
- The cached user list is not tied to a credential. After switching tokens
  without `hear refresh`, names can come from the old workspace; ids are always
  the current ones.
- `search:read` exists only as a user token scope, so `hear search` requires
  the read app to be user-scoped.
- The channel and user lists are cached and reused: a channel created, renamed
  or archived afterwards won't appear, or will resolve wrongly, until
  `hear refresh`.
- `sez --edit` refuses a message carrying files, and `sez --delete` does not.
  A delete removes the message and the files' shares in it — the render names
  them, and says outright that they stay. **A message is a share of a file, not
  the file**: deleting it leaves the file in the workspace, in no conversation,
  reachable by id and counted against storage. This is Slack's behaviour rather
  than this tool's — deleting the same message through Slack's own interface
  leaves the file exactly as `chat.delete` does, which was checked both ways.
  Removing a file is Slack's Files browser, or `files.delete`, which this tool
  does not call. An edit is refused because it sends text alone, and
  whether Slack keeps the shares through one is not established — an edit that
  silently detached them is the failure the read-before-write gate exists to
  prevent.
- `sez --scheduled` and `sez --unschedule` see only messages scheduled through
  Slack's API. Slack refuses to cancel a scheduled message in the last 60
  seconds before it posts.
- `sez --edit` and `sez --delete` refuse outright if the `hear` they find is too
  old to report what a message carries. The two binaries are installed together
  but `sez` falls back to `PATH`, and an older `hear` is wrong in both
  directions — it can say a message is partial without saying why, or, older
  still, call one carrying files plain. Neither is distinguishable from a sound
  answer, so the missing field is the refusal.
- `hear canvas` takes a canvas link, a file id, or a conversation — a channel
  canvas hangs off the conversation rather than off any message in it, so a
  channel link, `#name` or id reaches it. A channel carrying more than one names
  them rather than choosing, since a canvas and a notes document are different
  documents. Slack surfaces one under `properties.canvas`, as a canvas tab, or
  as `meeting_notes`, and a real channel was observed using the last two and not
  the first, so all three are read.
- `hear canvas` renders the document, not the page around it. Slack serves a
  canvas as HTML and this converts it to Markdown: headings, lists, tables,
  links and inline marks survive; the ids, classes and styling do not. `--html`
  gives the bytes Slack sent, for a caller that would rather render them itself.
  A canvas is written by whoever can edit it, so its text is stripped of
  anything that could drive a terminal, exactly as a filename is — under
  `--html` too, which gives the markup Slack sent rather than the bytes. Link
  destinations are limited to ordinary schemes and encoded so that a canvas
  cannot forge one that says a different place from where it goes.
- `hear files` fetches only what Slack stores. A file living in Google Drive,
  Dropbox or another external store is a link in Slack's model, and following it
  would mean holding a credential for that service — which is a different
  program.
- A download may take ten times `HERSAID_TIMEOUT`, since a body of up to
  `--max-size` cannot be held to the bound that suits a small JSON reply. The
  bound is on the whole transfer, not on a stall within it, so a link that
  crawls fails late rather than early.
- `sez --file` is all or nothing. A failure before the sharing leg posts nothing
  at all — an outward message referring to an attachment that is not there is
  worse than no message. The ids of files that had already reached Slack are
  reported, and they say how far the send got and nothing more: **an upload that
  is never shared is discarded**, so there is nothing left to find or clean up.
  Measured, not assumed — `files.info` on such an id answers `file_deleted`
  straight away, to the read token and the write token alike.
- An upload is capped at 1,000,000,000 bytes and ten files per message. Slack
  documents its own limit as "1 GB" without saying which one, and the decimal
  reading is the one that cannot let through a file Slack will refuse.
- A transfer is allowed the greater of ten times `HERSAID_TIMEOUT` and one
  second per megabyte. The assumed floor is 1 MB/s; on a slower link, raise
  `HERSAID_TIMEOUT`, which is itself capped at one day so that every multiple
  taken of it stays in range.
- Uploads follow no redirect. Slack has not been observed to redirect one, and
  a hop is not something to discover by taking it.
- Windows is not supported: XDG paths, `0600` file modes and Unix signals
  throughout. WSL is a Linux environment and uses the Linux paths
  and the file credential fallback.

## Contributing, security and licence

- [CONTRIBUTING.md](CONTRIBUTING.md): how to build, test and send a change.
- [SECURITY.md](SECURITY.md): how to report a vulnerability privately.
- Apache License 2.0, see [LICENSE](LICENSE). The notices for the bundled
  dependencies are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
