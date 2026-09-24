package agent

import (
	"context"
	"io"
	"strings"

	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/frontend/gobuild"
)

// recordsOnly is the agent's local disk tier with output bodies kept out of
// it.
//
// The agent has two local stores, and only one of them is load-bearing. Every
// output it serves is materialised into a directory the compiler opens by
// path; that file is the object, as far as the build is concerned. The disk
// tier held a second, complete copy of the same bytes.
//
// Nothing read it. A repeat lookup is answered from the materialised file --
// the agent resolves the 84-byte action record and finds the file already
// there -- so the tier's copy of an output was written once and read never.
// Measured on one truvity/gitops build: 3501 remote hits at around 1.2MB
// each, so roughly 4.3GB written to disk and then ignored, plus the page
// cache that came with it, charged to the container's memory limit.
//
// tailscale/go-cache-plugin has one local store for exactly this reason: its
// local cache IS the directory it hands to the compiler.
//
// Action records are still stored here, and that is the whole point of
// keeping the tier at all. They are 84 bytes, they are what a lookup resolves
// first, and having them locally is what lets a repeat lookup cost a stat
// instead of a round trip.
type recordsOnly struct{ tier.Tier }

// Put stores everything except an output body.
//
// Declining is reported as success, not as an error, because it is not a
// failure: the object is on disk under the name the compiler will open. The
// reader is deliberately left unread -- every caller in the chain closes the
// source it opened, and reading bytes in order to discard them is the cost
// this type exists to remove.
func (r recordsOnly) Put(ctx context.Context, key string, body io.Reader, m tier.Meta) error {
	if isOutputKey(key) {
		return nil
	}

	return r.Tier.Put(ctx, key, body, m)
}

// isOutputKey reports whether key names an output body rather than an action
// record.
//
// Matched on the full prefix rather than on "/output/" anywhere in the key:
// a substring test would also silence a front-end that happens to use the
// same word, and silently not storing another cache's objects is the kind of
// bug that shows up as a mysterious hit-rate collapse months later.
func isOutputKey(key string) bool {
	return strings.HasPrefix(key, gobuild.Prefix+"/output/")
}
