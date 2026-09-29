#!/usr/bin/env bash
# The Go cache must never hold a build up. This proves it with real builds.
#
# Each case builds a small program from a COLD cache through the real
# go-cache-plugin (built from third_party/, the binary this repository
# ships), started by setup/gocacheprog, against an AWS config whose
# `credential_process` is a stub. A cold build asks the cache for the whole
# runtime, a few hundred requests, which is what makes a per-request cost
# visible.
#
# The stub counts its own calls. That count is the assertion that matters:
# a time bound alone passes on a fast machine whatever the code does. And
# the control case runs the SAME build without the wrapper and expects the
# count to keep climbing, so this file fails if the stub, the config or the
# plugin stopped exercising the credential path at all.
set -uo pipefail
export LC_ALL=C

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
wrapper="$here/setup/gocacheprog"

command -v go >/dev/null || { echo "go is required: devbox provides it" >&2; exit 2; }
[ -x "$wrapper" ] || { echo "$wrapper is missing or not executable" >&2; exit 2; }
[ -x "$here/setup/credential-guard" ] || { echo "setup/credential-guard is missing or not executable" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/home"

echo "building go-cache-plugin from third_party/ ..."
if ! (cd "$here/third_party/go-cache-plugin" && GOWORK=off go build -o "$work/bin/go-cache-plugin" ./cmd/go-cache-plugin); then
    echo "could not build go-cache-plugin" >&2
    exit 2
fi

# A credential_process refused after half a second: about what a token
# exchange costs to be turned down, two round trips.
cat > "$work/bin/refuse" << 'EOF'
#!/bin/sh
echo call >> "$CALLS"
sleep 0.5
echo "the exchange was refused: subject_token is invalid" >&2
exit 1
EOF
# One that never answers. The SDK's own limit is a minute per call.
cat > "$work/bin/hang" << 'EOF'
#!/bin/sh
echo call >> "$CALLS"
exec sleep 300
EOF
# One that works. The keys are placeholders; the bucket below refuses
# connections, so they are never checked by anything.
cat > "$work/bin/grant" << 'EOF'
#!/bin/sh
echo call >> "$CALLS"
printf '{"Version":1,"AccessKeyId":"placeholder","SecretAccessKey":"placeholder"}\n'
EOF
chmod +x "$work/bin/refuse" "$work/bin/hang" "$work/bin/grant"

mkdir -p "$work/mod"
printf 'module example.com/failopen\n\ngo 1.24\n' > "$work/mod/go.mod"
cat > "$work/mod/main.go" << 'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func main() {
	b, _ := json.Marshal(map[string]int{"ok": 1})
	fmt.Println(string(b), http.StatusOK)
}
EOF

fail=0
checked=0

# build LABEL GOCACHEPROG CREDENTIAL_PROCESS BOUND [VAR=VALUE...]
#
# Runs one cold build with a timeout of BOUND seconds and leaves rc, the
# elapsed seconds, the stub's call count and the go command's stderr in
# globals for the caller to judge.
build() {
    local label="$1" prog="$2" process="$3" bound="$4"; shift 4
    local d="$work/case-$checked"
    checked=$((checked + 1))
    mkdir -p "$d/gocache" "$d/localdir"
    : > "$d/calls"
    {
        printf '[profile cache]\nregion = auto\n'
        [ -n "$process" ] && printf 'credential_process = %s\n' "$work/bin/$process"
    } > "$d/aws.ini"

    local start end
    start="$(date +%s)"
    (
        cd "$work/mod" &&
        env -i \
            PATH="$work/bin:$(dirname "$(command -v go)"):/usr/bin:/bin" \
            HOME="$work/home" TMPDIR="$d" \
            GOCACHE="$d/localdir" GOPATH="$work/gopath" GOTOOLCHAIN=local \
            GOPROXY=off GOFLAGS= GOWORK=off CGO_ENABLED=0 \
            GOCACHEPROG="$prog" \
            GOCACHE_DIR="$d/gocache" GOCACHE_S3_BUCKET=bucket GOCACHE_S3_REGION=auto \
            GOCACHE_S3_ENDPOINT_URL=http://127.0.0.1:1 GOCACHE_S3_PATH_STYLE=true \
            GOCACHE_EXPIRY=0 GOCACHE_METRICS=1 \
            AWS_CONFIG_FILE="$d/aws.ini" AWS_SHARED_CREDENTIALS_FILE="$d/none" \
            AWS_PROFILE=cache AWS_EC2_METADATA_DISABLED=true AWS_MAX_ATTEMPTS=1 \
            CALLS="$d/calls" \
            "$@" \
            timeout -k 5 "$bound" go build -o "$d/out" .
    ) 2> "$d/stderr"
    rc=$?
    end="$(date +%s)"
    elapsed=$((end - start))
    calls="$(wc -l < "$d/calls" | tr -d ' ')"
    stderr="$d/stderr"
    echo "  [$label] rc=$rc ${elapsed}s credential calls=$calls"
}

bad() { echo "FAIL [$1]: $2"; sed 's/^/    | /' "$stderr" | tail -15; fail=$((fail + 1)); }

off_lines() { grep -c 'ci-cache: the Go build cache could not get credentials' "$stderr"; }

# --- the incident: a credential that is refused, every time. The build must
# finish, well inside the bound, having asked at most CI_CACHE_GO_CRED_ATTEMPTS
# (3) times, and say so exactly once.
build "refused, wrapped" "$wrapper" refuse 120
[ "$rc" -eq 0 ] || bad "refused, wrapped" "the build failed or hit the bound (rc=$rc)"
[ "$calls" -ge 1 ] && [ "$calls" -le 3 ] || bad "refused, wrapped" "credential_process ran $calls times; want 1..3"
[ "$(off_lines)" -eq 1 ] || bad "refused, wrapped" "want exactly one 'could not get credentials' line, got $(off_lines)"
[ "$elapsed" -le 60 ] || bad "refused, wrapped" "took ${elapsed}s; want at most 60s"

# --- the control: the same build with the plugin started directly. It keeps
# asking, which is the bug; if it does not, this file is testing nothing.
build "refused, unwrapped (control)" go-cache-plugin refuse 20
[ "$calls" -gt 10 ] || bad "refused, unwrapped (control)" "credential_process ran only $calls times: the stub is not on the plugin's credential path, so the wrapped case proves nothing"

# --- a credential_process that never answers. The time budget, not the
# attempt count, is what ends it: one call, cut off at 10s.
build "hanging, wrapped" "$wrapper" hang 120
[ "$rc" -eq 0 ] || bad "hanging, wrapped" "the build failed or hit the bound (rc=$rc)"
[ "$calls" -eq 1 ] || bad "hanging, wrapped" "credential_process ran $calls times; want 1"
grep -q 'timed out after 10s' "$stderr" || bad "hanging, wrapped" "the line does not say it timed out"
[ "$elapsed" -le 60 ] || bad "hanging, wrapped" "took ${elapsed}s; want at most 60s"

# --- a credential that works goes through untouched: asked for once, since
# it carries no expiry, and nothing announces a failure.
build "granted, wrapped" "$wrapper" grant 120
[ "$rc" -eq 0 ] || bad "granted, wrapped" "the build failed (rc=$rc)"
[ "$calls" -eq 1 ] || bad "granted, wrapped" "credential_process ran $calls times; want 1"
[ "$(off_lines)" -eq 0 ] || bad "granted, wrapped" "announced a credential failure that did not happen"

# --- a profile that is named but not configured. Unwrapped, the SDK refuses
# to start and the build FAILS; wrapped, it runs local-only and says why.
build "missing profile, wrapped" "$wrapper" "" 120 AWS_PROFILE=nonesuch
[ "$rc" -eq 0 ] || bad "missing profile, wrapped" "the build failed (rc=$rc)"
grep -q 'local-only for this go command' "$stderr" || bad "missing profile, wrapped" "no line says the cache is local-only"

build "missing profile, unwrapped (control)" go-cache-plugin "" 60 AWS_PROFILE=nonesuch
[ "$rc" -ne 0 ] || bad "missing profile, unwrapped (control)" "the build passed without the wrapper, so the wrapped case proves nothing"

if [ "$checked" -eq 0 ]; then
    echo "no cases ran" >&2
    exit 2
fi
if [ "$fail" -ne 0 ]; then
    echo "$fail failure(s) across $checked cases"
    exit 1
fi
echo "gocacheprog: $checked cases, all fail open"
