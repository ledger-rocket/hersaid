# Usage

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

## hear (read-only)

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
[No ambient configuration can alter a request](safety-model.md#no-ambient-configuration-can-alter-a-request).

Without `files:read` Slack answers a file request with HTTP 200 and a sign-in
page rather than an error. `hear files` refuses a web page where a file was
expected and names the missing scope, so that page never lands as a `.pdf`.

`hear refresh` deletes every `*.json` in `~/.cache/hersaid/`; the next command
that needs the lists rebuilds them. `channels`, `dms` and `refresh` are
answered from the cache when it is warm, and resolve no credential at all.

## hear inbox

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

## hear --json

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

## sez (post, with audit log)

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

## tap (reactions, with audit log)

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
