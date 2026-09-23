#!/usr/bin/env bash
# The docs are the contract: a behaviour nobody wrote down is not a
# behaviour, and a chart value nobody documented is a value somebody will
# have to read the templates to understand.
#
# Three claims are checked, each one a promise a reader is entitled to make:
#
#   1. every front-end path in the architecture table has a client page,
#   2. every leaf key in the chart's values appears somewhere under docs/,
#   3. no relative link between files in this repository is broken.
#
# Each failure names its offender, because a gate that only says "docs are
# wrong" is a gate people learn to re-run rather than read.
#
# Nothing beyond coreutils, grep and awk: this runs in CI, in devbox and on a
# laptop, and a docs check that needs a toolchain is a docs check that gets
# skipped.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
note() { printf '%s\n' "$*"; }
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

# ---------------------------------------------------------------------------
# 1. Every front-end path has a client page.
# ---------------------------------------------------------------------------
#
# The architecture page's front-end table is the list of paths the data port
# serves. Each path's FIRST segment names its client page: /go/build and
# /go/mod are both docs/clients/go.md, /maven/<name> is docs/clients/maven.md.
# The probes are not a protocol anybody configures a client for, so they are
# the one exception and are listed here rather than guessed at.
arch="docs/architecture.md"
probe_paths="healthz readyz"

if [ ! -f "$arch" ]; then
  bad "$arch is missing: the front-end table is the source for this check"
else
  # Table rows only -- lines that begin a cell with a backticked path -- and
  # every backticked /path in that first cell, since a row may list more than
  # one (/go/mod and its sumdb subtree).
  #
  # `|| true` on the pipeline, not because a miss is acceptable, but because
  # under `pipefail` a grep that matches nothing would end the script here --
  # exiting non-zero with nothing printed, which is the least useful way a
  # gate can fail. The empty case is caught below, with a message.
  paths=$( { grep -E '^\| *`/' "$arch" \
             | awk -F'|' '{print $2}' \
             | grep -oE '`/[a-zA-Z0-9<>/_-]+`' \
             | tr -d '`' \
             | awk -F/ '{print $2}' \
             | sort -u; } || true )

  checked=0
  for seg in $paths; do
    case " $probe_paths " in
      *" $seg "*) continue ;;
    esac
    checked=$((checked + 1))
    if [ ! -f "docs/clients/$seg.md" ]; then
      bad "front-end /$seg is in $arch's table but docs/clients/$seg.md does not exist"
    fi
  done

  # A sweep that scanned nothing must not pass. If the table is renamed,
  # reformatted or deleted, this check silently checks zero paths -- which
  # looks exactly like success.
  if [ "$checked" -eq 0 ]; then
    bad "no front-end paths found in $arch: the table's shape changed, and this check just scanned nothing"
  else
    note "front-ends: $checked path prefixes in $arch, each with a client page"
  fi
fi

# ---------------------------------------------------------------------------
# 2. Every chart value is documented.
# ---------------------------------------------------------------------------
#
# Skipped keys: the chart's own Kubernetes boilerplate. These are Helm
# conventions a deployer already knows, they mean the same thing in every
# chart in the estate, and documenting them would be restating Helm rather
# than saying anything about this cache.
skip_sections="image resources nodeSelector tolerations affinity podAnnotations priorityClassName fullnameOverride nameOverride"

values="charts/ci-cache/values.yaml"
if [ ! -f "$values" ]; then
  note "chart values: skipped: chart not present ($values)"
else
  # Leaf keys, as dotted paths. A key with a scalar, an empty map or an empty
  # list is a leaf; so is one whose only children are list items, since a list
  # is a value. Comments -- whole-line and trailing -- are removed first.
  leaves=$(awk '
    { line = $0 }
    line ~ /^[ \t]*#/ { next }
    { sub(/[ \t]+#.*$/, "", line) }
    line ~ /^[ \t]*$/ { next }
    {
      indent = match(line, /[^ ]/) - 1
      if (line ~ /^[ \t]*- /) { n++; ind[n] = indent; key[n] = "-"; val[n] = ""; next }
      if (match(line, /^[ \t]*[A-Za-z0-9_.-]+:/)) {
        k = line
        sub(/^[ \t]*/, "", k)
        sub(/:.*$/, "", k)
        v = line
        sub(/^[ \t]*[A-Za-z0-9_.-]+:[ \t]*/, "", v)
        n++; ind[n] = indent; key[n] = k; val[n] = v
      }
    }
    END {
      depth = 0
      for (i = 1; i <= n; i++) {
        if (key[i] == "-") continue
        while (depth > 0 && stack_ind[depth] >= ind[i]) depth--
        path = ""
        for (d = 1; d <= depth; d++) path = path stack_key[d] "."
        path = path key[i]

        leaf = 1
        if (val[i] == "") {
          # Look ahead: a deeper KEY makes this a parent; a deeper list item
          # does not, because the list is this key value.
          for (j = i + 1; j <= n; j++) {
            if (ind[j] <= ind[i]) break
            if (key[j] != "-") { leaf = 0; break }
          }
        }
        if (leaf) print path
        else { depth++; stack_ind[depth] = ind[i]; stack_key[depth] = key[i] }
      }
    }
  ' "$values")

  if [ -z "$leaves" ]; then
    bad "no leaf keys parsed out of $values: the file's shape changed, and this check just scanned nothing"
  else
    total=0
    skipped=0
    for path in $leaves; do
      section=${path%%.*}
      case " $skip_sections " in
        *" $section "*) skipped=$((skipped + 1)); continue ;;
      esac
      total=$((total + 1))
      # The dotted path is what a deploy page should spell. A bare leaf name
      # is accepted as a fallback -- a page may write `keyPrefix` in prose
      # around the full path -- but the failure always names the full path,
      # because that is what somebody has to go and document.
      leaf=${path##*.}
      if grep -rqF -- "$path" docs/ 2>/dev/null; then
        continue
      fi
      if grep -rqE -- "(^|[^A-Za-z0-9_.])${leaf}([^A-Za-z0-9_]|$)" docs/ 2>/dev/null; then
        continue
      fi
      bad "chart value ${path} appears nowhere under docs/: a value nobody documented is a value nobody can set"
    done
    note "chart values: $total leaf keys checked, $skipped skipped as chart boilerplate"
  fi
fi

# ---------------------------------------------------------------------------
# 3. No broken relative link.
# ---------------------------------------------------------------------------
#
# Only relative targets: an external URL is somebody else's uptime, and an
# anchor within a page is not something this can verify without parsing
# headings. The anchor is stripped and the file is what is checked.
files=$(find . -name '*.md' \
        -not -path './.git/*' \
        -not -path './.devbox/*' \
        -not -path './node_modules/*' \
        -not -path './dist/*' | sort)

if [ -z "$files" ]; then
  bad "no markdown files found: this check just scanned nothing"
else
  links=0
  for f in $files; do
    dir=$(dirname "$f")
    # ](target) only. Reference-style links are not used in this repository;
    # if they ever are, this check goes quiet about them, so it counts what
    # it found and the count is printed below.
    targets=$(grep -oE '\]\([^)]+\)' "$f" | sed -e 's/^](//' -e 's/)$//' || true)
    for t in $targets; do
      case "$t" in
        http://*|https://*|mailto:*|'#'*) continue ;;
      esac
      target=${t%%#*}
      [ -z "$target" ] && continue
      links=$((links + 1))
      if [ ! -e "$dir/$target" ]; then
        bad "$f links to $t, which does not exist"
      fi
    done
  done
  note "links: $links relative links checked across $(printf '%s\n' "$files" | wc -l | tr -d ' ') markdown files"
fi

if [ "$fail" = 0 ]; then
  note "docs check clean"
fi
exit "$fail"
