#!/usr/bin/env bash
#
# hersaid installer — builds hear, sez, tap and hersaid-setup, and installs them
# into a directory on PATH.
#
# It does not enrol credentials. hersaid-setup does that: it checks a token
# against Slack, checks the scopes it was granted against the role, and stores
# it under the hersaid-read / hersaid-write keychain services the commands
# actually read. An installer that prompted as well would be a second
# implementation of the same thing, free to disagree with the first.
#
#   ./install.sh
#   PREFIX=/usr/local/bin ./install.sh
#
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST="$SRC/dist"
COMMANDS=(hear sez tap hersaid-setup)

say()  { printf '%s\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*" >&2; }
err()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }

usage() {
  cat <<'EOF'
usage: install.sh

  Builds hear, sez, tap and hersaid-setup, and installs them into a directory on
  PATH. Credentials are not set up here; run hersaid-setup afterwards.

  Environment:
    PREFIX   where to install the commands. Default: ~/.local/bin, or ~/bin when
             that is the one already on PATH.

  A Go toolchain builds the commands into ./dist. Without one, whatever is
  already in ./dist is what gets installed, which is how downloaded release
  binaries are set up.

  Exit: 0 installed, 1 nothing to install, 2 usage.
EOF
}

if [[ $# -gt 1 ]]; then
  err "install.sh: one option at most"
  usage >&2
  exit 2
fi
case "${1-}" in
  "") ;;
  -h | --help)
    usage
    exit 0
    ;;
  --with-write)
    err "install.sh: enrolment moved to hersaid-setup, which this no longer does."
    err "  Install first, then run:  hersaid-setup --with-write"
    exit 2
    ;;
  *)
    err "install.sh: unknown option: $1"
    usage >&2
    exit 2
    ;;
esac

# --- the binaries ---------------------------------------------------------

if command -v go >/dev/null 2>&1; then
  # go build stamps the revision, the commit date and a dirty tree by itself.
  # Only a tag has to be passed in, and only when one describes this commit.
  # The module path is asked for rather than written out, so a rename cannot
  # leave an -X here pointing at a variable that no longer exists — the linker
  # ignores one it cannot resolve, and the build would still look fine.
  LDFLAGS=""
  TAG="$(git -C "$SRC" describe --tags --dirty 2>/dev/null || true)"
  if [[ -n "$TAG" ]]; then
    LDFLAGS="-X $(cd "$SRC" && go list -m)/internal/version.version=$TAG"
  fi
  say "built:"
  for cmd in "${COMMANDS[@]}"; do
    (cd "$SRC" && go build -ldflags "$LDFLAGS" -o "$DIST/$cmd" "./cmd/$cmd")
    say "  $DIST/$cmd"
  done
else
  for cmd in "${COMMANDS[@]}"; do
    if [[ ! -x "$DIST/$cmd" ]]; then
      err "no Go toolchain, and $DIST/$cmd is not there to install."
      err "  Install Go — https://go.dev/dl/ — and run this again, or put the"
      err "  release binaries in $DIST/ first."
      exit 1
    fi
  done
  say "no Go toolchain — installing what is already in $DIST"
fi

# --- install into a directory on PATH -------------------------------------

on_path() { case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }

if [[ -n "${PREFIX:-}" ]]; then
  TARGET="$PREFIX"
elif on_path "$HOME/.local/bin"; then
  TARGET="$HOME/.local/bin"
elif on_path "$HOME/bin"; then
  TARGET="$HOME/bin"
else
  TARGET="$HOME/.local/bin" # XDG-conventional default
fi

mkdir -p "$TARGET"
say "installed:"
for cmd in "${COMMANDS[@]}"; do
  chmod +x "$DIST/$cmd"
  # Copied, not symlinked: dist/ is build output and is gitignored, so a symlink
  # into it dangles the moment the tree is cleaned or rebuilt — and a command
  # that vanishes reports only "command not found".
  # Removed first: an earlier install left a symlink here, and install(1)
  # follows it, so source and destination resolve to one file and it refuses.
  rm -f "$TARGET/$cmd"
  install -m 0755 "$DIST/$cmd" "$TARGET/$cmd"
  say "  $TARGET/$cmd"
done

if ! on_path "$TARGET"; then
  warn ""
  warn "$TARGET is not on your PATH. Add this line, then open a new shell:"
  warn "    export PATH=\"$TARGET:\$PATH\""
fi

cat <<'EOF'

Credentials are not set up here. hersaid-setup enrols them, checking each token
against Slack — and the scopes it was granted against the role — before storing
anything:

    hersaid-setup                # the read credential; posting stays impossible
    hersaid-setup --with-write   # also the write credential, enabling posting

What this does not do: create the Slack apps, edit your shell profile, or enrol
or revoke any credential.

done. try:  hear --help
EOF
