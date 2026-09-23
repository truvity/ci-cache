package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowQueue is an upload queue with a sink that takes its time: one entry
// every perEntry, and it stops where it got to when the context ends. That is
// the shape of the real thing under a drain -- an object store that is
// answering, just not fast enough -- and the only shape worth testing.
type slowQueue struct {
	mu       sync.Mutex
	depth    int
	perEntry time.Duration
}

func (q *slowQueue) Stop(ctx context.Context) error {
	for {
		q.mu.Lock()
		if q.depth == 0 {
			q.mu.Unlock()
			return nil
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(q.perEntry):
			q.mu.Lock()
			q.depth--
			q.mu.Unlock()
		}
	}
}

func (q *slowQueue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// capture collects structured records so a test can ask what was logged at
// what level, which is the whole assertion for a drain: the exit code says
// nothing and the count is the only thing anybody can act on.
type capture struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capture) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (c *capture) warnings(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range c.records(t) {
		if r["level"] == "WARN" {
			out = append(out, r)
		}
	}
	return out
}

func TestDrainFinishesWithinTheTimeout(t *testing.T) {
	q := &slowQueue{depth: 200, perEntry: time.Millisecond}
	sink := &capture{}
	log := slog.New(slog.NewJSONHandler(sink, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	drainQueue(ctx, log, q)

	if got := q.Depth(); got != 0 {
		t.Errorf("queue depth after the drain = %d, want 0", got)
	}
	if warns := sink.warnings(t); len(warns) != 0 {
		t.Errorf("a drain that finished logged %d warnings: %v", len(warns), warns)
	}
}

// TestDrainOverrunsTheTimeout is the case that decides the exit code.
//
// The objects still queued are on the disk of a pod that is going away, so
// they are lost whichever way this goes; exiting non-zero would turn an
// ordinary rollout into a CrashLoopBackOff and lose the rest of the cache
// with them. What is owed is a number.
func TestDrainOverrunsTheTimeout(t *testing.T) {
	q := &slowQueue{depth: 200, perEntry: 10 * time.Millisecond}
	sink := &capture{}
	log := slog.New(slog.NewJSONHandler(sink, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	drainQueue(ctx, log, q)

	left := q.Depth()
	if left == 0 {
		t.Fatal("the slow queue drained completely; the test is not testing an overrun")
	}

	warns := sink.warnings(t)
	if len(warns) != 1 {
		t.Fatalf("logged %d warnings, want exactly 1: %v", len(warns), warns)
	}
	remaining, ok := warns[0]["remaining"].(float64)
	if !ok {
		t.Fatalf("the warning does not carry a remaining count: %v", warns[0])
	}
	if int(remaining) != left {
		t.Errorf("warning says %d remaining, queue says %d", int(remaining), left)
	}
}
