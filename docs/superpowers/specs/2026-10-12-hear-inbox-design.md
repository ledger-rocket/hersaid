# hear inbox: what is new for me, in one read

## Problem

Catching up on Slack after time away takes several commands: `hear dms`, then
`hear read @person --since 14h` for each person, then `hear thread ...` for
every "⤷ N replies" line, then `hear read '#channel' --since ...` for each
channel worth watching. The step that gets skipped is following a thread. The
reply that matters often sits in a thread under a message the reader saw days
ago, and a transcript shows that only as a reply count, or not at all when the
parent is older than the window.

Scripts and agents that call hear meet the same problem, and they consume
`--json`.

## Goal

One read-only command answers "what is new for me since T". It covers:

- direct messages and group DMs;
- messages that mention me;
- new replies in threads I started or replied in, including threads older
  than the window, as far as Slack's search finds them (see "What complete
  means");
- new replies in threads I was mentioned in during the week before the window,
  read within a fixed share of the call budget;
- a short, explicit list of channels I watch (phase 2).

## Non-goals

- Slack's own unread state. The client's unread and mention feeds use
  undocumented `client.*` endpoints.
- A cursor that moves when read. Several callers sharing one cursor would each
  lose the items the others read.
- Routing items to people, tools or tickets. Callers do that with the JSON.
- Writing anything, including reactions or marking as read. `inbox` lives in
  the read binary, which has no write code path.
- Retries or backoff for any other verb. The 429 policy below is the inbox's
  own.
- Changing how `hear read` and `hear thread` render. Their text output does not
  pass through `termsafe` today; making it do so is a separate change.

## Phase 0: what the real API does (done)

A read-only spike ran against a real workspace with the read credential. It
printed key sets, counts and booleans only. No message text, name or id was
kept. These results decide the design. Each one the code depends on is pinned
by a test with fake data, which proves the code handles the recorded shape and
nothing more: a fake server cannot prove that Slack keeps answering this way.

**Search query syntax.**

- `<@U…>` and `@handle` give the same hits. Every hit carried my user id, in
  the text or, for bot messages, in `attachments`. So the mention search finds
  mentions inside attachments too.
- `from:<@U…>`, `from:me` and `from:@handle` give the same hits.
- `is:dm` returns every message in my 1:1 DMs and group DMs, from me and from
  others, including replies in DM threads. A group DM hit carries
  `channel.is_mpim: true` and `channel.is_private: true`; a 1:1 hit carries
  `channel.is_im: true`.
- `is:thread with:me` returns messages in threads I started or replied in.
  Every thread in a sample of 16 was one I had started or replied in, and every
  parent carried `subscribed: true`. It does not return threads where I was
  only mentioned (next paragraph).
- `after:YYYY-MM-DD` is exclusive and is read in the user's Slack time zone.
  `after:2026-10-09` started at 2026-10-10 00:00 local time.

**Old threads with new replies.** This is the case the first draft could not
find. Two samples, both checked against the full thread with
`conversations.replies`:

- A 2-day window: of 80 threads I had posted in during the previous 30 days, 3
  had a new reply from someone else. All 3 were among the `is:thread with:me`
  hits. In 2 of them I had written nothing inside the window.
- A 7-day window: of 108 threads I had posted in 8 to 60 days before, 3 had a
  new reply from someone else, and all 3 were among the hits. Of 60 threads
  where I had been mentioned but had never posted, 13 had a new reply from
  someone else, and none of the 13 was among the hits.

So `with:` matches the threads I started or replied in, and not the threads I
was only mentioned in. In this sample the second kind was the larger one, so
phase 1 looks for it separately: a search for the mention over a lookback, and
a read of each thread it names (step 7 below).

This is evidence, not proof. Slack does not document what `with:` matches, and
a thread it leaves out is invisible to the inbox. The README says so.

**Thread identity on search hits.**

- Hits have no `thread_ts` field. Key set of a hit: `blocks, channel,
  db_message, iid, no_reactions, permalink, score, team, text, ts, type, user,
  username`, plus `attachments`, and `files` when the message has files (90 of
  100 `has:file` hits carried it).
- The `permalink` carries `?thread_ts=` for every message in a thread. For a
  reply it differs from `ts`. For a parent that has replies it equals `ts`. A
  message outside any thread has no `thread_ts` in its permalink.
- `cmd/hear/address.go`'s `parsePermalink` already reads that shape, so the
  thread of a hit comes from its permalink.

**Search paging.** `messages` holds `matches`, `paging` (`count, page, pages,
total`) and `pagination` (`first, last, page, page_count, per_page,
total_count`). `count=100&sort=timestamp&sort_dir=desc` works.

**`conversations.replies` with `oldest`.** The parent is returned first even
when `oldest` is after it, and even when `oldest` is in the future, in which
case the parent is the only message. `oldest` is exclusive unless
`inclusive=true`. So `replies(ts=T, oldest=since)` gives the parent plus
exactly the new replies, in one call. A parent carries `latest_reply`,
`reply_count`, `reply_users`, `subscribed` and `last_read`.

**DM lists.** `conversations.list` and `users.conversations` return IM objects
with `context_team_id, created, id, is_archived, is_im, is_org_shared,
is_user_deleted, priority, updated, user`. There is no `latest`. A DM list
cannot say which DMs have new messages, so the inbox does not use one.

**Bounds and lookback.** `<@U…> is:thread after:… before:…` works: 68 hits,
all inside the bounds, every one naming a thread in its permalink, 45 of them
parents (`thread_ts == ts`). So a mention in a thread's first message is found
too. `conversations.replies` sent `oldest` and `latest` together still returned
the parent first when it was older than `oldest`, on all three threads tried.

**Rate limits.** `conversations.history` with `limit=100` returned 31
messages, more than the 15 a reduced-tier app gets, and three calls back to
back were all answered. 25 `search.messages` calls back to back, in about 4
seconds, were all answered with HTTP 200. The read app is not on a reduced
tier. The inbox still handles 429, because other workspaces and apps may be.

**Identity.** `auth.test` returns `user_id`, `user`, `team`, `team_id` and
`url`.

## Command

```
hear inbox [--since D] [--max-calls N] [--json] [--ts]
```

- `--since D`: the window, with the syntax `hear read --since` takes (30m,
  14h, 2d). Default 24h.
- `--max-calls N`: the most requests one run makes to Slack for the inbox
  itself, retries included. A positive whole number, at most 1000. Default 60.
  Cache fills are counted separately and are not capped by it (see "The call
  budget").
- `--json`: one object, `kind: "inbox"`.
- `--ts`: prefix each message with its timestamp, as `hear read` does.
- `inbox` takes no positional argument.

`--from` and `--mentions-only` from the first draft are cut. The JSON carries
what a caller needs to filter on (`user`, `mentions_me`), and each flag added
combinations to define.

`--max-calls` given to any verb but `inbox` is a usage error. `--since` stays
valid for `read` and becomes valid for `inbox`; for every other verb it is
still a usage error, now worded "--since applies to read and inbox".

## The window

The run reads the clock once. `until` is that instant truncated to the whole
second, and `since` is `until − D` truncated to the whole second, and never
earlier than 1970-01-01: a negative timestamp would order after every real one.
The window is whole seconds on purpose. The part of a Slack timestamp after the
point is a sequence, not a fraction of a second, so microsecond bounds would
claim a precision the timestamps do not have; the second at each end that the
truncation moves is covered by the next run, whose window overlaps it. The window is
`since < ts < until`, compared as Slack timestamps (seconds, then the sequence
after the point), and every message the inbox keeps is checked against it
locally, whatever the endpoint was asked. `conversations.replies` is also sent
`oldest = since` and `latest = until`, so Slack does the same cut.

The one exception is a thread's parent. It is context: it is kept whatever its
timestamp, so an old thread shows what it is about, and it counts toward the
message ceiling. It is never news by itself: a parent at or before `since` does
not make its thread news, however it is written.

A timestamp that does not read as digits, a point and digits cannot be ordered
or windowed. A search hit with one is skipped and its source is marked
incomplete (`hit without a timestamp`); a read that returns one stops there.

## What a run does (phase 1)

Steps run in this order.

1. **Identity.** "Me" is the cached identity when its credential key matches
   the credential in use. Otherwise inbox calls `auth.test` once and writes the
   result through, as `whoami` does. This is the one read that fetches the
   identity. It has to: the mention query cannot be written without my user
   id. A failure here is fatal.
2. **Names.** The user list, from the cache, filled with `users.list` when
   cold, as every transcript already does. A failure here is fatal.
3. **Discovery.** Four searches, each `count=100`, `sort=timestamp`,
   `sort_dir=desc`, run in this order before any thread is read:
   - `dms`: `is:dm after:<date>`
   - `mentions`: `<@ME> after:<date>`
   - `threads`: `is:thread with:me after:<date>`
   - `mention_threads`: `<@ME> is:thread after:<lookback date>
     before:<end date>`, the threads I was mentioned in during the week before
     the window (step 7).

   `<date>` is the UTC date of `since` less two days, `<lookback date>` the
   same for `since − 7d`, and `<end date>` the UTC date of `since` plus two
   days. Two days, not one: `before:` is exclusive, and `since` falls on the UTC
   date plus one in every zone east of UTC, so a one-day margin would drop that
   day. The cost is that a window shorter than two days is paged through by the
   lookback too, one shared call per hundred thread mentions in it. `after:` and `before:` are exclusive and read in the user's time zone,
   which can be up to 14 hours from UTC, so two days covers every zone. The
   `before:` bound keeps the lookback from paging through the window, which the
   `mentions` search already covers. Each search's range is `since < ts <
   until`, except `mention_threads`, whose range is `since − 7d < ts ≤ since`:
   it ends where the window starts, so nothing the window's searches cover is
   read again on the lookback's share.
   Hits outside the range are dropped. A hit whose `channel.id` is not shaped
   like a conversation id is skipped, and the source is marked incomplete with
   `hit without a channel`.

   Each search pages until one of these happens:
   - a page holds a hit at or before the start of its range (complete: the
     results are newest first, so the rest are older);
   - Slack's last page (`paging.page >= paging.pages`) is read, or the search
     has no results at all (`paging` present, `paging.total` 0) (complete);
   - a page is empty while `paging` says more follow, or is empty with no
     `paging` at all (incomplete: `empty page before the last`);
   - 10 pages are read (incomplete: `page limit`);
   - the retained-message ceiling is reached (incomplete: `message limit`);
   - the call budget is spent (incomplete: `call budget`);
   - Slack refuses (incomplete: the error code, e.g. `missing_scope`).

   Hits already kept stay kept when a source stops early.
4. **Placing hits.** The first three searches' hits are placed; the lookback's
   only name threads (step 7). Every placed hit becomes a message keyed by
   `(channel id, ts)`.
   The first source to see a key places it; later sightings of the same key are
   dropped. A hit's conversation kind comes from `channel.is_im` (dm) and
   `channel.is_mpim` (group dm); anything else, including a hit with neither
   flag, is a channel. Its thread comes from its permalink:
   - `thread_ts` absent: a top-level message of its conversation;
   - `thread_ts == ts`: the parent of that thread, which becomes a target;
   - `thread_ts != ts`: a reply in that thread, which becomes a target;
   - a permalink that is missing or does not parse: a top-level message of
     its conversation. When the hit came from the `threads` source, that source
     is marked incomplete with the reason `hit without a thread`, because every
     hit of `is:thread` should name one.

   A thread's parent is filed as its parent in whichever order the two arrive:
   a top-level hit whose ts is a known thread's ts, or a thread whose ts is
   already a top-level hit, moves the message into the thread. So a thread
   whose read is refused still lists its parent once. The permalink's channel
   must be the hit's `channel.id`, or the permalink is treated as missing.
5. **Not news.** A thread is news when a message of it inside the window is
   not from me. A conversation is news when one of its top-level messages
   inside the window is not from me, or one of its threads is news. Anything
   that is not news is dropped and costs no read. The rule runs again after
   reading, on what was read. My own messages stay inside kept groups, as
   context.
6. **Reading threads.** Each thread target is read with
   `conversations.replies` (`ts = thread_ts`, `oldest = since`,
   `latest = until`, `limit = 200`), paged, up to 1000 replies or 6 pages per
   thread, whichever comes first. The page bound is what stops a server that
   repeats one cursor and one page, which adds no new message.
   Targets are read in this order: threads in DMs and group DMs, then threads
   holding a hit from the `mentions` source, then the rest. A thread is read
   even when search already returned its replies: the read is what brings an
   old parent, and it catches a reply the index has not seen yet. Within each class:
   newest kept message first, then channel id, then thread ts.

   What a read returns is merged with the search hits for that thread by key:
   - the message whose ts is the thread ts is the parent, wherever it appears
     and however often;
   - every other message inside the window is a reply, including a broadcast
     (`subtype: thread_broadcast`), which belongs to its thread and never to
     the conversation's top level;
   - for a key both sources hold, the read's copy wins;
   - a search hit that a whole read did not return is dropped, the parent
     included: a read to the end of the window is the authority on what the
     thread holds now, and a hit it left out was deleted, or edited, since the
     index saw it.

   A read that stops early (an error, the budget, a 429, the 1000-message cap
   or the message ceiling) keeps what it got and every search hit, and marks
   the thread incomplete with the reason.
7. **Threads I was mentioned in before the window.** Each thread the
   `mention_threads` search named, and that none of the other three found, is
   read like any other thread, newest mention first, after every other target.
   Its share is a quarter of `--max-calls`, at least one, counted in requests:
   pages and retries count, threads do not.
   - The share is reserved. When the lookback found threads, the reads of
     step 6 stop at `--max-calls` less the smaller of the share and the number
     of lookback threads, with the reason `call budget`. What step 6 leaves
     unused goes to the lookback, still up to its share.
   - The lookback's search pages are spent in step 3, from the shared budget,
     not from the share. It keeps only where each thread is, never the hit's
     message, so it holds nothing the message ceiling does not count.
   - A thread whose read shows a reply from someone else inside the window is
     shown; the rest are dropped as not news.
   - A thread not read to the end, by the share, the budget, a 429, the
     deadline or an error, is listed in the source's `unread` with its channel,
     thread ts and reason, even when what was read held nothing new, since what
     was not read might. The source is then incomplete, with the reason
     `N of M threads not read: <reasons>`. So `complete` on this source covers
     both its search and its reads, and `unread` says exactly which threads a
     caller can read itself.

   The spike saw about one such thread a day, so the week costs about 7 reads.
8. **Output.** See below.

DM and group DM messages are not re-read with `conversations.history`. The
`is:dm` search already returns every one of them inside the window, threads
included, and a history read would spend a call per DM to fetch the same
messages again. A DM's top-level messages are therefore search hits
(`source: "search"`); its threads are read like any other.

A mention outside any thread is shown as search returned it. It needs no
further call.

### The call budget

`--max-calls` is enforced at the HTTP transport, in a `RoundTripper` that wraps
the one `slackhttp` builds, so no call site can forget it. It counts every
request the inbox makes: `auth.test`, each search page, each replies page and
each retry. When the budget is spent, the transport refuses the next request
without sending it, and the refusal is a sentinel error each step recognises.

Cache fills (`users.list` here, `conversations.list` in phase 2) are made with
a context the transport counts separately and does not charge: `calls.cache` in
the JSON. They are already bounded by the existing 1000-page cap, they are paid
once and then shared by every command, and charging them would make the first
run after `hear refresh` fail in any workspace with more than a few thousand
members, every time, since a fill that does not finish writes nothing.

Identity always fits: it is the first request, and the budget is at least 1. A
budget too small for discovery gives an inbox whose sources say `call budget`.

The budget counts the requests the inbox hands the transport. Go's transport
can resend a request by itself when a reused connection fails under it; that
resend is not counted. It happens only after a connection failure, so it does
not compound, and the budget is a bound on what the inbox asks, not a count of
packets.

### Memory

The inbox holds at most 5000 messages in all, the ceiling `hear read --since`
already uses for one window. It counts what is held, not what was asked for:
each distinct `(channel id, ts)` once, a thread's parent included, whichever
search or read brought it. What is dropped, as not news or by a whole read,
stops counting at once: the not-news rule runs after every thread read, so a
quiet thread holds no room the reads after it need. A search that would add one past the ceiling stops
there; so does a read, and so does a read that would hold more than 1000
replies for one thread. Each stops with the reason `message limit`, and what
was held before it stays. Each response is already capped at 32 MiB by
`slackread`.

### 429 handling

- `slackread`'s request path returns a typed `RateLimitedError` on HTTP 429,
  before it decodes the body, carrying the method and the `Retry-After` value in
  whole seconds. A missing or unreadable header is zero; a huge one is clamped
  to a day. The body is drained, up to 4 KiB, so the connection can carry a
  retry.
- Its text is exactly `<method>: ratelimited`, followed by
  ` (retry after <d>)` when the wait is known, `<d>` in Go's duration form
  (`7s`). Every verb prints that text on a 429 and makes no second request.
  Before, a JSON body gave `<method>: ratelimited` and any other body gave
  `<method>: malformed response (HTTP 429)`; the second changes on purpose,
  since the status, not the body, says what happened. Tests pin the text for
  `hear read` and `hear search`, and that each makes exactly one request.
- A 200 whose body says `ratelimited` is Slack's ordinary refusal, carries no
  `Retry-After`, and stays an `APIError`.
- Inbox makes each search page and each replies page itself, so a retry repeats
  exactly the refused request, cursor and page included. It retries once, after
  waiting `Retry-After`, when all of these hold: the value is between 1 and
  30 seconds, the total wait in the run stays at or under 60 seconds,
  and the budget, and the stage's part of it, have a call left.
- When the wait is out of that range or would pass the total, and after a
  second 429 on the same request, the method is blocked for the rest of the
  run. Its pending pages and targets are marked
  `rate limited` without a call.
- A 429 during a cache fill or `auth.test` is not retried. It fails the run like
  any other error there, as it would fail `hear read`.
- The wait honours the context and goes through an injected sleeper, and the
  clock is injected too, so tests take no real time.
- A 429 that arrives when the budget, or the part of it a stage may use, is
  spent is not retried, and does not block the method: the run's own bound
  ruled the retry out, not Slack, and a later stage with a share of its own may
  still ask.
- Inbox reports what it saw, as the reason `rate limited`, and does not guess
  why.

### Run deadline

The run has two deadlines of 3 minutes each, on top of the per-request
timeout (`HERSAID_TIMEOUT`, default 20 s): one for setup (resolving the
credential, the identity and the user list), and one for everything after it.
Resolving the credential is inside it because a keyring with nobody to answer
can block; a resolution that outlives the deadline is left to finish while the
command exits. They are separate so a slow cold user
list cannot eat into the searches and reads, and so setup itself cannot run
unbounded: 1000 pages each waiting out a 20-second timeout would otherwise be
hours. A setup that runs out of time fails the run, as any setup failure does.
A source or target still pending when the second deadline passes is marked
`deadline`, and no request is made after it. "Deadline" is decided by the run's own
context, never by the error a request returned, since a request that hit its
own timeout carries the same error. The duration comes in with the clock, so a
test can reach it without waiting.

Without it, 60 requests that each hang to their timeout would take 20 minutes,
which an agent waiting on the answer cannot tell from a hang.

### Failures

- **Fatal, exit 1, no inbox printed:** no credential; identity or names not
  established (any error, a 429 included); an identity whose id is not shaped
  like a user id (`U` or `W`, then capitals and digits), since it goes into a
  query; an auth error (`invalid_auth`,
  `not_authed`, `token_revoked`, `token_expired`, `account_inactive`) from any
  call. `slackread` exports its API error as `APIError{Method, Code}` with an
  `IsAuthError` predicate, so this is decided by code, never by matching text.
- **Per source:** any other error ends that search. The source shows
  `complete: false` and the reason.
- **Per thread:** any other error ends that read. The thread keeps what it
  has, with `complete: false` and the reason.

### What complete means

Completeness is defined at four levels, and each is only as strong as its
definition:

- A **source** is complete when its search covered the window: a page reached
  a hit at or before `since`, or Slack said there were no more pages.
- A **thread** is complete when it was read to the end of the window. A thread
  that is still only search hits is not complete.
- A **conversation** is complete when all its threads are, and, for a DM or
  group DM, the `dms` source is complete. A conversation's `reason` is set when
  its own part is not complete; a thread that is not complete carries its own.
  For a channel, `complete` covers the reads made for it, not whether a search
  that stopped early would have found more of its messages: that is the
  source's `complete`, and the inbox's.
- The **inbox** is complete when every source, conversation and thread is.

"Complete" is complete over what Slack returned. It cannot mean "nothing was
missed", for two reasons the inbox cannot detect:

- Slack's search index lags behind posting, by an amount Slack does not state,
  so a message posted moments before the run can be absent.
- A thread `is:thread with:me` does not match, and that I was not mentioned in
  during the lookback, is never seen. Among threads I posted in, the spike found
  none missing, on the samples above, from one account in one workspace.

The README states both.

### Exit status

0 whenever the inbox is printed, complete or not, as `hear read --since`
already exits 0 with `complete: false`. 1 for the fatal failures above, with
nothing on stdout. 2 for usage. A caller that needs the whole answer checks
`complete`; the text form ends with an `incomplete:` section whenever it is not.

## Output

### Grouping and order

- A message appears once, keyed by `(channel id, ts)`.
- A thread's parent is in the thread, never also in the conversation's
  messages. A reply is in its thread.
- Conversations are ordered by their newest message, newest first. Ties go by
  channel id.
- Inside a conversation, top-level messages and threads are ordered by `ts`
  (a thread by its thread ts), oldest first, as a transcript reads.
- Replies are oldest first.

### Text

```
@bob  (dm)
thread 11-03 08:26  me  the plan for the release …  (hear thread D0000000001 1699000000.000100)
    11-15 14:53  bob  done
11-15 12:06  bob  can you look at this?
#general
thread 11-09 03:20 (parent not returned)  (hear thread C0000000001 1699500000.000100)
    11-15 17:40  carol  @me can you sign off?
incomplete:
  search threads: call budget
  thread #general 1699600000.000100: call budget
```

- A conversation header is `@person  (dm)`, `<group dm name>  (group dm)` or
  `#channel`; the id when there is no better name.
- A thread's parent is cut to one line of at most 80 runes, with `…` when cut.
  A missing parent prints as `thread <time> (parent not returned)`.
- A thread is always printed with its replies. No line says "N new replies"
  without them.
- Message text goes through `termsafe.TextKeepingNewlines`, and every other
  string from Slack (names, the parent line, file names) through
  `termsafe.Text`. A multi-line message continues on indented lines, as in a
  transcript.
- `incomplete:` lists every source, conversation and thread that is not
  complete, one per line, with its reason.

### JSON

```json
{
  "kind": "inbox",
  "since": "2023-11-14T22:13:20Z",
  "until": "2023-11-15T22:13:20Z",
  "user_id": "U0000000001",
  "complete": false,
  "calls": {"used": 60, "max": 60, "cache": 0},
  "sources": [
    {"name": "dms", "query": "is:dm after:2023-11-12", "complete": true, "hits": 4},
    {"name": "mentions", "query": "<@U0000000001> after:2023-11-12", "complete": true, "hits": 1},
    {"name": "threads", "query": "is:thread with:me after:2023-11-12", "complete": false, "hits": 100, "reason": "call budget"},
    {"name": "mention_threads", "query": "<@U0000000001> is:thread after:2023-11-05 before:2023-11-16", "complete": false, "hits": 3, "reason": "1 of 3 threads not read: share of the call budget",
     "unread": [{"channel": {"id": "C0000000002"}, "thread_ts": "1699600000.000100", "reason": "share of the call budget"}]}
  ],
  "conversations": [
    {
      "channel": {"id": "D0000000001"},
      "kind": "dm",
      "user": "U0000000002",
      "name": "bob",
      "source": "search",
      "complete": true,
      "messages": [],
      "threads": [
        {
          "thread_ts": "1699000000.000100",
          "parent": {"ts": "1699000000.000100", "author": "me", "text": "…", "mentions_me": false},
          "replies": [],
          "source": "replies",
          "complete": true
        }
      ]
    }
  ]
}
```

- `kind` on a conversation is `dm`, `group_dm` or `channel`. `channel.name` is
  set for channels and group DMs. `user` is set for a 1:1 DM: the author of any
  message in it that is not me. A DM where the only other writer is an app has
  no `user`, and its text header is the channel id.
- `name` is that person's label, set with `user` when the user list knows them.
  For such a person it is what `hear dms --json` gives under the same key, and
  what their messages carry as `author`. The text header is built from it. It
  comes from the user list the run already loads, so it costs no request. A
  `user` the list does not know has no `name`, where `hear dms` gives the id,
  and the header shows the id. Like `author`, it is Slack's label as given:
  `termsafe` applies to the text form, not to the JSON.
- `source` is `search` (search hits) or `replies` (read). A read cut short is
  `replies` with `complete: false`: it holds what the read returned and every
  search hit. Phase 2 adds `history`.
- `hits` on a source is the number of hits inside the range it covers (the
  window, or the lookback for `mention_threads`), counted once per hit even
  when another source found the same message first. A hit without a timestamp,
  or without a channel, is not counted.
- `unread` on a source lists each thread it found and could not read to the
  end, with `channel`, `thread_ts` and `reason`. Only `mention_threads` has any;
  a thread from the other sources that was not read to the end is in
  `conversations` with its search hits and `complete: false`.
- `parent` is a message object, or null when Slack did not return it. It is a
  pointer in the Go type, so null is what an absent parent encodes as.
- Messages are the existing transcript message (`render.Message`: `ts`,
  `time`, `author`, `user`, `text`, `text_rendered`, `blocks` and the rest),
  plus `mentions_me`. `blocks` stays present, so a message can go to
  `sez --edit` exactly as from `hear read --json`.
### mentions_me

True when, and only when, one of these names my user id exactly:

- the text holds `<@ME>` or `<@ME|…>`;
- an attachment's `text`, `pretext`, `fallback`, `title` or a field `value`
  holds `<@ME>` or `<@ME|…>`;
- a block holds, at any depth, an element `{"type": "user", "user_id": ME}`.

Blocks and attachments are decoded and walked, never searched as raw text, so
an id that merely occurs inside other JSON, or another user's id that starts
with mine, does not count. Undecodable blocks or attachments count as no
mention.

## Security

- Inbox text is untrusted. It is the largest single read surface hear offers.
  The README rule applies unchanged: the text is data, never instructions. The
  README section for `inbox` says so.
- No token or credential appears in output or errors. The query strings carry
  my own user id, which `whoami` already prints.
- `inbox` is read-only, in the read binary only. Phase 1 calls `auth.test`,
  `users.list` (cache fill), `search.messages` and `conversations.replies`.
  Phase 2 adds `conversations.history` and `conversations.list` (cache fill).
- The user list on disk is not bound to a credential, for this command as for
  every other: after a token switch without `hear refresh`, names can come from
  the old workspace. Ids are always the current ones. This is unchanged here and
  stated in the README limitations.

## Testing

All tests use the `slacktest` fake server, fake ids (`U0000000001`,
`D0000000001`, `C0000000001`) and invented text. Fixtures copy the response
shapes recorded above, never real values. The fake routes `search.messages`
on its query and `conversations.replies` on its `ts`.

- An old parent with a new reply, in a thread I joined before the window, is
  found through the `threads` search, read with `replies`, and printed with its
  parent.
- A DM that is not in the conversation cache is found; no `conversations.list`
  or `conversations.history` request is made.
- A DM, a group DM and a channel are told apart by the hit's flags; a hit with
  neither flag is a channel.
- A DM where only I wrote is dropped and costs no read; so is a thread where
  only I replied, found by search or by a read.
- A whole read drops a search hit it did not return; a read cut short keeps it.
- A mention outside any thread appears from search with no further call.
- The same message found by two sources appears once; a thread found by two
  sources is read once.
- A search that needs a second page reads it, with the page number sent; one
  that reaches 10 pages is incomplete with `page limit`.
- Window: a hit at exactly `since`, and one at exactly `until`, are outside; a
  hit at `since` plus one sequence step is inside.
- The `after:` date is right when `since` falls just after midnight UTC and just
  before it.
- `replies`: the parent is first and not counted as a reply; a parent repeated
  on page 2 is kept once; a missing parent gives `parent: null` and the text
  line; a broadcast reply lands in its thread; a read failing on page 2 keeps
  page 1 and the search hits, incomplete.
- Permalinks: a `threads` hit with no permalink, or with a bad `thread_ts`, is a
  top-level message and marks that source incomplete.
- Identity: warm and matching makes no `auth.test`; cold, or another key's,
  makes one and writes it through; a failing `auth.test` exits 1 with nothing on
  stdout.
- Budget: the fake server never sees more charged requests than
  `--max-calls`; `calls.used` equals what it saw, less the cache fills, which
  `calls.cache` counts; a cold user cache with two pages still fills under
  `--max-calls 1`.
- A budget that runs out leaves the unread threads as search hits, named in
  `incomplete`.
- 429: `Retry-After: 2` is waited for once on the fake clock and the same
  request succeeds; a second 429 blocks the method; a value above the per-wait
  cap is not waited for; the total-wait cap stops a third wait; a 429 on
  `users.list` is fatal.
- Each of the five auth codes is fatal from search and from replies; a
  `channel_not_found` on one thread is not; `missing_scope` on one search marks
  only that source.
- `mentions_me`: positive in text, in each attachment field and in a nested
  block; negative for another user's id, for a longer id that starts with mine,
  for my id inside unrelated JSON, and for malformed blocks.
- The set of endpoints called is a subset of the four phase 1 lists.
- Output order, tie-breaks and the JSON shape are asserted on a fixture with a
  DM, a group DM and a channel.
- Hostile text (ESC, BEL, U+202E, an embedded newline) in a name, a message and
  a parent prints without the control characters.
- Exit is 0 for complete and incomplete fixtures, 1 for each fatal one.
- A parent before `since`, and one at exactly `since`, are shown as context and
  make no thread news on their own.
- The ceiling counts held messages once each, a parent included; a read that
  would pass it, or a thread at 1000 replies, stops with `message limit`.
- An empty page while Slack says more follow leaves the source incomplete; a
  search with no results is complete.
- A hit with no readable timestamp is skipped and named.
- The deadline: with a run deadline of a nanosecond, setup still runs, no
  search is made, and every source says `deadline`; a credential resolution
  that blocks past the setup deadline ends the run with exit 1 and no request.
- `--since 3000w` starts the window at 1970 and still finds what is in it.
- An empty page with no `paging` leaves its source incomplete.
- Quiet lookback threads free their room before the next is read: with a
  ceiling of three messages, two quiet threads and one with news, the one
  with news is kept and nothing is unread.
- A thread's parent found as a top-level hit is listed once when its read is
  refused, whichever of parent and reply came first.
- A thread read whose server repeats one cursor stops after 6 pages with
  `page limit`.
- A whole read that does not return a parent search found drops it.
- A hit with a channel id that is not one is skipped and named.
- `mention_threads`: a mention inside the window is not counted or read on
  the lookback's share, and one at exactly `since` is; a 429 at step 6's limit
  does not stop the lookback's reads; a thread read in part is listed in
  `unread`; a lookback
  hit naming no thread is a flaw; the share counts requests (two two-page
  threads under `--max-calls 8` read one thread and list the other); the share
  is reserved from the other reads; a DM found only by the lookback carries the
  `dms` source's reason.
- `hear read` and `hear search` on a 429 print `<method>: ratelimited (retry
  after 7s)`, exit 1, and make one request.
- `mention_threads`: a thread with a new reply is shown and one without is not;
  a thread another search found is not read twice; with `--max-calls 8`, two
  of three threads are read and the source names the third.
- An identity whose id is not shaped like a user id ends the run before any
  search.
- `--max-calls=N` is the same as `--max-calls N`; `0`, `-1`, `lots`, `1001` and
  a missing value are refused; `--max-calls` on another verb and an argument to
  `inbox` are usage errors.

## Implementation (phase 1)

Build order: `slackhttp` and `slackread` first, then `render`, which uses the
new `slackread` fields, then `cmd/hear`. The grouping logic stays in `cmd/hear`
beside the other verbs; it is split into functions small enough for the lint
limits (cyclop 10, funlen 120), and tested through the command against the fake
server.

- `internal/slackhttp`: `Budget` with `NewBudget`, `Wrap`, `Used`, `Max`,
  `Remaining` and `ErrBudgetSpent`; `Uncharged(ctx)`, a context that the
  wrapped transport counts in `Cache()` without charging.
- `internal/slackread`:
  - `SearchMatch.Channel` gains `IsIM` and `IsMPIM`. Nothing else is decoded
    that the inbox does not read.
  - `SearchPage(ctx, SearchOptions)` returns matches and paging. `Search`
    keeps its signature, its request fields and its behaviour for
    `hear search`.
  - `get` returns `RateLimitedError{Method, RetryAfter}` on HTTP 429, before it
    reads the body.
  - `apiError` becomes the exported `APIError{Method, Code}`, with the same
    text, and `IsAuthError(err)`. `errors_text_test.go` builds the type by
    name and changes with it; its assertion does not.
- `internal/render`: `KindInbox`, the inbox types, `WriteInbox` and
  `TSLess`.
- `cmd/hear`: the `inbox` verb, `--max-calls`, the `--since` gating and its
  wording, the usage text, an injected clock, sleeper and deadline, the budget
  wired into the source.
- `internal/cache`: the `Identity` comment, since inbox now fetches it.
- `README.md`: the verb, the JSON kind, the exit rule, the 429 policy, the run
  deadline, the mention lookback and its share, and the limitations: search
  lag; threads `with:` does not match and the lookback does not reach;
  `@here`, `@channel` and user-group mentions are not matched; names after a
  token switch.

## Phase 2: watched channels

This document specifies phase 1 for building. This section records the
decisions phase 2 has so far, and phase 2 gets its own design pass before it is
built. That pass has to give it an output model: a `watch` list with one entry
per address, carrying the address as given, the channel it resolved to when it
did, `complete` and `reason`, since an address that does not resolve has no
place in `conversations`. It also has to give the history reads their paging,
limits, priority and tests.

- The watch file is `$XDG_CONFIG_HOME/hersaid/watch`, else
  `~/.config/hersaid/watch`, the same rule as the credential file. It is
  absent by default; an absent or empty file is an empty list. An unreadable
  one fails the run.
- Grammar: lines are split on `\n`, a trailing `\r` is dropped, and surrounding
  whitespace is trimmed. Blank lines are ignored. A line that is `#` alone or
  starts with `# ` is a comment, so `#eng` is a channel and `# eng` is a
  comment; there are no inline comments. Every other line is one address in a
  form `hear read` takes, except a permalink or `@person` (DMs are always
  included), which are refused with the line number. An address that does not
  resolve is a per-target error, not dropped. Duplicates, by resolved id, are
  read once.
- `--channel ADDR`, repeatable, adds to the watched set for this run only. It
  never writes the file. Duplicates across file and flag are read once.
- Each watched channel is read with `conversations.history` over the window,
  after every phase 1 target. Top-level messages in the window with replies
  add their threads, last in priority.
- A watched channel's old thread that I am not part of is not found. A
  discovery lookback (`history` over N days, keeping parents whose
  `latest_reply` is inside the window) can close that later, at a cost per
  channel.

## Later

- A flag for the mention lookback's length, if a week proves too short.
- An absolute `--since` (RFC 3339 or a Slack timestamp) for callers that keep
  their own state, with advice to overlap windows and drop duplicates by
  `(channel id, ts)`.
- The spike again on a second account in a busier workspace, before the README
  claim about `with:` is strengthened.
- A cross-check for thread recall: `from:me is:thread` over a lookback gives
  thread ids, and any id the `threads` source missed gets a `replies` read. It
  turns "trust the query" into something a run can measure.
- `urls` on each message, from the links Slack delimits in the text.
- Permalinks built from the workspace `url` that `auth.test` returns.
- Thread read state from `last_read` and `subscribed`, which parents carry for
  a user token.
- Named cursors with an explicit `--mark`.
