# `sez --delete-file` — remove a file from Slack

## Goal

Two situations leave a file in Slack that nothing in this tool can remove.

**A staged upload nobody shared.** `sez --file` stages bytes on legs 1 and 2 and
shares them on leg 3. When leg 3 never runs, the file objects exist in the
uploader's storage and appear in no conversation. `sez` prints their ids and
deliberately deletes nothing.

**A file whose message was deleted.** Verified against real Slack: `chat.delete`
removes the message and its file *shares*, and the file object survives.
`files.info` still returns it, with `channels: []` and no shares.

Both leave something that exists, that the uploader owns, and that no verb here
addresses. `files.delete` is the endpoint; it takes a file id and the
`files:write` scope the write app already holds.

**No new scope.** An enrolment that can upload can already delete — the
capability is in the token today, and nothing but the absence of a verb stops it.
This change does not widen what the credential can do. It moves an existing
capability onto the audited path, behind the render, where every other
destructive act in this tool already sits.

## CLI surface

```
sez [--send] --delete-file <F…|file permalink>
```

- One file per invocation. Each delete is separately irreversible, so each gets
  its own render and its own `--send`. Three renders followed by one `--send` is
  exactly the "did I check all of them" ambiguity the render exists to remove.
- No conversation argument. A file is not addressed by conversation, and
  inventing one would ask the caller to name a channel the command does not use.
- Excluded from `--send`-less action like every other verb: without `--send`
  nothing is called.
- `--delete-file` with `--delete`, `--edit`, `--file`, `--thread` or `--md` is a
  usage error. None of them describe a file, and a flag accepted and dropped
  teaches that it did something.

The argument is a file id, or a Slack file permalink —
`https://<team>.slack.com/files/<U…>/<F…>/<name>`, and `/docs/<team>/<F…>` for a
canvas. `cmd/hear/canvas.go` already host-checks a Slack link and extracts a file
id from a path; the same two functions serve, extended by one pattern. The
permalink is what a person copies out of the Slack UI, and requiring them to
first extract an id by hand is a step that buys nothing.

A **message address** is deliberately not accepted. `sez --delete-file '#eng'
<ts>`, offering that message's files, turns one destructive target into a
chooser, and a chooser needs either interactive selection or an index argument.
The composition already exists: `hear read #eng --ts` and `hear thread` name the
file ids on each message, and one of them is what gets passed here.

## The read gates the delete

`sez` holds a write credential and the write app has no history scopes, so it
cannot see what it is about to destroy. It already solves this for `--edit` and
`--delete` by running the `hear` binary — a separate process holding its own
credential, with `SLACK_WRITE_TOKEN` stripped from its environment. The same
mechanism, unchanged, gates this.

It is not enough to hope the write app happens to hold `files:read`. That grant
is incidental, revocable, and would put a read scope on the token whose whole
point is that it cannot read. The read stays on the read binary.

**If the file cannot be read, the delete is refused** — under `--send` too, not
warned about. A tool that cannot show what it is destroying has not earned the
call. That is the rule `--edit` and `--delete` already follow, and the reasons
are stronger here: what a message delete destroys is one message the render can
print in full, while what a file delete destroys is a thing visible in a set of
conversations that only the read side can enumerate.

### `hear file <F…>`

A new read verb, because no existing one answers this question.

- `hear files <address> <ts>` needs a message address and writes bytes to disk.
  A file addressed by id has no message, and bytes are not what is wanted.
- `hear canvas F…` prints a canvas's content, for canvases only.
- `slackread.FileInfo` already exists — `canvas` uses it — and is exposed on no
  command line.

```
hear file <F…|file permalink>        print one file's metadata
hear file <F…> --json                {"kind": "file", …}
```

Human form for a person asking "what is F07ABC123", JSON for `sez`. The JSON
carries the file's id, name, title, mimetype, size, mode, created time,
uploader id and resolved uploader label, permalink, and **every conversation the
file is shared in** — each with its id, its resolved name when the cache knows
it, and the share timestamp. It also carries the read credential's own user id,
so a caller can tell whether the file belongs to the account that would delete
it without paying a second `auth.test`.

`slackread.File` grows the fields `files.info` returns that a download never
needed: `created`, `user`, `permalink`, `channels`, `groups`, `ims`, and
`shares`. Both share maps are read, public and private, for the reason
`slackwrite.Shares` already states — which map a share lands in depends on the
channel, and reading only `public` loses every private channel and DM.

`hear file` and `hear files` differ by one letter. They differ in arity too — one
argument against two — so a mistyped invocation is a usage error rather than a
different action. `hear fileinfo` is the alternative if the collision reads worse
in practice than it does on paper.

## The render

The render is the authorisation. Reading it has to be enough, so it names the
file, what it is, who owns it, and every conversation it disappears from.

```
--- dry run: nothing deleted ---
file    : F07ABC123
name    : trace.pdf
type    : application/pdf   1.2 MB
uploaded: 2026-08-24T09:14:02Z by @jane
visible : #eng                     1785708834.862009
          #general                 1785712596.256909
          @alice (DM)              1785713001.114900
--- the file goes; the 3 messages carrying it stay, showing a deleted file ---
----------------------------------------
```

And for a file no message carries any more — the case this feature exists for,
a file left behind when its message was deleted:

```
visible : nowhere — no message carries this file
--- the file goes; nothing else changes ---
```

Points that are load-bearing rather than cosmetic:

- **The conversation list is the point of the render** — and it is the visible
  list, not necessarily the whole one. `files.info` answers with the shares the
  *reading* token can see, so a file another member reshared into a private
  channel or a DM that account is not in may not appear. Slack also reports
  `has_more_shares` and `skipped_shares` when it has omitted some. So the
  summary line claims only what it knows:

  ```
  --- the file goes; the 3 messages carrying it stay, showing a deleted file ---
  ```

  Observed, not assumed: deleting a file leaves every message that carried it
  in place, its file entry replaced by a tombstone. Nothing is removed from a
  conversation. The count is what is *visible* — Slack flags a partial list and
  a partial list is refused.

  and when Slack says it has left some out, or the share object is absent
  rather than empty, the delete is **refused**: a render that cannot bound the
  blast radius has not earned the authorisation. An empty share object that
  Slack does not flag as partial is a positive answer and passes.

  The count is stated in words so the list cannot be skimmed past.
- **Names and titles go through `termsafe`.** A filename is chosen by whoever
  uploaded the file, an external guest included. So does the uploader's display
  name, which is attacker-controlled by the README's own analysis.
- Sizes through `internal/humansize`, as `sez --file` prints them.
- A channel is shown by name when `hear` resolved one and by id when it did not.
  A DM is shown by the person, which is the only name it has.
- When the uploader is not the account that would delete it, the render says so.
  The identity in hand is the **read** credential's, and the account that deletes
  is the **write** credential's — normally the same person, but nothing enforces
  it: the two are enrolled separately and either can be overridden by an
  environment variable. So the wording says what is actually known:
  `uploaded: … by @jane (not the account this tool reads as)`. When no identity
  is cached at all, the line is omitted rather than guessed.

  A warning, not a refusal: Slack is the authority on who may delete, an admin
  token may succeed, and refusing here would be this tool guessing at a
  permission model it does not hold.

`--send` is the only confirmation. A second one — a `--force`, a typed
filename — teaches people to produce both without reading either.

## The write allowlist

`HERSAID_WRITE_CHANNELS` is the strongest control in the tool, and this is the
most destructive verb in it, so it applies. A file delete touches every
conversation the file is shared in, so **every one of them must be permitted**,
and the refusal names the first that is not.

The check runs after the read — it needs the share list — and before any write
request, so a refusal issues nothing on the write credential. Note the inversion:
for a message the allowlist runs *before* the read, and a refusal really does
cost nothing. Here the read has to come first, so a refused file delete has
already called `files.info` and possibly filled a cache. That is unavoidable and
harmless, and it is written down so nobody later "fixes" the order back and
breaks the share list the check depends on. The record is written with
`phase: "refused"`, as an allowlisted-out message is.

A DM share is checked as a DM, not as a bare id. The allowlist's documented
meaning is that a person allowlisted by their `U…` stays allowed once a DM
exists, which works because resolution yields both the `D…` and the `U…`. A
share arriving from `files.info` carries only the conversation id, so
`hear file --json` reports the counterpart's user id alongside an `im` share —
the render needs it to name the person anyway — and the check is built from
both. Without that, a file shared in a DM would be refused for someone who had
allowlisted exactly that person, which fails closed but breaks the control's
contract for the people who bothered to configure it.

A file in no conversation touches no conversation, so it passes an active
allowlist. That is not a loophole granted to make the orphan case work: it is
what "every conversation it is visible in is permitted" says about the empty set,
and the orphan case is exactly the one where nobody can see the thing being
destroyed.

The consequence in the fail-closed direction: a file shared into a conversation
the cache cannot name, while the list is active, is refused. Consistent with the
README's existing statement that a `#name` allowlisted and then addressed by raw
id is refused.

## The call, and its classification

`files.delete` is a form POST to the API returning an envelope. It is exactly
what `slackwrite` already guarantees, so it is a typed method there —
`DeleteFile(ctx, fileID) error` — beside `Delete`, with `files.delete` in the
method constants and `files:write` in `scopeNames`. Nothing new goes on the
wire, no caller-supplied `url.Values` exists, and `missingScopeFor` names
`files:write` without a change.

`slackwrite.Classify` applies unchanged and must not be re-derived. The upload
needed `slackupload.ClassifyShare` because a three-leg flow has a phase before
which nothing can have happened; this is one request, which is the case
`Classify` was written for. A `PreflightError` is `Rejected`, an `APIError` is
`Rejected` unless its code is in `ambiguous`, and everything else — a transport
failure included — is `Ambiguous` and recorded `ok: null`.

**No code is added to the idempotent set.** "Already deleted" is arguably the end
state asked for, and `idempotent()` is method-scoped so admitting it is one line.
It is not admitted, for two reasons. The code Slack returns for deleting an
already-deleted file has not been observed, and the upload spec's rule is that a
code list grows by evidence rather than by analogy. And the read gate makes it
close to unreachable: a file `files.info` cannot return is refused before any
write is attempted. A real observation, in a table-driven test carrying its
reason, is what adds it later.

`file_not_found` must never become an idempotent success. An id that never
existed is a typo, and reporting a typo as a successful deletion is the failure
mode this whole tool is shaped against.

## The audit record

A file delete gets its own record type, `audit.FileDelete`, action `file.delete`.

The `audit` package's opening comment already states the rule: `Post` and
`Reaction` are separate types because the records genuinely differ. This record
differs more than either. It has no channel, no thread, no message ts, no text,
no `slack_text`, no `previous_text` — and it must carry a list of conversations,
which no post field holds. Reusing `Post` would emit ten nulls, an empty
`channel` string that is not a channel, and would still have nowhere to put the
one fact worth reconstructing later: what was visible where, at the moment it was
destroyed.

```json
{"at":"2026-08-26T11:02:14Z","by":"alice","phase":"intent","action":"file.delete",
 "file":{"id":"F07ABC123","name":"trace.pdf","size":1258291,
         "mimetype":"application/pdf","created":"2026-08-24T09:14:02Z",
         "uploader":"U0123ABCD"},
 "shares":[{"channel_id":"C0123ABCD","channel":"eng","ts":"1785708834.862009"},
           {"channel_id":"D0456EFGH","channel":null,"ts":"1785713001.114900"}],
 "dry_run":false,"ok":null,"error":null}
```

- `audit.File` is not reused for the file object. Its wire carries `path`, which
  is a local path an upload sent and is meaningless for a file that only exists
  in Slack; a field that is always empty is a field that misleads.
- `shares` is what the render showed. The record should say what the render
  claimed, which is the rule `sez --file` already follows for `path` and
  `mimetype`.
- Two lines per attempt, as everywhere else: intent before the request, outcome
  after. The intent line's `ok` is null, and it is the record that survives
  SIGKILL — which matters more here than for a send, since it is the only
  evidence of what a killed delete was pointed at.
- `phase` takes the same five values. `refused` for the allowlist, `abort` for
  the read failing or a caught signal, `dry-run` for a render.
- The signal protocol is `change()`'s, unchanged: a snapshot record, one
  `audit.OnAbort` handler writing `abort`/`unknown`, `stop()` deferred. The
  `progress` mutex in `cmd/sez/upload.go` exists because an upload has several
  requests and a phase boundary inside one of them. This has one request.

Two consequences to write down rather than discover:

- The README's "every record carries `at`, `by`, `phase`, `channel`,
  `channel_id`, `dry_run`, `ok` and `error`" stops being true. It is amended to
  name the three shapes.
- `select(.action // "" | startswith("message."))` still isolates `sez` message
  records and `startswith("react.")` still isolates `tap`'s. `file.` is a third
  prefix, and a jq recipe for it joins the README's list.

## What this breaks in the README

The audit log is a documented contract, and a third record shape changes what
reading it looks like. Two things need amending rather than adding:

- The `files` bullet already covers uploads and message-deletes; `file.delete`
  is a record with no `channel` at all.
- The intent-reconciliation recipe, `group_by(.channel + (.text // .ts // ""))`,
  is a jq type error the moment a record has a null `channel` — so it stops
  working on the whole log, not just on the new lines. It becomes
  `group_by((.channel // "") + (.text // .ts // .file.id // ""))`.

A recipe published as a way to audit the tool has to keep working when the tool
grows, so this counts as part of the feature and not as documentation tidying.

## Failure

Exit 0 deleted or rendered, 1 refused or failed, 2 usage, 3 the attempt could not
be recorded. Unchanged.

One attempt, never retried. An `Ambiguous` outcome is recorded `ok: null` and
printed as `delete UNKNOWN — check Slack before retrying`. A retry after a
transport failure is harmless here in a way it is not for a send — a second
delete of a deleted file changes nothing — but the tool does not retry, and a
rule with an exception is not a rule.

## The orphan case

**Half of it turned out not to exist.** See the section below: a file staged and
never shared is discarded by Slack, so `sez --file`'s note names ids that
correspond to nothing. That note is shipped text and wrong; correcting it is a
separate change and does not belong here.

What remains is the case this feature is now entirely for: a file whose message
was deleted. `chat.delete` takes the message and its shares; the file stays,
visible to `files.info`, carried by nothing. Nobody can see it and no verb here
can remove it.

**Not changed: nothing deletes automatically.** The reasoning in the upload spec
stands word for word — destroying something automatically in response to a
failure is the wrong instinct in a tool whose whole shape is "show it first", and
it would be a second irreversible call on the first's failure path.

**Not built: an orphan sweep.** A `files.list` walk finding every file with no
shares would end in an offer to delete files in bulk, which is the shape this
tool refuses. If it is ever wanted, it is a `hear` verb that only lists, and it
is a separate change with its own argument.

## What step 0 observed

Run against the real workspace before any code, with one account. Two of the
six questions needed a second account or a throwaway canvas and are still open;
four are answered, and two of the answers change the design.

**A staged file that is never shared does not exist.** `files.info` on the id
returned by `files.getUploadURLExternal`, after the bytes were sent and the
completion leg deliberately skipped, is refused as `file_deleted` — from the
read token and the write token alike. Slack discards an upload that never
completes, which is what its own documentation says and what nothing here had
tested.

*This removes half the reason for the feature.* The orphan case was one of two
motivations; there are no orphans of that kind. It also means `sez --file`'s
note — "N files were transferred but never shared: F07ABC123" — names ids that
correspond to nothing. That text is shipped and misleading, and fixing it is a
separate change that should not wait for this one.

**Deleting a file does not delete the messages that carried it.** A message was
posted with a file, the file deleted, and the message read back: it is still
there, and its file entry has become `mode: "tombstone"`, with `size: 0` and the
id in place of the name.

*This rewrites the render's central sentence.* "Deleting this removes it from
all 3 conversations" is wrong: nothing is removed from a conversation. Every
message that carried the file stays exactly where it is, showing a placeholder
where the file was. The honest line names what actually happens:

```
--- the file goes; the 3 messages carrying it stay, showing a deleted file ---
```

which is a different and in some ways worse thing to authorise — the evidence
that something was deleted remains visible to everyone who could see it.

**`files.delete` is idempotent in the shape this codebase already recognises.**
A second delete of the same id answers `file_deleted`. That is the end state the
caller asked for, exactly as `already_reacted` is for a reaction, so it joins the
idempotent set as a method-scoped entry, records `ok:true`, and prints that the
file was already gone. `file_not_found` stays a refusal: it conflates a typo
with a file the token cannot see.

**The two credentials are the same person here**, and `auth.test` on the write
token returns its own user id — so the render can compare the uploader against
the account that will actually delete, rather than against the one that read.
The identity concern is real in principle and empty in this install.

### Still unanswered

- **Can a share be invisible to the reading token?** Needs a second account to
  reshare a file somewhere the read account cannot follow. Until it is answered
  the render claims only what it can see and refuses when Slack flags the list
  as partial, which is the safe reading either way.
- **What `files.delete` does to a canvas.** Needs a throwaway canvas. Canvases
  stay out of scope until it is answered.

## Risks and unverified claims

Stated plainly, because three of these decide whether the design works and none
has been observed.

1. **Can the read credential see a file with no shares?** Partly answered. A
   file uploaded by the write app's user, shared, and then left share-less by
   deleting its message *is* returned by `files.info` on the read token:

   ```
   F0123ABCDEF: name=hersaid-test-1.txt mode=snippet channels=[] ims=[] shares={}
   ```

   Both credentials are user tokens for the same person, so this is expected —
   but the case that matters is a file staged and **never** shared, which has
   not been observed. It is the first thing step 0 checks.

   **Contingency if the answer is no, corrected:** the earlier draft had the
   render degrade and `--send` still proceed, which is the read gate weakening
   for exactly the file whose blast radius cannot be shown — and it contradicts
   this document's own rule two sections above. It also could not tell an
   invisible orphan from a mistyped id, a revoked token, or a transient failure.

   So: if `files.info` answers `ok:false` with `file_not_found` — a definitive
   answer from Slack, not a failure to reach it — the id may be attempted, and
   Slack refuses it as `file_not_found` if that is what it is. Every other
   failure, transport and auth and scope alike, refuses. If step 0 shows the
   read token sees staged orphans, this contingency is deleted rather than
   shipped.
2. **The exact shape of `files.info`'s reply** — whether `channels`/`groups`/`ims`
   and `shares` agree, whether the channel lists page, what a canvas returns — is
   read from documentation, not from this workspace. One real reply is captured
   and the decoding is written against it, as the upload's `shares` handling was.
3. **What `files.delete` does to a message whose whole content is the file.**
   A `sez --file` upload with a comment creates a message carrying the file. If
   deleting the file removes or tombstones that message, "removes it from 3
   conversations" understates what happens, and the render's summary line must
   say so instead. Observed before the wording is fixed.
4. `files.delete` on a file the caller does not own is expected to be refused.
   The render warns; the refusal is Slack's to give.
5. **Whether the share list can be complete at all.** `files.info` returns the
   shares the reading token can see, and Slack reports `has_more_shares` and
   `skipped_shares` when it has omitted some. Whether another member resharing a
   file into a channel the read account cannot see makes that share invisible is
   unobserved — and the render's summary line is the entire authorisation, so an
   understated blast radius is the worst failure this design has. Step 0 has a
   second account reshare a test file somewhere the read account cannot follow,
   and the wording above stands or is narrowed on what comes back.
6. **What `files.delete` does to a canvas.** Slack has a separate
   `canvases.delete`, which is evidence `files.delete` may refuse one or behave
   differently — and a canvas is co-authored, so naming a single uploader
   understates whose work goes. Until that is observed, canvases are **out of
   scope**: `/docs/` permalinks are not accepted and a canvas mimetype is
   refused by name. Accepting them is a later change with its own render.
7. **Which identity is the deleting one.** The read credential's user id is what
   `hear` can report; the write credential's is what deletes. They are enrolled
   separately and either can be overridden by environment variable, so they are
   not the same fact. Step 0 checks whether the write identity can be
   established at all; until it can, the render says "not the account this tool
   reads as" rather than "not you".

## Open questions

These are the human's call. Recommendations are given and are not decisions.

### 1. Flag on `sez`, a subcommand, or a new command

`sez`'s verbs address messages, and a file is not a message: `--delete-file`
would be the first `sez` invocation with no conversation in it.

- **A new command** is what the README's own argument for `tap` recommends: "a
  separate command name is a separate string to permission. A policy that permits
  `tap` while keeping `sez` behind review is only expressible if the two are
  different commands." That argument is strongest here, where the act is
  irreversible and touches other people.
- **A flag on `sez`** inherits the credential gate, the render-and-`--send`
  discipline, the audit log, the `hear` subprocess, the signal handling and
  `missingScopeFor`. A new command reimplements all of it — `tap` already does —
  and a reimplemented gate is how a gate drifts.

**Recommendation: a flag on `sez`** — and the code-sharing argument above is not
the reason, because it is the weaker one. Two corrections to it: `viaHear` has to
be forked anyway (different argv, different reply to parse), so what is reused
verbatim is the process-launching scaffolding and the send-path plumbing rather
than "~80 lines"; and where code currently sits should not decide a public
interface.

The reason is that **the permission boundary this would protect is already
crossed**. `sez --delete` destroys a message somebody else has read, today,
behind the `sez` string. Any policy that permits `sez` already permits
irreversible destruction of other people's content. Splitting *file* deletion
into its own command would put the boundary in an indefensible place — messages
destroyable under the permitted name, files not — and would leave the person who
drew it believing they had separated destructive acts from safe ones when they
had separated one destructive act from another.

The honest version of the permission argument is that **all three destructive
verbs belong behind one name**, and `--edit`, `--delete` and `--delete-file`
should move together if they move at all. That is a refactor with its own
argument, and this feature should not decide it by accident in either direction.

The weakness of the flag stands and is worth stating: flags are order-free, so
`sez --send --delete-file` defeats prefix matching, and no permission rule can
separate this from posting. It is a real cost. It is a cost the tool already
pays for `--delete`.

**Sub-question: one file or many.** Recommended one per invocation, for the
reason in the CLI section. Going from one to many later is additive — N files
produce N record pairs, each identical to the single case — so nothing is
foreclosed.

### 2. File id only, or permalink, or a message address

**Recommendation: id and file permalink; not a message address.** The id is what
a failed upload prints and what `hear read --ts` shows, so it is mandatory. The
permalink is what a person copies from Slack, and the host-check plus
id-extraction already exists in `cmd/hear/canvas.go` for `/docs/` links — one
more pattern covers `/files/`. A message address is the one to leave out: it
makes a single destructive target into a chooser, needing interactive selection
or an index argument, and `hear read --ts` already composes to the same result.

### 3. A file still shared in a live message: refuse, warn, or allow

**Recommendation: allow, with the render enumerating every conversation.**
Refusing would leave a tool that can only delete files nobody can see — which
excludes the commonest real case, cleaning up a file that was posted by mistake.
The project's model throughout is "show it, then `--send`", not "forbid it".

The render carries the weight: one line per conversation, and a summary stating
the count in words. Two guards go with it:

- The render is refused when the shares could not be enumerated at all. Fail
  closed, the same rule as `--edit` refusing a message it cannot show whole.
- No `--force` and no second confirmation. `--send` is the confirmation.

If the human prefers stricter: refusing when share count is above some threshold
is arbitrary, and refusing *any* live share is coherent but halves the feature.
Between them, the coherent one is refusing any live share — I do not recommend
it, but it is the only stricter option worth stating.

### 4. Does `hear` need a new verb

**Recommendation: yes — `hear file <F…>`.** No existing verb answers "what is
this file and where is it visible": `files` needs a message address and writes
bytes, `canvas` prints canvas content. `slackread.FileInfo` is already
implemented and reachable from nothing. The verb is independently useful outside
this feature, and the share list it returns is precisely what the delete render
needs.

The alternative is a `sez`-only JSON mode on `hear files` — a flag that makes a
download verb not download. That is worse than a verb.

If the one-letter distance from `hear files` reads badly, `hear fileinfo` is the
same design under a longer name.

### 5. Does the orphan case deserve more

**Recommendation: one line of text, nothing more.** `sez --file`'s staged-file
note names `sez --delete-file` and says the files are visible to nobody. That
closes the loop: the failure that produced the ids tells you what removes them.

Nothing automatic, and no orphan sweep — reasons in the orphan section above.

### 6. What the write allowlist means for a file (raised here, not in the brief)

`HERSAID_WRITE_CHANNELS` names conversations, and a file is not addressed by one.
Three readings: ignore the list for file deletes; require every conversation the
file is shared in to be permitted; or refuse all file deletes while the list is
active.

**Recommendation: require every conversation to be permitted.** Ignoring the list
would put the most destructive verb outside the strongest control. Refusing
outright is blunt and would break the orphan case for anyone who uses the list at
all. The empty set passing is correct rather than convenient: a file in no
conversation touches no allowlisted conversation.

---

## Implementation plan

Ordered. Step 0 can invalidate the design, so nothing is built before it.

**0. Verify against real Slack.** No code. Capture, from a scratch workspace or
the user's own with a throwaway upload:

- `files.info` on a **read** token for a file staged by the **write** token and
  never shared. Does it return? This decides whether the orphan case survives
  the read gate, and the contingency in Risks is what runs if it does not.
- One full `files.info` reply for a file shared into a public channel and a DM.
  The decoding is written against this, not against the docs.
- `files.delete` against a file whose message was created by `sez --file` with a
  comment: does the message survive, tombstone, or vanish? The render's summary
  wording depends on the answer.
- The error code for deleting an already-deleted file, recorded for whoever adds
  it to the idempotent set later. Not acted on now.

Findings go into the doc above, under "Observed, once", replacing the
corresponding risk.

**1. `internal/slackread`** — widen `File` with `created`, `user`, `permalink`,
`channels`, `groups`, `ims`, `shares`, decoded against the step-0 capture. No new
endpoint: `FileInfo` exists.
*Tests pin:* both share maps are read; a reply with `channels: []` and no shares
decodes to an empty conversation list rather than an error; a reply missing
`shares` entirely does the same; the channel lists and the share maps are
reconciled into one deduplicated set.

**2. `internal/render`** — `KindFile`, the `File` payload, and the human form.
*Tests pin:* a hostile filename, title and uploader display name are stripped in
the **human** form and survive **byte for byte** in the JSON. That split is the
convention the rest of the tool already keeps — `termsafe` is for display, and a
structured document carries what Slack has, because the record built from it is
supposed to say what Slack has rather than what happened to print. An earlier
draft pinned stripping in both, which would have put a sanitised name in the
audit record and quietly repeated the bug the message-delete record was just
fixed for. Also pinned: a DM with no channel name renders by person; an
unresolved channel renders by id; the JSON field set is exact.

**3. `cmd/hear`** — the `file` verb, its usage block, permalink acceptance
extended to `/files/<U…>/<F…>`, and arity strictness.
*Tests pin:* `hear file` with no argument and with two arguments is a usage error
and resolves no credential; a non-Slack host in a link is refused; a `/files/`
link and a bare id reach the same lookup; a file id that does not exist exits 1;
`--json` carries the read user's own id.

**4. `internal/slackwrite`** — `methodDeleteFile`, `scopeNames` entry,
`DeleteFile(ctx, fileID) error`.
*Tests pin:* the form carries exactly `file` and nothing else; `Scope` on its
`APIError` returns `files:write`; `Classify` gives `Rejected` for
`cant_delete_file` and `file_not_found`, `Ambiguous` for a transport failure and
for `internal_error`; a zero credential is a `PreflightError` and issues no
request.

`file_deleted` is left for step 0 to decide, and the earlier draft was
inconsistent about it: it told step 0 to observe the code and then pinned a test
that it must not be idempotent. Slack documents `file_deleted` as the answer for
a file that is already gone, which is the end state the caller asked for — the
shape `idempotent()` already recognises for `already_reacted` and `no_reaction`.
A race between the read and the delete reaches it. If step 0 sees Slack send it
for an already-deleted file, it joins the idempotent set as a method-scoped
entry, records `ok:true`, and prints that the file was already gone. If it means
something else, it does not. What stays fixed either way is that `file_not_found`
is never a success: it conflates a typo with a file the token cannot see.

**5. `internal/audit`** — `FileDelete`, its wire type, its key order, `file.delete`.
*Tests pin:* the exact key order and field set; `ok` marshals null/false/true;
`shares` is null when empty and an array otherwise; a share with no resolved name
carries `channel: null`; `audit.File` is untouched.

**6. `cmd/sez`** — flag parsing and exclusions, the `hear file` read, the render,
the allowlist check, the delete path.
*Tests pin, in this order of importance:*

- Without `--send`, no request is made and the record is `dry-run`. Asserted on
  the fake server's hit count, not inferred.
- The read failing — `hear` absent, `hear` non-zero, `hear` timing out, `hear`
  returning unparseable output — refuses the delete **under `--send`**, records
  `abort`, and issues no request.
- The render names every conversation, the count, the size, the type and the
  uploader; a hostile filename cannot restructure it.
- A conversation outside `HERSAID_WRITE_CHANNELS` is refused, records `refused`,
  and issues no request. A file with no shares passes an active allowlist.
- Intent precedes the request and outcome follows it: two records, `ok` null then
  set.
- A signal between them leaves one `abort` record with `ok: null`, and exit
  128+n.
- `missing_scope` prints the `files:write` help, not `chat:write`.
- A transport failure records `ok: null` and prints UNKNOWN.
- Every exclusion — `--delete-file` with `--delete`, `--edit`, `--file`,
  `--thread`, `--md` — is a usage error that writes no audit line.
- `--help` and `--version` still resolve no credential.

**7. `cmd/sez/upload.go`** — the staged-file note names `sez --delete-file`.
*Test pins:* the note's text contains the command and each staged id.

**8. `README.md`** — `hear file` in the read section; `--delete-file` in the
`sez` section and in "Editing and deleting", which becomes "Editing, deleting and
removing files"; the safety-model note that no new scope is involved and that the
allowlist covers every conversation a file is shared in; the audit section's
"every record carries" sentence amended to three shapes, with a `file.delete` jq
recipe.

**Not in scope:** batch deletion, an orphan sweep, `files.list`, any automatic
deletion on an upload failure, and a message-address form of the argument.
