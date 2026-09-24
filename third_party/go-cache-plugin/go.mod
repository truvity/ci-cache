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
// Renovate is told to leave mds alone here for that reason.
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

go 1.27.0

tool github.com/tailscale/go-cache-plugin/cmd/go-cache-plugin

require (
	github.com/aws/aws-sdk-go-v2 v1.41.5
	github.com/aws/aws-sdk-go-v2/config v1.29.5
	github.com/aws/aws-sdk-go-v2/service/s3 v1.97.3
	github.com/creachadair/atomicfile v0.3.7
	github.com/creachadair/command v0.1.20
	github.com/creachadair/flax v0.0.4
	github.com/creachadair/gocache v0.0.0-20250108235800-51cd8478f1c9
	github.com/creachadair/mhttp v0.0.0-20241114003125-97da0a4f17b1
	github.com/creachadair/taskgroup v0.13.2
	github.com/creachadair/tlsutil v0.0.0-20241111194928-a9f540254538
	github.com/goproxy/goproxy v0.18.0
	github.com/tailscale/go-cache-plugin v0.1.1
	golang.org/x/sys v0.32.0
	tailscale.com v1.82.5
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.8 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.17.58 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.27 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.21 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.2 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.22 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.21 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.24.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.28.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.33.13 // indirect
	github.com/aws/smithy-go v1.24.2 // indirect
	github.com/creachadair/mds v0.23.0 // indirect
	github.com/creachadair/msync v0.4.0 // indirect
	github.com/creachadair/scheddle v0.0.0-20241121045015-b2e30c9594a1 // indirect
	github.com/go-json-experiment/json v0.0.0-20250223041408-d3c622f1b874 // indirect
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745 // indirect
	go4.org/netipx v0.0.0-20231129151722-fdeea329fbba // indirect
	golang.org/x/crypto v0.35.0 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/net v0.36.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
)
