#!/usr/bin/env bash
# The vendored upstream cmd/ must be byte-identical to the version its
# go.mod pins.
#
# WHY this exists. third_party/go-cache-plugin carries a COPY of
# upstream's cmd/go-cache-plugin, because goreleaser cannot build a main
# package it cannot stat -- the library itself is still a normal,
# checksummed module dependency. That copy is the weak link: nothing about
# a copy follows the version beside it.
#
# Two ways it goes wrong, both silent:
#
#   1. The pin moves and the copy does not. Renovate bumps
#      `github.com/tailscale/go-cache-plugin` in the go.mod next door and
#      the copied main stays where it was. If the library's API happened
#      not to change, that compiles -- and ships one version's entry point
#      against another version's library.
#
#   2. Somebody edits the copy. The release notes say it redistributes an
#      UNMODIFIED upstream binary, and BSD-3-Clause asks that changes be
#      marked. An unnoticed local edit makes both statements false.
#
# The trust anchor is go.sum, not the network. `go mod download` verifies
# the module against the recorded hash (and the checksum database), so
# comparing against the module cache proves the copy matches what upstream
# published at that tag, without trusting a fetch done here.
set -euo pipefail

cd "$(dirname "$0")/../third_party/go-cache-plugin"

MOD=github.com/tailscale/go-cache-plugin

# Whatever the go.mod says, that is what we compare against -- so bumping
# the pin without re-vendoring fails here rather than shipping.
version="$(go list -m -f '{{.Version}}' "$MOD")"
go mod download "$MOD"
upstream="$(go env GOMODCACHE)/${MOD}@${version}"

if [ ! -d "$upstream/cmd/go-cache-plugin" ]; then
    echo "module cache has no cmd/go-cache-plugin for ${MOD}@${version}" >&2
    exit 1
fi

# A guard that compared nothing would pass forever.
count=$(find cmd/go-cache-plugin -name '*.go' -type f | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
    echo "no vendored .go files found -- this guard swept nothing" >&2
    exit 2
fi

fail=0
if ! diff -r "$upstream/cmd/go-cache-plugin" cmd/go-cache-plugin; then
    echo "::error::vendored cmd/go-cache-plugin differs from ${MOD}@${version}" >&2
    fail=1
fi

if ! diff "$upstream/LICENSE" LICENSE >/dev/null; then
    echo "::error::vendored LICENSE differs from ${MOD}@${version}" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo >&2
    echo "Re-vendor from the module cache, or restore the pin:" >&2
    echo "  cp ${upstream}/cmd/go-cache-plugin/*.go third_party/go-cache-plugin/cmd/go-cache-plugin/" >&2
    echo "  cp ${upstream}/LICENSE third_party/go-cache-plugin/LICENSE" >&2
    exit 1
fi

echo "vendored cmd/go-cache-plugin (${count} files) + LICENSE match ${MOD}@${version}"
