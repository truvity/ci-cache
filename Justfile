# Development commands for ci-cache. Tools come from devbox (`devbox shell`,
# or direnv); CI runs each recipe as its own job, so a red mark says which.

export GOWORK := "off"

# Format all Go files
fmt:
    golangci-lint fmt ./...

# Regenerate the Connect code from the protos. Every plugin is local
# (installed by devbox), so this needs nothing but the checkout.
generate:
    buf generate

# Generated code is committed, for `go get`-ability. A proto change that is
# not followed by `just generate` leaves the tree dirty here.
drift: generate
    git diff --exit-code -- gen

# Lint the protos, and check that nothing released has changed incompatibly.
proto:
    buf lint
    # A first release has nothing to be incompatible with, and saying so beats
    # a recipe that always passes because its comparison silently failed.
    if tag=$(git describe --tags --abbrev=0 2>/dev/null); then \
        buf breaking --against ".git#tag=$tag"; \
    else \
        echo "no release tag yet: nothing to compare against"; \
    fi

# Build everything, including the main package CI never links.
build: fmt
    go build ./...

# Unit tests. Nothing here needs a cluster, a bucket or a network: the
# real-store checks live behind environment variables (see internal/storetest)
# and skip when it is not set.
test:
    go test ./... -coverprofile=coverage.out

# The suite under the race detector. A cache hands objects between a request
# goroutine and a background writer, so a race here is a corrupted object
# rather than a crash -- it would not show up in an ordinary run.
#
# Not part of `check`: everything else builds with cgo off, which is what
# makes the binaries static and the image small, and the race detector is the
# one thing that needs a C toolchain. CI runs it as its own job.
race:
    CGO_ENABLED=1 go test -race ./...

# Run linters
lint:
    golangci-lint config verify
    golangci-lint run ./...
    goreleaser check
    # Nothing built is committed. A binary in a public repository's history is
    # in every clone forever, and carries the build machine's paths.
    ! git ls-files -s | awk '{print $2" "$4}' | git cat-file --batch-check='%(objectsize) %(rest)' | awk '$1 > 1048576 {print "too large to be source:", $2; found=1} END {exit !found}'

# Hold the chart to what the binary will accept.
#
# The golden renders are committed, so a template change that alters a
# manifest is a diff a reviewer reads rather than a surprise in a cluster. The
# refusals matter more: each is a configuration the binary rejects at start-up,
# or accepts and then gets quietly wrong, and a chart that renders one anyway
# moves the failure somewhere nobody is looking.
chart:
    helm lint charts/ci-cache -f charts/ci-cache/testdata/values/r2.yaml
    bash charts/ci-cache/testdata/refuse.sh
    helm template ci-cache charts/ci-cache -f charts/ci-cache/testdata/values/aws.yaml \
        > charts/ci-cache/testdata/golden/aws.yaml
    helm template ci-cache charts/ci-cache -f charts/ci-cache/testdata/values/r2.yaml \
        > charts/ci-cache/testdata/golden/r2.yaml
    helm template ci-cache charts/ci-cache -f charts/ci-cache/testdata/values/go-only.yaml \
        > charts/ci-cache/testdata/golden/go-only.yaml
    helm template ci-cache charts/ci-cache -f charts/ci-cache/testdata/values/everything.yaml \
        > charts/ci-cache/testdata/golden/everything.yaml
    # The two estates' real shapes, rendered from the very files the deploy
    # pages show, so a documented example that no longer renders is a red mark
    # rather than somebody's afternoon.
    helm template ci-cache charts/ci-cache -f charts/ci-cache/examples/aws-s3.yaml > /dev/null
    helm template ci-cache charts/ci-cache -f charts/ci-cache/examples/cloudflare-r2.yaml > /dev/null
    git diff --exit-code -- charts/ci-cache/testdata/golden

# The docs are the contract: a behaviour not written down is not a behaviour.
# This checks the two claims a reader depends on -- every front-end the server
# mounts has a client page, and every chart value appears in a deploy page.
docs:
    bash hack/check-docs.sh

# Load a RUNNING server and report what it did.
#
# Deliberately NOT part of `check`: it needs a server, a bucket and minutes,
# and a gate that cannot run on a laptop with no cluster is a gate people
# learn to skip. This is the instrument for the performance work, and its
# output belongs in docs/bench/ beside the change that moved it.
#
# The sweep is the useful form -- one point tells you a number, a curve tells
# you where the limit is:
#
#   just bench-sweep http://localhost:8080/go/build http://localhost:8081
bench *args:
    go run ./cmd/ci-cache-bench {{args}}

# Every scenario at every concurrency, appended to one file.
bench-sweep server admin report="bench.jsonl":
    #!/usr/bin/env bash
    set -euo pipefail
    for scenario in warm-disk cold-bucket mixed; do
        go run ./cmd/ci-cache-bench \
            --server "{{server}}" --admin "{{admin}}" \
            --scenario "$scenario" \
            --concurrency 1,8,64,256,1024 \
            --report "{{report}}"
    done
    echo "wrote {{report}}"

# Known vulnerabilities in what we import and call.
#
# third_party/go-cache-plugin is a SEPARATE module (see .goreleaser.yaml for
# why), so govulncheck's own module-boundary rule means `./...` from here
# does not reach it -- scanning it needs its own invocation, in its own
# directory, or a redistributed binary's dependency tree goes unchecked
# just because it isn't imported by anything of ours.
vuln:
    govulncheck ./...
    cd third_party/go-cache-plugin && govulncheck ./...

# This repository is public; particulars are caller inputs, never defaults.
leak-canary:
    bash hack/leak-canary.sh

# Execute the setup action's step bodies against a stubbed environment.
#
# A composite action is shell in a YAML envelope and actionlint does not read
# it, so the only way to know what a step does is to run it. The cases assert
# what each shape SETS, that a runner with no backend fails open rather than
# failing, and that the steps which merely inspect the repository's own files
# never edit them.
setup-action:
    bash hack/setup-cases.sh

# Build everything a release would, locally and unpublished.
snapshot:
    goreleaser release --snapshot --clean

check: build test lint proto drift chart docs vuln leak-canary setup-action
