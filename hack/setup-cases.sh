#!/usr/bin/env bash
# Execute the setup action's step bodies against a stubbed environment.
#
# A composite action is shell in a YAML envelope, and actionlint does not
# read it. The only way to know what a step does is to run it, so this
# extracts each step's `run` by NAME -- a rename is a loud failure rather
# than a silently skipped case -- and executes it with the inputs GitHub
# would have set.
#
# The pattern is ci-workflows' hack/cache-env-cases.sh, which earned its
# place by catching a step-precedence bug that reading could not.
set -uo pipefail

# The C locale, for the whole script: `sort` orders differently in a UTF-8
# locale (it ignores the underscore, putting GOCACHE_DIR before GOCACHEPROG
# where the C locale does the opposite), and this compares that order as a
# string. Without it the harness passes on one machine and fails on the
# runner.
export LC_ALL=C

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
action="$here/setup/action.yaml"
[ -f "$action" ] || { echo "no action at $action" >&2; exit 2; }

command -v yq >/dev/null || { echo "yq (mikefarah) is required: devbox provides yq-go" >&2; exit 2; }

fail=0
checked=0

extract() {
    yq -r ".runs.steps[] | select(.name == \"$1\") | .run" "$action"
}

# names prints the variable names a run emitted, sorted. Values are ignored
# on purpose: they carry random heredoc delimiters, and what must not drift
# is WHICH variables a shape sets.
names() {
    grep -oE '^[A-Z_][A-Z0-9_]*<<|^[A-Z_][A-Z0-9_]*=' "$1" \
        | sed 's/<<$//; s/=$//' | LC_ALL=C sort -u | tr '\n' ' ' | sed 's/ $//'
}

# outputs prints what a step wrote to GITHUB_OUTPUT, as key=value lines.
out_get() { grep -E "^$2=" "$1" | tail -1 | cut -d= -f2-; }

# ---------------------------------------------------------------------------
# Detection reads the tree. Every marker is a file the build system cannot
# work without, so a false positive means the marker was wrong, not that the
# repository is unusual.
# ---------------------------------------------------------------------------
detect_case() {
    local label="$1" want="$2" override="${3:-}"; shift 3 2>/dev/null || shift 2
    checked=$((checked + 1))

    local d; d="$(mktemp -d)"
    mkdir -p "$d/repo"; cd "$d/repo" || return
    for f in "$@"; do mkdir -p "$(dirname "$f")"; touch "$f"; done

    extract "Detect build systems and choose a cache directory" > "$d/step.sh"
    : > "$d/out"; : > "$d/env"
    env -i PATH="/usr/bin:/bin" HOME="$d" \
        GITHUB_OUTPUT="$d/out" GITHUB_ENV="$d/env" RUNNER_TEMP="$d/tmp" \
        RUNNER_ENVIRONMENT=self-hosted \
        INPUT_LANGUAGES="$override" INPUT_BUCKET=b \
        bash "$d/step.sh" >/dev/null 2>&1

    local got; got="$(out_get "$d/out" languages)"
    if [ "$got" != "$want" ]; then
        echo "FAIL [detect $label]: languages=\"$got\", want \"$want\""
        fail=$((fail + 1))
    fi
    cd "$here" || true; rm -rf "$d"
}

detect_case "go only"        "go"                  "" go.mod
detect_case "nix via devbox" "nix"                 "" devbox.json
detect_case "nix via flake"  "nix"                 "" flake.nix
detect_case "yarn"           "yarn"                "" yarn.lock
detect_case "moon"           "moon"                "" .moon/workspace.yml
detect_case "gradle kts"     "gradle"              "" build.gradle.kts
detect_case "bar-shaped"     "go nix yarn moon"    "" go.mod devbox.json yarn.lock .moon/workspace.yml
detect_case "empty tree"     ""                    ""
# The override exists for the tree that genuinely does not say. It must win
# outright, including over markers that are present.
detect_case "override wins"  "gradle"              "gradle" go.mod devbox.json

# ---------------------------------------------------------------------------
# Fail open. A runner that cannot reach a backend gets the plain toolchain
# caches and a message -- never a failed job. The one exception is a client
# that fails checksum verification, which is not reachable from these steps.
# ---------------------------------------------------------------------------
usable_case() {
    local label="$1" want="$2"; shift 2
    checked=$((checked + 1))

    local d; d="$(mktemp -d)"
    mkdir -p "$d/repo"; cd "$d/repo" || return; touch go.mod
    extract "Detect build systems and choose a cache directory" > "$d/step.sh"
    : > "$d/out"
    env -i PATH="/usr/bin:/bin" HOME="$d" \
        GITHUB_OUTPUT="$d/out" GITHUB_ENV="$d/env" RUNNER_TEMP="$d/tmp" \
        "$@" bash "$d/step.sh" >/dev/null 2>&1
    local rc=$?

    local got; got="$(out_get "$d/out" usable)"
    if [ "$got" != "$want" ]; then
        echo "FAIL [usable $label]: usable=\"$got\", want \"$want\""
        fail=$((fail + 1))
    fi
    if [ "$rc" -ne 0 ]; then
        echo "FAIL [usable $label]: the step exited $rc; it must fail open, not fail"
        fail=$((fail + 1))
    fi
    cd "$here" || true; rm -rf "$d"
}

usable_case "self-hosted with a bucket" yes RUNNER_ENVIRONMENT=self-hosted  INPUT_BUCKET=b INPUT_LANGUAGES=
usable_case "github-hosted"             no  RUNNER_ENVIRONMENT=github-hosted INPUT_BUCKET=b INPUT_LANGUAGES=
usable_case "no bucket"                 no  RUNNER_ENVIRONMENT=self-hosted  INPUT_BUCKET=  INPUT_LANGUAGES=

# ---------------------------------------------------------------------------
# The cache directory. The node-persistent mount is ci-plane's to provide
# and this action's to use; neither hard-codes the path, so the contract is
# the variable and the fallback must be silent and correct.
# ---------------------------------------------------------------------------
dir_case() {
    local label="$1" wantkind="$2" nodedir="$3"
    checked=$((checked + 1))

    local d; d="$(mktemp -d)"
    mkdir -p "$d/repo"; cd "$d/repo" || return; touch go.mod
    case "$nodedir" in /tmp/*) mkdir -p "$nodedir" ;; esac
    extract "Detect build systems and choose a cache directory" > "$d/step.sh"
    : > "$d/out"
    env -i PATH="/usr/bin:/bin" HOME="$d" \
        GITHUB_OUTPUT="$d/out" GITHUB_ENV="$d/env" RUNNER_TEMP="$d/tmp" \
        RUNNER_ENVIRONMENT=self-hosted INPUT_BUCKET=b INPUT_LANGUAGES= \
        CI_CACHE_NODE_DIR="$nodedir" \
        bash "$d/step.sh" >/dev/null 2>&1

    local got; got="$(out_get "$d/out" dirkind)"
    if [ "$got" != "$wantkind" ]; then
        echo "FAIL [dir $label]: dirkind=\"$got\", want \"$wantkind\""
        fail=$((fail + 1))
    fi
    cd "$here" || true; rm -rf "$d"
}

node="$(mktemp -d)"
dir_case "node dir present" node-persistent "$node"
dir_case "node dir unset"   job-local       ""
# A named directory that does not exist is a scale set that lost its mount.
# Falling back is right; pretending it is there is not.
dir_case "node dir missing" job-local       "/nonexistent/ci-cache-$$"
rm -rf "$node"

# ---------------------------------------------------------------------------
# The fetch step. The one step in this file that talks to the network, and
# the one step that fails CLOSED -- so it earns more cases than a step that
# only sets variables, not fewer.
#
# curl is stubbed: this harness has to run on a hosted runner with no
# backend and no real download. The stub serves a file out of $SERVER_DIR
# by the basename curl was asked to -o, so building a case is "put a file
# where the download would have landed", never a real network call.
# ---------------------------------------------------------------------------
fetch_stub_curl() {
    # $1 = bin dir to install the stub into.
    cat > "$1/curl" << 'STUB'
#!/bin/sh
out=""
url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        -f|-s|-S|-L|-fsSL) shift ;;
        *) url="$1"; shift ;;
    esac
done
name="${url##*/}"
[ -n "${FETCH_SPY:-}" ] && : >> "$FETCH_SPY"
if [ -f "$SERVER_DIR/$name" ]; then
    cp "$SERVER_DIR/$name" "$out"
    exit 0
fi
exit 22
STUB
    chmod +x "$1/curl"
}

# sha256sum/shasum here is the HARNESS building a fixture, not the action's
# own network path -- the same tool the action itself falls back to when
# sha256sum is absent (see setup/action.yaml's sha256() helper).
fetch_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# A real archive and a real, correctly-hashed checksums.txt, so the verify
# logic in the action is exercised for real rather than assumed.
fetch_build_server() {
    # $1 = server dir, $2 = archive filename.
    mkdir -p "$1"
    local work; work="$(mktemp -d)"
    printf '#!/bin/sh\nexit 0\n' > "$work/go-cache-plugin"; chmod +x "$work/go-cache-plugin"
    echo "BSD-3-Clause" > "$work/LICENSE"
    tar -czf "$1/$2" -C "$work" go-cache-plugin LICENSE
    rm -rf "$work"
    printf '%s  %s\n' "$(fetch_sha256 "$1/$2")" "$2" > "$1/checksums.txt"
}

# $1 = work dir, $2 = CLIENT_VERSION, $3 = RUNNER_OS, $4 = RUNNER_ARCH,
# $5 = server dir (may be empty or missing files, on purpose).
fetch_run() {
    local d="$1"
    mkdir -p "$d/bin" "$d/tmp"
    fetch_stub_curl "$d/bin"
    extract "Fetch this release's go-cache-plugin" > "$d/step.sh"
    : > "$d/path"
    env -i PATH="$d/bin:/usr/bin:/bin" HOME="$d" \
        RUNNER_TEMP="$d/tmp" GITHUB_PATH="$d/path" \
        SERVER_DIR="$5" FETCH_SPY="$d/curl-called" \
        CLIENT_VERSION="$2" RUNNER_OS="$3" RUNNER_ARCH="$4" \
        bash "$d/step.sh" >"$d/log" 2>&1
}

# --- no client-version: the fast default path, and it must cost NOTHING on
# the network, whether or not the step-level `if:` is the thing skipping it.
checked=$((checked + 1))
d="$(mktemp -d)"
fetch_run "$d" "" Linux X64 "$d/unused-server"
rc=$?
[ "$rc" -ne 0 ] && { echo "FAIL [fetch empty]: exited $rc; empty client-version must be a no-op"; fail=$((fail + 1)); }
[ -e "$d/curl-called" ] && { echo "FAIL [fetch empty]: curl was invoked with no client-version set"; fail=$((fail + 1)); }
[ -s "$d/path" ] && { echo "FAIL [fetch empty]: wrote to GITHUB_PATH with no client-version set"; fail=$((fail + 1)); }
rm -rf "$d"

# --- a valid version: the binary lands where GITHUB_PATH now points, and
# the log names the version as verified.
checked=$((checked + 1))
d="$(mktemp -d)"
archive="go-cache-plugin_9.9.9_linux_amd64.tar.gz"
fetch_build_server "$d/server" "$archive"
fetch_run "$d" "9.9.9" Linux X64 "$d/server"
rc=$?
if [ "$rc" -ne 0 ]; then
    echo "FAIL [fetch valid]: exited $rc; log:"
    sed 's/^/     /' "$d/log"
    fail=$((fail + 1))
fi
bindir="$(head -1 "$d/path" 2>/dev/null)"
if [ -z "$bindir" ] || [ ! -x "$bindir/go-cache-plugin" ]; then
    echo "FAIL [fetch valid]: go-cache-plugin did not land where GITHUB_PATH points"
    fail=$((fail + 1))
fi
grep -q "verified" "$d/log" || { echo "FAIL [fetch valid]: no line naming the version as verified"; fail=$((fail + 1)); }
rm -rf "$d"

# --- a tampered checksums.txt: the one hard failure in this file. A hash
# that matches nothing in the caller's own checksums.txt is, from this
# step's side, indistinguishable from a MITM or a corrupted upload -- so it
# must fail CLOSED, name the file, and never touch GITHUB_PATH.
checked=$((checked + 1))
d="$(mktemp -d)"
archive="go-cache-plugin_9.9.9_linux_amd64.tar.gz"
fetch_build_server "$d/server" "$archive"
printf '0000000000000000000000000000000000000000000000000000000000000000  %s\n' "$archive" > "$d/server/checksums.txt"
fetch_run "$d" "9.9.9" Linux X64 "$d/server"
rc=$?
[ "$rc" -eq 0 ] && { echo "FAIL [fetch tampered]: exited 0; a checksum mismatch must fail CLOSED"; fail=$((fail + 1)); }
grep -q "$archive" "$d/log" || { echo "FAIL [fetch tampered]: failure message did not name $archive"; fail=$((fail + 1)); }
[ -s "$d/path" ] && { echo "FAIL [fetch tampered]: wrote to GITHUB_PATH despite the checksum mismatch"; fail=$((fail + 1)); }
rm -rf "$d"

# --- a missing release asset (this version's release does not exist yet,
# including the very first one, before its own tag): fail OPEN, same as
# every other absence in this file.
checked=$((checked + 1))
d="$(mktemp -d)"
mkdir -p "$d/server" # nothing in it to serve
fetch_run "$d" "9.9.9" Linux X64 "$d/server"
rc=$?
[ "$rc" -ne 0 ] && { echo "FAIL [fetch missing asset]: exited $rc; a release that doesn't exist yet must fail open"; fail=$((fail + 1)); }
grep -q "::warning::" "$d/log" || { echo "FAIL [fetch missing asset]: said nothing; a job that fell back must say so"; fail=$((fail + 1)); }
rm -rf "$d"

# --- an unrecognised OS/arch pair: warn and fall through, never fail, and
# never even reach the network for a runner this action cannot map.
checked=$((checked + 1))
d="$(mktemp -d)"
fetch_run "$d" "9.9.9" Windows X86 "$d/unused-server"
rc=$?
[ "$rc" -ne 0 ] && { echo "FAIL [fetch unrecognised]: exited $rc; an unmapped runner must fail open"; fail=$((fail + 1)); }
[ -e "$d/curl-called" ] && { echo "FAIL [fetch unrecognised]: curl was invoked for a runner this action does not map"; fail=$((fail + 1)); }
grep -q "::warning::" "$d/log" || { echo "FAIL [fetch unrecognised]: said nothing about the unrecognised runner"; fail=$((fail + 1)); }
rm -rf "$d"

# ---------------------------------------------------------------------------
# The Go step. What it sets, and what it must not set.
# ---------------------------------------------------------------------------
go_case() {
    local label="$1" want="$2"; shift 2
    checked=$((checked + 1))

    local d; d="$(mktemp -d)"
    mkdir -p "$d/bin"
    printf '#!/bin/sh\nexit 0\n' > "$d/bin/go-cache-plugin"; chmod +x "$d/bin/go-cache-plugin"
    extract "Wire the Go build cache" > "$d/step.sh"
    : > "$d/env"
    env -i PATH="$d/bin:/usr/bin:/bin" HOME="$d" GITHUB_ENV="$d/env" \
        DIR="$d/cache" BUCKET=b REGION=r ENDPOINT= PATH_STYLE= GOPROXY_IN= \
        DIRKIND=job-local \
        "$@" bash "$d/step.sh" >/dev/null 2>&1
    local rc=$?

    local got; got="$(names "$d/env")"
    if [ "$got" != "$want" ]; then
        echo "FAIL [go $label]: variables set:"
        echo "     got:  $got"
        echo "     want: $want"
        fail=$((fail + 1))
    fi
    if [ "$rc" -ne 0 ]; then
        echo "FAIL [go $label]: exited $rc"
        fail=$((fail + 1))
    fi
    cd "$here" || true; rm -rf "$d"
}

# GOCACHE_EXPIRY sorts between DIR and KEY_PREFIX, and GOCACHEPROG still
# comes first: in the C locale 'P' (0x50) precedes '_' (0x5F).
base="GOCACHEPROG GOCACHE_DIR GOCACHE_EXPIRY GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_REGION"
# Sorted in the C locale, which is where the optional keys land: S3_BUCKET
# < S3_ENDPOINT_URL < S3_PATH_STYLE < S3_REGION, and GOCACHE* before GOPROXY
# because 'C' < 'P'. Writing these out rather than composing them keeps the
# expectation a statement about the action and not about my sort order.
endpoint_want="GOCACHEPROG GOCACHE_DIR GOCACHE_EXPIRY GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_ENDPOINT_URL GOCACHE_S3_REGION"
pathstyle_want="GOCACHEPROG GOCACHE_DIR GOCACHE_EXPIRY GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_PATH_STYLE GOCACHE_S3_REGION"
go_case "aws defaults"   "$base"
# An empty endpoint must not be WRITTEN empty: the SDK takes it literally
# and then fails to resolve, which looks like a broken bucket.
go_case "with endpoint"  "$endpoint_want" ENDPOINT=https://x.r2.cloudflarestorage.com
go_case "with pathstyle" "$pathstyle_want" PATH_STYLE=true
go_case "with goproxy"   "$base GOPROXY" GOPROXY_IN=https://proxy.golang.org,direct

# ---------------------------------------------------------------------------
# GOCACHE_EXPIRY on a SHARED directory.
#
# This is the one setting that decides whether a node-persistent cache dir
# is safe. go-cache-plugin prunes by cachedir.Cleanup(expiry), which is a
# no-op at <= 0; above zero it mark-and-sweeps the whole directory at job
# close, deleting every object its mark phase did not see -- including one
# another runner on the same node wrote a moment ago and is still building
# against.
#
# So the action pins it to 0 always, and SAYS SO when the directory is
# shared and somebody had set it. Both halves are cases: a pin nobody can
# see is a pin nobody will keep, and a warning that does not fire is the
# failure mode this whole file exists for.
# ---------------------------------------------------------------------------
# go_case above compares WHICH variables a shape sets; this compares the
# one VALUE that has a safety property attached to it.
expiry_case() {
    local label="$1" dirkind="$2" incoming="$3" want_warn="$4"
    checked=$((checked + 1))

    local d; d="$(mktemp -d)"
    mkdir -p "$d/bin"
    printf '#!/bin/sh\nexit 0\n' > "$d/bin/go-cache-plugin"; chmod +x "$d/bin/go-cache-plugin"
    extract "Wire the Go build cache" > "$d/step.sh"
    : > "$d/env"

    local -a extra=()
    [ -n "$incoming" ] && extra=(GOCACHE_EXPIRY="$incoming")

    env -i PATH="$d/bin:/usr/bin:/bin" HOME="$d" GITHUB_ENV="$d/env" \
        DIR="$d/cache" BUCKET=b REGION=r ENDPOINT= PATH_STYLE= GOPROXY_IN= \
        DIRKIND="$dirkind" "${extra[@]}" \
        bash "$d/step.sh" >"$d/log" 2>&1

    local got; got="$(grep -E '^GOCACHE_EXPIRY=' "$d/env" | tail -1 | cut -d= -f2-)"
    if [ "$got" != "0" ]; then
        echo "FAIL [expiry $label]: GOCACHE_EXPIRY=\"$got\" in the job env, want 0"
        fail=$((fail + 1))
    fi

    local warned=no
    grep -q '::warning::.*GOCACHE_EXPIRY' "$d/log" && warned=yes
    if [ "$warned" != "$want_warn" ]; then
        echo "FAIL [expiry $label]: warned=$warned, want $want_warn"
        echo "     log: $(cat "$d/log")"
        fail=$((fail + 1))
    fi

    rm -rf "$d"
}

#            label                        dirkind          incoming  warn?
expiry_case "shared, caller set it"      node-persistent  "168h"    yes
expiry_case "shared, caller set zero"    node-persistent  "0"       no
expiry_case "shared, unset"              node-persistent  ""        no
# A job-local directory is thrown away with the job, so pruning it is work
# nobody benefits from -- pinned off too, but silently: nothing is at risk,
# and a warning on every job of every repository is a warning people learn
# to scroll past.
expiry_case "job-local, caller set it"   job-local        "168h"    no

# A missing client is a slower job, not a failed one -- the runner image may
# predate the binary. The agent's one hard failure (a binary that fails
# checksum verification) is deliberately not this shape.
checked=$((checked + 1))
d="$(mktemp -d)"
extract "Wire the Go build cache" > "$d/step.sh"
: > "$d/env"
env -i PATH="/usr/bin:/bin" HOME="$d" GITHUB_ENV="$d/env" \
    DIR="$d/cache" BUCKET=b REGION=r ENDPOINT= PATH_STYLE= GOPROXY_IN= \
    bash "$d/step.sh" >"$d/log" 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then
    echo "FAIL [go no client]: exited $rc; a missing client must fail open"
    fail=$((fail + 1))
fi
if [ -s "$d/env" ]; then
    echo "FAIL [go no client]: wrote GITHUB_ENV despite having no client"
    fail=$((fail + 1))
fi
if ! grep -q "::warning::" "$d/log"; then
    echo "FAIL [go no client]: said nothing; a job with no cache must say so"
    fail=$((fail + 1))
fi
rm -rf "$d"

# ---------------------------------------------------------------------------
# The repo-file caches are CHECKED, never written. An action that edited
# .yarnrc.yml or .moon/workspace.yml would fight the tree and leave a diff.
# ---------------------------------------------------------------------------
checked=$((checked + 1))
d="$(mktemp -d)"; mkdir -p "$d/repo/.moon"; cd "$d/repo" || exit
printf 'unloadedVersion: 1\n' > .yarnrc.yml
printf 'remote:\n  host: grpc://other:9092\n' > .moon/workspace.yml
before_y="$(cat .yarnrc.yml)"; before_m="$(cat .moon/workspace.yml)"
extract "Check the repository's own cache configuration" > "$d/step.sh"
env -i PATH="/usr/bin:/bin" HOME="$d" \
    LANGS="moon yarn" BAZEL_REMOTE=grpc://estate:9092 NPM_REGISTRY=http://estate/npm \
    bash "$d/step.sh" > "$d/log" 2>&1
rc=$?
[ "$rc" -ne 0 ] && { echo "FAIL [check]: exited $rc"; fail=$((fail + 1)); }
if [ "$(cat .yarnrc.yml)" != "$before_y" ] || [ "$(cat .moon/workspace.yml)" != "$before_m" ]; then
    echo "FAIL [check]: the step EDITED the repository's files; it must only report"
    fail=$((fail + 1))
fi
if ! grep -q "::warning::" "$d/log"; then
    echo "FAIL [check]: a repository pointed elsewhere produced no warning"
    fail=$((fail + 1))
fi
cd "$here" || true; rm -rf "$d"

# ---------------------------------------------------------------------------
# A harness that checked nothing is not a passing harness. This is the
# guard that a refactor silently emptying the cases cannot get past.
# ---------------------------------------------------------------------------
if [ "$checked" -eq 0 ]; then
    echo "FAIL: no cases ran"
    exit 1
fi

if [ "$fail" -ne 0 ]; then
    echo "$fail case(s) failed of $checked"
    exit 1
fi

echo "setup action holds ($checked cases checked)"
