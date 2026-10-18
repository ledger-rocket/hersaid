# Security policy

## Reporting a vulnerability

Report a vulnerability privately through GitHub: open the repository's
**Security** tab and choose **Report a vulnerability**. Do not open a public
issue or pull request for it.

Include the command, its version (`<command> --version`), the platform, and the
smallest input that shows the problem. Replace any real workspace content with
made-up text first.

We reply within five working days. Fixes ship as a new release, and the
advisory credits the reporter unless they ask otherwise.

## Supported versions

Only the latest release gets fixes.

## Scope

In scope, because the [safety model](README.md#the-safety-model) promises it:

- `hear` reaching a Slack write endpoint, or carrying write code in its binary
- `sez` delivering anything without `--send`, or delivering text that differs
  from the render
- a write that bypasses `HERSAID_WRITE_CHANNELS`
- a write attempt that leaves no line in the audit log
- a token sent to a host other than Slack, or shown in output, errors or the
  audit log
- text from Slack that drives the terminal, or a file name that escapes the
  directory `hear files` writes to

Out of scope:

- anything that needs the token itself: a Slack token is a bearer credential,
  and whoever holds it can call Slack directly
- Slack's own behaviour and rate limits
- a compromised local account, or a hostile binary earlier on `PATH`
