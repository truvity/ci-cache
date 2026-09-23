// Package tiertest provides a tier implementation for tests.
//
// It lives in its own package rather than in each test file because more than
// one package needs a tier that is not a disk: the server's round-trip tests,
// the chain's composition tests and any front-end that wants to assert what it
// stored. One implementation means one set of semantics, so a test that passes
// against Memory and fails against the disk is telling us about the disk.
package tiertest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/truvity/ci-cache/engine/tier"
)

// Memory is a tier that keeps objects in a map.
//
// It is deliberately simple, but not simplified: it enforces the immutability
// rule and the not-found error exactly as a real tier does, because those two
// are what every caller's error handling is built on.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject

	// Label is what Name reports. A test that wires two Memory tiers into a
	// chain needs to tell them apart in a failure message.
	Label string

	// Hooks let a test stall or fail one operation without a second tier
	// implementation. They are called before anything is stored or read, so a
	// hook that returns an error leaves the tier untouched.
	BeforeGet func(ctx context.Context, key string) error
	BeforePut func(ctx context.Context, key string) error
}

type memObject struct {
	body []byte
	meta tier.Meta
}

// NewMemory returns an empty in-memory tier.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memObject{}}
}

// Name implements tier.Tier.
func (m *Memory) Name() string {
	if m.Label != "" {
		return m.Label
	}
	return "memory"
}

// Get implements tier.Tier.
func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	if m.BeforeGet != nil {
		if err := m.BeforeGet(ctx, key); err != nil {
			return nil, tier.Meta{}, err
		}
	}
	m.mu.RLock()
	o, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, tier.Meta{}, fmt.Errorf("%w: %s", tier.ErrNotFound, key)
	}
	// The stored slice is never handed out: a caller that reads an object and
	// then writes through the same buffer would otherwise mutate what the tier
	// still believes it is holding.
	body := make([]byte, len(o.body))
	copy(body, o.body)
	return io.NopCloser(bytes.NewReader(body)), o.meta, nil
}

// Put implements tier.Tier.
func (m *Memory) Put(ctx context.Context, key string, r io.Reader, meta tier.Meta) error {
	if m.BeforePut != nil {
		if err := m.BeforePut(ctx, key); err != nil {
			return err
		}
	}

	// The existence check happens before the body is read, which is what the
	// wire protocol promises: a client racing to write an object it already
	// lost the race for must not pay to upload it.
	m.mu.RLock()
	existing, ok := m.objects[key]
	m.mu.RUnlock()
	if ok && existing.meta.Immutable {
		return fmt.Errorf("%w: %s", tier.ErrExists, key)
	}

	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("tiertest: read body for %s: %w", key, err)
	}
	meta.Size = int64(len(body))

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-checked under the write lock: two concurrent puts of the same
	// immutable key must not both succeed, or the second would overwrite bytes
	// a reader has already been told are final.
	if existing, ok := m.objects[key]; ok && existing.meta.Immutable {
		return fmt.Errorf("%w: %s", tier.ErrExists, key)
	}
	m.objects[key] = memObject{body: body, meta: meta}
	return nil
}

// Stat implements tier.Tier.
func (m *Memory) Stat(_ context.Context, key string) (tier.Meta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return tier.Meta{}, fmt.Errorf("%w: %s", tier.ErrNotFound, key)
	}
	return o.meta, nil
}

// Delete implements tier.Tier. Deleting an absent key succeeds, as tier.Tier
// requires: a wipe that runs twice must not fail the second time.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Len reports how many objects the tier holds, for assertions about what a
// front-end actually wrote.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

// Keys returns the keys the tier holds, in no particular order.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}

// Bytes returns a copy of one object's body, or nil when it is absent.
func (m *Memory) Bytes(key string) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return nil
	}
	body := make([]byte, len(o.body))
	copy(body, o.body)
	return body
}

// Seed stores an object without going through Put, so that a test can arrange
// a state -- an immutable object that is already present, say -- without first
// having to satisfy the rules that state would normally be reached through.
func (m *Memory) Seed(key string, body []byte, meta tier.Meta) {
	stored := make([]byte, len(body))
	copy(stored, body)
	meta.Size = int64(len(stored))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{body: stored, meta: meta}
}

// Memory is a Tier; the compiler says so here rather than at every use.
var _ tier.Tier = (*Memory)(nil)
