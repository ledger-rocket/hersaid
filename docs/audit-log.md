# Audit log

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

## sez records

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

## tap records

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

## Reading it

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
