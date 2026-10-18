# Credentials

## 1. Create the Slack app(s)

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

## 2. Enrol

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

## Where things live

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
