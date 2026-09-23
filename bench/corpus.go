// Package bench loads a running cache server and reports what it did.
//
// It exists because CI is the wrong instrument for a performance question. A
// job takes five minutes, conflates compile time with cache time, runs on
// whichever runner is free, and answers one point on one curve. The evening
// of 2026-09-23 spent an hour discovering, through CI runs, things this
// package answers in a minute: that the budget was not the constraint, that
// hit rates were fine, and that the cost was re-transfer.
//
// Everything here is shaped by one measurement from that evening, and the
// shape matters more than the numbers: a Go build cache lookup is TWO
// requests, a tiny action record and then a large output object, and they
// are bound by different things. The record is latency and the output is
// throughput. A benchmark that generates one uniform object size measures
// neither workload.
package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"strings"

	"github.com/truvity/ci-cache/frontend/gobuild"
)

// The corpus's shape, measured on truvity/gitops, 2026-09-23.
//
// From the agent's own summary on the `build` recipe: gets=2622, and 4.3GB
// read across 3435 remote hits. An action record is the two fields
// gobuild writes (an output id and a timestamp) and is 84 bytes on the
// wire; the outputs average about 1.2MB with a long tail.
const (
	// ActionRecordSize is exact rather than sampled: the record is a fixed
	// format, so every one of them is this big.
	ActionRecordSize = 84

	// outputMedian and outputSigma describe the log-normal the output sizes
	// are drawn from. A normal distribution would be wrong in the way that
	// matters: object files cluster small with a tail of large archives, and
	// the tail is where the bytes are.
	outputMedian = 300 << 10
	outputSigma  = 1.4

	// outputCap bounds the tail. Without it a seed can draw a
	// several-hundred-megabyte object and one request dominates a 60-second
	// run, which makes two runs incomparable for a reason that has nothing
	// to do with the server.
	outputCap = 64 << 20
)

// Object is one item in the corpus.
//
// Body is not stored: a corpus of 2000 objects averaging 1.2MB is 2.4GB, and
// holding that in the load generator would make the generator the thing
// being measured. The body is regenerated from Seed on demand, which is
// cheaper than the network it is about to go over.
type Object struct {
	Key  string
	Size int64
	Seed int64
	// Record is true for an action record: small, and the thing a lookup
	// asks for first.
	Record bool
}

// Corpus is a deterministic set of objects.
//
// Deterministic is the whole point. Two runs of the bench against two builds
// of the server must ask for the same bytes in the same order, or the
// difference between them is the corpus rather than the change.
type Corpus struct {
	// Root is the prefix every key sits under, and the thing a scenario
	// wipes. It is on the corpus rather than computed at the wipe site so
	// that the two cannot disagree -- a wipe of the wrong prefix either
	// destroys somebody's cache or silently wipes nothing.
	Root string

	Objects []Object
	// Lookups pairs each record with its output, in the order a build would
	// ask for them. A worker picks a pair, not an object, because asking for
	// outputs without their records measures a workload nobody runs.
	Lookups [][2]int
}

// NewCorpus builds a corpus of n lookups from seed.
//
// n is lookups, not objects: each produces one record and one output, so the
// object count is 2n and the request count per pass is 2n. That ratio is the
// measured one and is not configurable, because a bench whose request mix
// can be tuned away from the real one is a bench that can be made to say
// anything.
//
// keyPrefix segregates a run from whatever else the server holds. Empty is
// the faithful shape and what a dedicated bench server should use; a value
// is for pointing this at a cache real jobs depend on, where the corpus
// must be removable afterwards without touching their entries.
func NewCorpus(n int, seed int64, keyPrefix string) *Corpus {
	if n <= 0 {
		panic("bench: corpus size must be positive")
	}

	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // reproducibility is the requirement, not secrecy

	prefix := gobuild.Prefix
	if keyPrefix != "" {
		prefix += "/" + strings.Trim(keyPrefix, "/")
	}
	c := &Corpus{
		Root:    prefix + "/",
		Objects: make([]Object, 0, 2*n),
		Lookups: make([][2]int, 0, n),
	}

	for i := range n {
		actionID := idFor(seed, "action", i)
		outputID := idFor(seed, "output", i)

		ri := len(c.Objects)
		c.Objects = append(c.Objects, Object{
			Key:    prefix + "/action/" + actionID[:2] + "/" + actionID,
			Size:   ActionRecordSize,
			Seed:   seed + int64(i),
			Record: true,
		})

		oi := len(c.Objects)
		c.Objects = append(c.Objects, Object{
			Key:  prefix + "/output/" + outputID[:2] + "/" + outputID,
			Size: drawSize(rng),
			Seed: seed + int64(n) + int64(i),
		})

		c.Lookups = append(c.Lookups, [2]int{ri, oi})
	}

	return c
}

// Bytes is the corpus's total size, which is what a full pass transfers.
func (c *Corpus) Bytes() int64 {
	var n int64
	for _, o := range c.Objects {
		n += o.Size
	}

	return n
}

// drawSize samples one output size from the log-normal described above.
func drawSize(rng *rand.Rand) int64 {
	n := int64(math.Exp(rng.NormFloat64()*outputSigma) * outputMedian)
	if n < 1 {
		n = 1
	}

	if n > outputCap {
		n = outputCap
	}

	return n
}

// idFor derives a content-address-shaped id.
//
// The ids must look like the real ones because the key layout shards on the
// first two characters: a corpus whose ids all began with the same pair
// would put every object in one directory and measure that instead.
func idFor(seed int64, kind string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("ci-cache-bench/%d/%s/%d", seed, kind, i)))

	return hex.EncodeToString(sum[:])
}

// Body returns a reader over o's bytes.
//
// The content is pseudo-random from the object's seed, which matters for
// measuring compression: incompressible bodies would make a compressed build
// look identical to an uncompressed one, and zero-filled bodies would make it
// look infinitely better. Neither is the truth about object files, so
// `--compressible` exists to sweep between them rather than to pick one.
func (o Object) Body(compressible float64) []byte {
	b := make([]byte, o.Size)
	rng := rand.New(rand.NewSource(o.Seed)) //nolint:gosec // see NewCorpus

	// The body is a run-length mixture: a fraction of it is repeated bytes,
	// which a compressor finds, and the rest is noise, which it cannot. At
	// 0 the body is incompressible; at 1 it is a single repeated byte.
	if compressible < 0 {
		compressible = 0
	}

	if compressible > 1 {
		compressible = 1
	}

	_, _ = rng.Read(b)

	runs := int(float64(len(b)) * compressible)
	for i := range runs {
		b[i] = byte(i % 7)
	}

	return b
}
