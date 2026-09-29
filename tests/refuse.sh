#!/usr/bin/env bash
# The configurations this chart must REFUSE to render, and the two things the
# NetworkPolicy must be true of.
#
# Each refusal is a configuration `config.Validate` rejects at start-up, or one
# the binary accepts and then gets quietly wrong. A chart that renders one
# anyway moves the failure from `helm install`, where somebody is looking, to a
# CrashLoopBackOff nobody reads -- or, worse, to a cache that answers every
# request with a miss while every dashboard stays green.
#
# A refusal is only checked as far as its MESSAGE: a chart that fails for some
# other reason has not held the rule, it has merely also been broken.
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
chart="$root/charts/ci-cache-server"

# Enough to render. Every case below breaks exactly one thing.
base=(--set store.bucket=ci-cache-example --set store.region=eu-example-1)

fail=0
checked=0

# Not a refusal: the one thing the chart must ASSERT about the container it
# renders. The refusals below all check that a bad input is rejected; none of
# them would notice a render that succeeds and produces a pod which cannot
# run, which is exactly what shipped in 0.1.0 -- no `args`, so the ko
# entrypoint ran the bare CLI, printed its help, exited 0, and crash-looped.
# Every golden was reviewed and every refusal passed.
checked=$((checked + 1))
if ! helm template t "$chart" "${base[@]}" 2>/dev/null \
    | grep -A2 -E '^ +args:' | grep -qE '^ +- serve$'; then
    echo "FAIL: the rendered container does not run \`serve\`"
    echo "      ko's entrypoint is the bare binary; with no subcommand it prints help and exits"
    fail=$((fail + 1))
fi


# The control. Every case below is a refusal, so a chart that refused
# EVERYTHING -- a typo in a helper, say -- would pass all of them and prove
# nothing. This is the render that must succeed.
if ! helm template t "$chart" "${base[@]}" >/dev/null 2>&1; then
    echo "THE BASE DOES NOT RENDER: every refusal below would pass for the wrong reason"
    helm template t "$chart" "${base[@]}" 2>&1 | tail -3 | sed 's/^/    /'
    exit 1
fi
echo "control: the base values render"

refuse() { # <name> <wanted words> <helm --set args...>
    local name=$1 want=$2; shift 2
    checked=$((checked + 1))
    local out
    out=$(helm template t "$chart" "${base[@]}" "$@" 2>&1)
    if [ $? -eq 0 ]; then
        echo "NOT REFUSED: $name"
        fail=1
    elif ! grep -qF "$want" <<< "$out"; then
        echo "REFUSED WITHOUT SAYING WHY: $name"
        echo "    wanted the words: $want"
        echo "    said: $(tail -3 <<< "$out" | tr '\n' ' ')"
        fail=1
    else
        echo "refused: $name"
    fi
}

# 1. A build cache on a volume that empties. Nothing about this fails visibly:
#    every request still answers, always with a miss.
refuse "a build cache without persistence" \
    "persistence.ephemeralIsAcceptable" \
    --set frontends.go.build.enabled=true \
    --set persistence.enabled=false \
    --set persistence.ephemeralIsAcceptable=false

# 2. A non-AWS store with no region. The SDK guesses, signs with the guess, and
#    the first request fails on a signature that reads as bad credentials.
refuse "an endpoint with no region" \
    'is set and `store.region` is empty' \
    --set store.endpoint=https://storage.example.com \
    --set store.region=""

# 3. Static keys and a pod-identity annotation: two identities for one process,
#    and which one wins is a property of the SDK's version.
refuse "two identities at once" \
    "two identities for one process" \
    --set store.existingSecret=ci-cache-keys \
    --set serviceAccount.annotations.identity=some-role

# 4. No bucket. The disk is the fast copy of something; with no something,
#    every restart starts from nothing.
refuse "no bucket" \
    'set `store.bucket`' \
    --set store.bucket=""

# 5. One port for both audiences would put the wipes behind the address every
#    CI job is given.
refuse "one port for data and admin" \
    "must differ" \
    --set service.admin=8080 \
    --set service.data=8080

# 6. The values files under tests/invalid/ci-cache-server/, one per refusal
#    the SCHEMA makes (values.schema.json) rather than a template: a key the
#    chart does not have, a value of the wrong type. Each file's first line is
#    `# refuses: <words>`, and the words must appear in what helm says -- a
#    fixture that fails for some other reason has proved nothing.
invalid="$root/tests/invalid/ci-cache-server"
fixtures=0
for f in "$invalid"/*.yaml; do
    [ -e "$f" ] || continue
    fixtures=$((fixtures + 1))
    want=$(head -1 "$f" | sed -n 's/^# refuses: //p')
    if [ -z "$want" ]; then
        echo "FIXTURE SAYS NOTHING: $f has no '# refuses: <words>' first line"
        fail=1
        continue
    fi
    checked=$((checked + 1))
    out=$(helm template t "$chart" -f "$f" 2>&1)
    if [ $? -eq 0 ]; then
        echo "NOT REFUSED: ${f#"$root"/}"
        fail=1
    elif ! grep -qF -- "$want" <<< "$out"; then
        echo "REFUSED WITHOUT SAYING WHY: ${f#"$root"/}"
        echo "    wanted the words: $want"
        echo "    said: $(tail -3 <<< "$out" | tr '\n' ' ')"
        fail=1
    else
        echo "refused: ${f#"$root"/}"
    fi
done
# A directory that emptied, or a glob that stopped matching, would otherwise
# pass by checking nothing.
if [ "$fixtures" -eq 0 ]; then
    echo "NO FIXTURES under ${invalid#"$root"/}: the schema's refusals went unchecked"
    fail=1
fi

# 7. The NetworkPolicy, asserted positively: the admin port must appear in no
#    ingress rule, and an empty consumer list must admit nobody rather than
#    everybody. The first cannot be written as a refusal -- a namespace name
#    is just a string, and no rule the chart could state would recognise "the
#    admin Service's own namespace" -- so it is checked on what renders.
netpol() { helm template t "$chart" "${base[@]}" -s templates/networkpolicy.yaml "$@" 2>&1; }

checked=$((checked + 1))
out=$(netpol --set networkPolicy.enabled=true \
             --set 'networkPolicy.consumerNamespaces={ci,build}' \
             --set service.data=8080 --set service.admin=8081)
if [ $? -ne 0 ]; then
    echo "DID NOT RENDER: the NetworkPolicy with two consumer namespaces"
    echo "    said: $(tail -3 <<< "$out" | tr '\n' ' ')"
    fail=1
elif ! grep -q "kind: NetworkPolicy" <<< "$out"; then
    # A check that passes because it read nothing has proved nothing.
    echo "NOTHING TO CHECK: no NetworkPolicy rendered"
    fail=1
elif grep -q "8081" <<< "$out"; then
    echo "ADMIN PORT IN THE POLICY: the wipes are reachable from a consumer namespace"
    grep -n "8081" <<< "$out" | sed 's/^/    /'
    fail=1
elif ! grep -q "port: 8080" <<< "$out"; then
    echo "NO DATA PORT IN THE POLICY: it admits the named namespaces to nothing"
    fail=1
else
    echo "holds: the admin port appears in no ingress rule"
fi

checked=$((checked + 1))
out=$(netpol --set networkPolicy.enabled=true)
if [ $? -ne 0 ]; then
    echo "DID NOT RENDER: the NetworkPolicy with no consumer namespaces"
    fail=1
elif ! grep -q "kind: NetworkPolicy" <<< "$out"; then
    echo "NOTHING TO CHECK: no NetworkPolicy rendered for the empty list"
    fail=1
elif grep -qE "^\s+- from:|^\s+from:" <<< "$out"; then
    # The classic inversion: an empty list that renders a rule with an empty
    # `from`, which admits every pod in the cluster and looks identical in a
    # diff to one that admits nobody.
    echo "EMPTY CONSUMER LIST ADMITS EVERYONE: it rendered an ingress rule"
    sed 's/^/    /' <<< "$out"
    fail=1
elif ! grep -q "ingress: \[\]" <<< "$out"; then
    echo "EMPTY CONSUMER LIST DID NOT SAY SO: expected an explicit empty ingress list"
    fail=1
else
    echo "holds: an empty consumer list admits nobody"
fi

if [ "$checked" -eq 0 ]; then
    echo "checked nothing -- that is a failure, not a pass"
    exit 1
fi
if [ "$fail" -eq 0 ]; then
    echo "every refusal holds ($checked cases checked)"
fi
exit "$fail"
