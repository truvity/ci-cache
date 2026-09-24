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

base="GOCACHEPROG GOCACHE_DIR GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_REGION"
# Sorted in the C locale, which is where the optional keys land: S3_BUCKET
# < S3_ENDPOINT_URL < S3_PATH_STYLE < S3_REGION, and GOCACHE* before GOPROXY
# because 'C' < 'P'. Writing these out rather than composing them keeps the
# expectation a statement about the action and not about my sort order.
endpoint_want="GOCACHEPROG GOCACHE_DIR GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_ENDPOINT_URL GOCACHE_S3_REGION"
pathstyle_want="GOCACHEPROG GOCACHE_DIR GOCACHE_KEY_PREFIX GOCACHE_METRICS GOCACHE_S3_BUCKET GOCACHE_S3_PATH_STYLE GOCACHE_S3_REGION"
go_case "aws defaults"   "$base"
# An empty endpoint must not be WRITTEN empty: the SDK takes it literally
# and then fails to resolve, which looks like a broken bucket.
go_case "with endpoint"  "$endpoint_want" ENDPOINT=https://x.r2.cloudflarestorage.com
go_case "with pathstyle" "$pathstyle_want" PATH_STYLE=true
go_case "with goproxy"   "$base GOPROXY" GOPROXY_IN=https://proxy.golang.org,direct

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
