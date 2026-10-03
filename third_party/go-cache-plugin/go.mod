// A module of its own, so upstream's dependency graph resolves the way
// upstream tested it.
//
// Folded into the root go.mod this does not build. The root already pulls
// creachadair/gocache, which requires a newer creachadair/mds than the
// pinned go-cache-plugin was compiled against; one module graph means one
// MVS run, the higher version wins, and three APIs move under code that
// cannot ask for the older one (Queue.Update became SetUpdate,
// atomicfile.Tx and cache.LRU changed signatures). Isolation is the fix,
// and it is the whole reason this directory is a module.
//
// So: `github.com/creachadair/mds` below is LOAD-BEARING at upstream's
// version. Moving it forward here reproduces exactly the breakage this
// module exists to avoid. When it needs to move, the way is to bump
// `github.com/tailscale/go-cache-plugin` -- which brings a dependency set
// upstream has actually built against -- and re-vendor cmd/ to match
// (hack/vendored-upstream-is-pristine.sh enforces the second half).
// Renovate is told to leave the whole creachadair family here alone for
// that reason -- not just mds. Ignoring mds alone was tried first and
// was not enough (2026-09-29): a "non-major dependencies" bump moved
// atomicfile, command, gocache and mhttp forward, and EACH of those
// carries its own require on a newer mds, so MVS picked v0.31.0 with
// the mds line itself untouched. That broke `just check`'s vuln recipe
// on master until reverted. The whole family is one upstream-tested
// set; see renovate.json.
//
// Three dependencies ARE ahead of upstream's pins, deliberately, because
// govulncheck found live findings at upstream's own versions:
// golang.org/x/mod (GO-2026-6179, GO-2026-6180) and
// aws-sdk-go-v2's eventstream and s3 (GO-2026-5764). Safe to move only
// because this module is isolated -- none of it reaches the graph that
// made isolation necessary. Consequence worth knowing: the binary this
// repository releases is therefore NOT built from an identical dependency
// set to the one ci-plane's runner image builds with `go install`, even at
// the same upstream version. If a measurement ever turns on a difference
// between those two binaries, that is where to look first.
module github.com/truvity/ci-cache/third_party/go-cache-plugin

go 1.27.1

tool github.com/tailscale/go-cache-plugin/cmd/go-cache-plugin

require (
	github.com/aws/aws-sdk-go-v2 v1.46.0
	github.com/aws/aws-sdk-go-v2/config v1.33.3
	github.com/aws/aws-sdk-go-v2/service/s3 v1.112.0
	github.com/creachadair/atomicfile v0.3.7
	github.com/creachadair/command v0.1.20
	github.com/creachadair/flax v0.0.4
	github.com/creachadair/gocache v0.0.0-20250108235800-51cd8478f1c9
	github.com/creachadair/mhttp v0.0.0-20241114003125-97da0a4f17b1
	github.com/creachadair/taskgroup v0.14.4
	github.com/creachadair/tlsutil v0.0.0-20241111194928-a9f540254538
	github.com/goproxy/goproxy v0.18.0
	github.com/tailscale/go-cache-plugin v0.1.1
	golang.org/x/sys v0.48.0
	tailscale.com v1.104.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.3 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.19.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.9.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.37.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.42.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.49.0 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/creachadair/mds v0.31.0 // indirect
	github.com/creachadair/msync v0.10.1 // indirect
	github.com/creachadair/scheddle v0.0.0-20241121045015-b2e30c9594a1 // indirect
	github.com/go-json-experiment/json v0.0.0-20260820222146-c27c302e5fc3 // indirect
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745 // indirect
	go4.org/netipx v0.0.0-20260823151212-3075585bcbeb // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
)
