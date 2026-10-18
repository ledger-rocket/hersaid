# Install

macOS and Linux, on amd64 or arm64. Each route installs all four commands;
none of them sets up a credential. [Credentials](credentials.md) does that.

## From a release

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

## With `go install`

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

## From source

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
