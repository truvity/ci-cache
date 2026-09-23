package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"github.com/truvity/ci-cache/engine/tier"
)

// Scenario is which part of the chain the load is meant to exercise.
//
// The three are not variations on one test. They ask different questions,
// and a change can move one without touching another: streaming the fault-in
// should transform ColdBucket and leave WarmDisk untouched, and a result where
// both moved together means the change did something other than what was
// intended.
type Scenario string

const (
	// WarmDisk means everything is on the server's disk tier. It measures the server
	// and the wire, with the object store out of the picture.
	WarmDisk Scenario = "warm-disk"

	// ColdBucket means nothing is on disk and everything is in the bucket. Every
	// request is a fault-in, which is the path that buffers today.
	ColdBucket Scenario = "cold-bucket"

	// Mixed applies a hit ratio, defaulting to the 90% measured on gitops. It is the
	// closest of the three to a real job, and the least useful for
	// attributing a change to a cause -- which is why it is not the only one.
	Mixed Scenario = "mixed"
)

// Store is the subset of a cache the bench needs. engine/remote satisfies
// it, so the bench exercises the same client the agent uses rather than a
// hand-rolled one that might be fast for reasons the agent is not.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error)
	Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error
}

// DiskWiper removes the server's disk tier, which is how ColdBucket and
// Mixed are set up. The admin API provides it; a bench against a server
// whose admin port is not reachable can still run WarmDisk.
type DiskWiper interface {
	WipeDisk(ctx context.Context, prefix string) error
}

// Config is one point on the curve.
type Config struct {
	Scenario     Scenario
	Concurrency  int
	Duration     time.Duration
	Corpus       *Corpus
	HitRatio     float64 // Mixed only
	Compressible float64
	Seed         int64

	// Populate fills the server before the run. Off when the corpus is
	// already there from a previous point in a sweep -- populating 2.4GB
	// before every point would make a sweep take longer than it measures.
	Populate bool
}

// Run populates as the scenario requires, loads the server, and reports.
func Run(ctx context.Context, cfg Config, store Store, wiper DiskWiper) (Report, error) {
	if cfg.Concurrency <= 0 {
		return Report{}, errors.New("bench: concurrency must be positive")
	}

	if cfg.Corpus == nil || len(cfg.Corpus.Lookups) == 0 {
		return Report{}, errors.New("bench: empty corpus")
	}

	var notes []string

	if cfg.Populate {
		if err := populate(ctx, cfg, store); err != nil {
			return Report{}, fmt.Errorf("populate: %w", err)
		}
	}

	if note, err := prepare(ctx, cfg, store, wiper); err != nil {
		return Report{}, fmt.Errorf("prepare %s: %w", cfg.Scenario, err)
	} else if note != "" {
		notes = append(notes, note)
	}

	samples, elapsed := load(ctx, cfg, store)

	// A run that made no requests is not a fast run, it is a broken one --
	// and it would report 0 ms at every percentile, which reads like the
	// best result the bench has ever produced.
	if len(samples) == 0 {
		return Report{}, errors.New("bench: no requests completed; the run measured nothing")
	}

	r := Summarise(string(cfg.Scenario), cfg.Concurrency, elapsed, samples)
	r.Notes = notes

	// Every request failing is the same trap one level down: the percentiles
	// are computed over an empty set and the line looks clean.
	if r.Errors == r.Requests {
		return r, fmt.Errorf("bench: all %d requests failed", r.Requests)
	}

	return r, nil
}

// populate writes the whole corpus.
func populate(ctx context.Context, cfg Config, store Store) error {
	sem := make(chan struct{}, cfg.Concurrency)

	var (
		wg   sync.WaitGroup
		once sync.Once
		bad  error
	)

	for _, o := range cfg.Corpus.Objects {
		wg.Add(1)
		sem <- struct{}{}

		go func(o Object) {
			defer wg.Done()
			defer func() { <-sem }()

			body := o.Body(cfg.Compressible)

			err := store.Put(ctx, o.Key, bytes.NewReader(body), tier.Meta{
				Size:    int64(len(body)),
				ModTime: time.Now(),
			})
			if err != nil && !errors.Is(err, tier.ErrExists) {
				once.Do(func() { bad = fmt.Errorf("put %s: %w", o.Key, err) })
			}
		}(o)
	}

	wg.Wait()

	return bad
}

// prepare puts the server's tiers into the state the scenario describes.
func prepare(ctx context.Context, cfg Config, store Store, wiper DiskWiper) (string, error) {
	if cfg.Scenario == WarmDisk {
		return "", nil
	}

	if wiper == nil {
		return "", errors.New("needs the admin API to wipe the disk tier, and none was given")
	}

	// Wiping the disk leaves the bucket, which is exactly ColdBucket. Mixed
	// then re-reads a fraction back by asking for it, which is honest: the
	// server faults those in the way it would in life, rather than being
	// placed into a state by hand that it could not reach on its own.
	// The corpus's own root, and its trailing slash is load-bearing: the
	// admin API reads a prefix as a prefix only when it ends in "/", and
	// anything else as one exact key. Wiping "go/build" would match nothing
	// and cold-bucket would silently be warm-disk wearing another name.
	if err := wiper.WipeDisk(ctx, cfg.Corpus.Root); err != nil {
		return "", fmt.Errorf("wipe disk: %w", err)
	}

	if cfg.Scenario == ColdBucket {
		return "", nil
	}

	// Mixed then faults a share back in by READING it, rather than by
	// placing the server in a state by hand. The difference matters: a
	// hand-placed disk tier could hold entries the server would never have
	// written, and the bench would be measuring a cache that cannot exist.
	warm := int(float64(len(cfg.Corpus.Lookups)) * cfg.HitRatio)
	if warm > 0 {
		if err := warmUp(ctx, cfg, store, warm); err != nil {
			return "", fmt.Errorf("warm %d lookups: %w", warm, err)
		}
	}

	return fmt.Sprintf("warmed %d of %d lookups (%.0f%%) by reading them",
		warm, len(cfg.Corpus.Lookups), cfg.HitRatio*100), nil
}

// warmUp reads the first n lookups so the server faults them onto its disk.
//
// The first n rather than a random n: the load phase picks at random from
// the whole corpus, so a deterministic warm set keeps the achieved hit ratio
// reproducible between runs instead of varying with the seed twice over.
func warmUp(ctx context.Context, cfg Config, store Store, n int) error {
	sem := make(chan struct{}, cfg.Concurrency)

	var (
		wg   sync.WaitGroup
		once sync.Once
		bad  error
	)

	for _, pair := range cfg.Corpus.Lookups[:n] {
		wg.Add(1)
		sem <- struct{}{}

		go func(pair [2]int) {
			defer wg.Done()
			defer func() { <-sem }()

			for _, idx := range pair {
				if s := fetch(ctx, store, cfg.Corpus.Objects[idx]); s.Err != nil {
					once.Do(func() { bad = s.Err })
				}
			}
		}(pair)
	}

	wg.Wait()

	return bad
}

// load runs the workers for the configured duration.
func load(ctx context.Context, cfg Config, store Store) ([]Sample, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	var (
		mu      sync.Mutex
		samples []Sample
		wg      sync.WaitGroup
	)

	start := time.Now()

	for w := range cfg.Concurrency {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			// Each worker has its own stream, offset by its index, so the
			// workers do not march through the corpus in lockstep asking for
			// the same key at the same moment -- which would measure the
			// singleflight group rather than the cache.
			rng := rand.New(rand.NewSource(cfg.Seed + int64(w)*7919)) //nolint:gosec // see NewCorpus
			local := make([]Sample, 0, 1024)

			// Deferred, and that is not tidiness. Every worker leaves this
			// loop with the deadline already expired, so a hand-written
			// hand-off at the bottom is skipped by whichever path actually
			// ends the loop -- which discarded every sample in the run and
			// reported "measured nothing" for a run that measured plenty.
			defer func() {
				mu.Lock()
				samples = append(samples, local...)
				mu.Unlock()
			}()

			for ctx.Err() == nil {
				pair := cfg.Corpus.Lookups[rng.Intn(len(cfg.Corpus.Lookups))]

				// Both halves of the lookup, in order, because that is what
				// a build does: the record tells it which output to ask for.
				for _, idx := range pair {
					s := fetch(ctx, store, cfg.Corpus.Objects[idx])

					// A request the deadline cancelled is not a failure of
					// the server and must not be counted as one -- but the
					// samples already taken are real and are kept.
					if ctx.Err() != nil && s.Err != nil {
						return
					}

					local = append(local, s)
				}
			}
		}(w)
	}

	wg.Wait()

	return samples, time.Since(start)
}

// fetch times one Get, separating the wait for the first byte from the
// transfer.
func fetch(ctx context.Context, store Store, o Object) Sample {
	start := time.Now()

	rc, _, err := store.Get(ctx, o.Key)
	if err != nil {
		return Sample{Err: err, FirstByte: time.Since(start), Complete: time.Since(start)}
	}

	first := time.Since(start)

	n, err := io.Copy(io.Discard, rc)
	_ = rc.Close()

	return Sample{
		FirstByte: first,
		Complete:  time.Since(start),
		Bytes:     n,
		Err:       err,
	}
}
