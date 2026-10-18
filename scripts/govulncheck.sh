#!/usr/bin/env bash
# Scans for known vulnerabilities the code can reach, on every platform a
# release ships. CI, the release gate and contributors all run this file, so
# the pin and the targets live in one place.
#
# Only a reachable vulnerability fails; one in a required module that no call
# path touches is listed but passes. The standard library scanned is the
# toolchain's: in CI setup-go installs exactly go.mod's version, so a
# reachable stdlib advisory fails until the go line is bumped, which also
# moves `go install` users. A local run on a newer Go patch release can pass
# where CI fails. The advisory database updates daily, so this can turn red
# with no code change. Dependabot does not read this file; bump the pin by
# hand.
set -euo pipefail

bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
GOBIN="$bin" go install golang.org/x/vuln/cmd/govulncheck@v1.9.0

# The goos x goarch matrix in .goreleaser.yaml. Build constraints select
# different files and modules per platform, so one host scan is not enough.
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

# Every target is scanned before the exit, so one failing run reports all the
# platforms an advisory reaches.
rc=0
for target in $targets; do
  echo "== $target"
  GOOS="${target%/*}" GOARCH="${target#*/}" CGO_ENABLED=0 "$bin/govulncheck" ./... || rc=1
done
exit "$rc"
