# Contributing

Issues and pull requests are welcome. For a security problem, do not open an
issue: follow [SECURITY.md](SECURITY.md).

## Before you start

Open an issue first for anything larger than a small fix. hersaid holds a
narrow safety model (see [The safety model](README.md#the-safety-model)), and a
change that widens what a command can do is easier to agree on before it is
written. In particular, a pull request is not accepted if it:

- gives `hear` a write path, or imports the write packages into it
- lets `sez` deliver anything without `--send`
- writes to Slack without a line in the audit log
- prints text from Slack without passing it through `internal/termsafe`

## Build and test

You need the Go version named in `go.mod`.

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...           # the version CI pins is in .github/workflows/ci.yml
```

CI also runs `gofmt -l .` and lints `install.sh` with `shellcheck`. The tests
never reach Slack: `internal/slacktest` is a fake Slack API, and a test that
needs one uses it.

## Writing a change

- Write the failing test first, and keep it in the pull request. A bug fix
  without a test that fails before the fix is not done.
- Test data is made up. Use placeholder people (`alice`, `bob`), channels
  (`#general`, `#eng`) and ids (`C0123ABCD`, `U0123ABCDE`). Never paste real
  workspace content, names or ids, even in a comment.
- Comments say why the code is the way it is, not what it does.
- A change to what a command prints or accepts updates `README.md` in the same
  pull request.

## Commits and pull requests

Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org/):
`fix: …`, `feat: …`, `docs: …`, `chore: …`. Keep one change per pull request,
and say in the description what was wrong and how you checked the fix.

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
