# `hear files` — download message attachments

## Goal

`hear` can tell you a message carries files; it cannot get them. `Message.Files`
is decoded only far enough to set `partial` on the JSON transcript, so a
reader who sees "there is a file here" has no way to reach it.

`hear files <address> <ts>` writes the bytes of every file on one message to
disk. It does not parse, decode, or summarise them: the deliverable is bytes on
disk and a line saying where they landed.

## CLI surface

```
hear files <address> <ts>     download every file attached to one message
```

- `<address>` is the existing grammar: `#channel`, `@person`, a Slack id, or a
  permalink. A permalink carries channel and timestamp, so it takes no `<ts>`.
- `--out DIR` — parent directory for the message directory. Default: CWD.
- `--max-size N` — per-file ceiling, suffixed `K`/`M`/`G`. Default `100M`.
- `--json` — one object, `"kind": "files"`.

Files land in `<out>/<channel>-<ts>/<name>`:

```
$ hear files #eng 1785708834.862009
wrote  eng-1785708834.862009/trace.pdf (1.2 MB)
skip   design-notes — external file (google drive), no bytes to fetch
skip   demo.mp4 — 3.1 GB exceeds --max-size 100M
```

The directory is per message so two messages' files cannot collide and a
re-run is answerable. The channel segment is the resolved channel name when the
cache knows it and the raw id when it does not; either is sanitised.

## Transcript surfacing

`hear read` and `hear thread` name the files on a message, because otherwise
nothing tells you a message is worth passing to `hear files`:

```
1785708834.862009  jane  here's the trace
                         ↳ 2 files: trace.pdf (1.2 MB), out.log (14 KB)
```

`slackread.Message.Files` becomes `[]File` rather than `json.RawMessage`.
`HasUnrenderedContent` continues to answer from `len(m.Files) > 0`.

`File` carries `id`, `name`, `title`, `mimetype`, `size`, `mode`,
`is_external`, `url_private_download`. `mode` is what says whether bytes exist:
`hosted` and `snippet` are fetchable; `external` is a link to someone else's
storage, `tombstone` is a deleted file, `email` is a mail body. The
unfetchable modes are skipped with the reason named.

## Download path

A new package, `internal/slackfiles`. It does not live in `slackread` because
every response there passes through one `get()` whose contract is that the body
is an enveloped JSON document; a streaming binary fetch either breaks that
contract or bypasses the function that enforces it. `slackfiles` imports
`cred.Read` only, so `cmd/hear`'s import graph still contains no write code.

### Redirects

`slackhttp` refuses every redirect: a `Location` can move an `Authorization`
header to a host of someone else's choosing. Slack serves file bytes from
`files.slack.com`, which may answer directly or redirect to a pre-signed URL on
storage it does not itself serve.

The allowlist therefore has two tiers, because a host that may serve bytes is
not thereby a host the credential may be sent to:

- **Bearer hosts** — Slack's own. A download must *start* at one of these, or no
  request is made at all.
- **Redirect-only hosts** — the object storage a pre-signed URL points at. A hop
  may land here; a download may never start here. This tier is broad, since it
  is anyone's bucket, and that is affordable exactly because no credential
  reaches it.

A redirect is then followed under four bounds:

1. The target host must match either tier.
2. `Authorization` is dropped as soon as the host changes. A pre-signed URL
   carries its own credential in the query string, so the bearer is both
   unnecessary and exactly what must not leak. Go drops it across a change of
   domain of its own accord, but not across a change of port, so this package
   drops it itself and a test holds it to that.
3. At most three hops.
4. No downgrade from HTTPS, and `Proxy: nil` as everywhere else. A plaintext
   download URL is refused outright, loopback aside, where a bearer that never
   leaves the machine has not been disclosed by travelling.

This is a superset of the no-redirect behaviour: if Slack answers hosted files
directly, no hop is ever taken and the allowlist is dead weight that costs
nothing.

Observed, once: a `hosted` 18.5 MB video came back from `files.slack.com` with
HTTP 200 and no redirect at all. That is one file, in one workspace, on one
plan — not enough to collapse this to reusing `slackhttp` unchanged. Other file
modes, larger objects and Enterprise Grid may still hand off to storage, and the
allowlist costs nothing while unused, where being wrong about it means either
refusing a download Slack intended or following a hop unguarded.

### Refusing what is not the file

Without `files:read` Slack does not return a JSON error for a file request. It
returns HTTP 200 carrying an HTML sign-in page. Written blindly that becomes a
`trace.pdf` containing a login form — a corrupt file that looks like a
successful download.

So a response whose content type is HTML is refused when the file's own
`mimetype` is not, and the refusal names the missing scope. The test is narrow
on purpose: Slack sends `application/octet-stream` for plenty of legitimate
files, so a rule demanding agreement with `mimetype` would refuse real
downloads. HTML where a PDF was expected is the failure that actually happens.

### Writing

Bytes go to a temporary file in the destination directory and are put into place
only after the whole body has been read and accepted. A refused, truncated, or
interrupted download leaves nothing behind rather than something that looks
complete.

The last step is a hard link rather than a rename, because rename replaces
silently. Linking both puts the file in place and refuses to overwrite in one
step, with no window between checking that the target is free and taking it.

The size ceiling is enforced on bytes actually read, through a limit of
`max+1`, not on Slack's declared `size`. The declared size is metadata about
the file, not a promise about the response.

### Names

Slack's `name` is chosen by whoever uploaded the file, including an external
guest, so it is untrusted input to a path. The name is reduced to its last
element under both separators — a Windows-style name reaching a Unix path is
still someone else's choice of directory — and replaced by the file id when what
remains is empty, `.`, `..`, or holds a NUL or control character. A name that
collides inside the message directory gains its file id before the extension. An
existing target is refused rather than overwritten.

## Failure

Best effort, one line per file, exit 1 if any file was skipped or failed. A
message holding one Google Doc link and three real files yields the three
files and a non-zero exit, so a script can tell it got less than it asked for.

`--json` reports the same per file: `status` is `written`, `skipped`, or
`failed`, with `reason` set on the latter two. A message carrying no files at
all is an error rather than an empty success — it means the ts was wrong.

A download is allowed ten times `HERSAID_TIMEOUT`. That variable bounds a
request returning a small JSON document; a body of up to `--max-size` held to
the same bound would fail on size alone over a link that is working.

## Scope

`files:read` joins the read app's user token scopes. `hersaid-setup`'s
`checkScopes` gates on the presence of `chat:write` alone, so the read/write
split is unaffected and existing enrolments keep working. A token without
`files:read` is diagnosed at download time by the HTML refusal above rather
than at enrolment, which would break every credential already stored.

## Testing

`internal/slacktest` grows the ability to serve a file body and an optional
redirect. The cases that matter:

- A traversing name cannot write outside the message directory.
- An HTML sign-in body is refused, not written.
- The ceiling is enforced on real bytes when `size` understates them.
- A redirect to a non-allowlisted host is refused.
- On a cross-host hop the second server observes no `Authorization` header.
- A failed download leaves no file behind.
- A mixed set writes what it can and exits 1.
