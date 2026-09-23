package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/truvity/ci-cache/engine/tier"
)

// meter counts what one tier in the chain did.
//
// The counts are per tier rather than per chain because the only number a
// build log reader actually acts on is where the hits came from: a job whose
// hits are all local was warm, a job whose hits are all remote paid the
// network for every one of them, and a job with neither has a cache that is
// not working. A single "hits" number cannot tell those three apart.
type meter struct {
	t tier.Tier

	gets, hits, misses, errs atomic.Int64
	puts, putErrs            atomic.Int64
	bytesRead, bytesWritten  atomic.Int64
}

func newMeter(t tier.Tier) *meter { return &meter{t: t} }

// Name implements tier.Tier.
func (m *meter) Name() string { return m.t.Name() }

// Get implements tier.Tier.
func (m *meter) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	m.gets.Add(1)
	rc, meta, err := m.t.Get(ctx, key)
	switch {
	case err == nil:
		m.hits.Add(1)
		if meta.Size > 0 {
			m.bytesRead.Add(meta.Size)
		}
	case errors.Is(err, tier.ErrNotFound):
		m.misses.Add(1)
	default:
		m.errs.Add(1)
	}
	return rc, meta, err
}

// Put implements tier.Tier.
func (m *meter) Put(ctx context.Context, key string, r io.Reader, meta tier.Meta) error {
	m.puts.Add(1)
	err := m.t.Put(ctx, key, r, meta)
	switch {
	case err == nil:
		if meta.Size > 0 {
			m.bytesWritten.Add(meta.Size)
		}
	case errors.Is(err, tier.ErrExists):
		// An immutable key that is already there is a write that did not
		// need to happen, not a write that failed.
	default:
		m.putErrs.Add(1)
	}
	return err
}

// Stat implements tier.Tier.
func (m *meter) Stat(ctx context.Context, key string) (tier.Meta, error) {
	return m.t.Stat(ctx, key)
}

// Delete implements tier.Tier.
func (m *meter) Delete(ctx context.Context, key string) error {
	return m.t.Delete(ctx, key)
}

// snapshot reads the counters.
func (m *meter) snapshot() TierStats {
	return TierStats{
		Tier:         m.t.Name(),
		Gets:         m.gets.Load(),
		Hits:         m.hits.Load(),
		Misses:       m.misses.Load(),
		Errors:       m.errs.Load(),
		Puts:         m.puts.Load(),
		PutErrors:    m.putErrs.Load(),
		BytesRead:    m.bytesRead.Load(),
		BytesWritten: m.bytesWritten.Load(),
	}
}
