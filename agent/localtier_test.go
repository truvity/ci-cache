package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/truvity/ci-cache/engine/tier"
)

type capturingTier struct {
	tier.Tier
	puts []string
	read int64
}

func (c *capturingTier) Name() string { return "capturing" }

func (c *capturingTier) Put(_ context.Context, key string, body io.Reader, _ tier.Meta) error {
	c.puts = append(c.puts, key)

	n, _ := io.Copy(io.Discard, body)
	c.read += n

	return nil
}

// Output bodies must not reach the disk tier, and everything else must.
//
// The negative half is the point of the type; the positive half is what
// stops a careless prefix change from silently turning the local tier off
// altogether, which would look like a hit-rate collapse and nothing else.
func TestRecordsOnlyStoresRecordsAndNotOutputs(t *testing.T) {
	inner := &capturingTier{}
	filtered := recordsOnly{inner}

	cases := []struct {
		key    string
		stored bool
		why    string
	}{
		{"go/build/output/ab/abcdef", false, "an output body is already materialised for the compiler"},
		{"go/build/action/ab/abcdef", true, "the record is what a lookup resolves first"},
		{"go/mod/cache/download/x", true, "another front-end's keys are none of this type's business"},
		{"gocache/output/ab/abcdef", true, "the legacy layout is not the prefix this matches"},
		{"go/build/outputs/ab/abcdef", true, "a near-miss on the prefix must not be swallowed"},
	}

	for _, c := range cases {
		body := strings.NewReader("some bytes")
		if err := filtered.Put(context.Background(), c.key, body, tier.Meta{Size: 10}); err != nil {
			t.Fatalf("Put(%s) = %v, want nil", c.key, err)
		}

		var got bool

		for _, k := range inner.puts {
			if k == c.key {
				got = true
			}
		}

		if got != c.stored {
			t.Errorf("Put(%s) stored=%v, want %v: %s", c.key, got, c.stored, c.why)
		}
	}
}

// A declined write must not read the body either. Reading bytes to discard
// them is most of the cost this removes -- a filter that drained would move
// the write off the disk and leave the read behind.
func TestADeclinedPutDoesNotReadTheBody(t *testing.T) {
	inner := &capturingTier{}
	filtered := recordsOnly{inner}

	body := strings.NewReader(strings.Repeat("x", 1<<20))

	if err := filtered.Put(context.Background(), "go/build/output/ab/abcdef", body, tier.Meta{Size: 1 << 20}); err != nil {
		t.Fatalf("Put = %v", err)
	}

	if body.Len() != 1<<20 {
		t.Errorf("%d bytes were consumed from a declined put; it should read none", (1<<20)-body.Len())
	}

	if inner.read != 0 {
		t.Errorf("the inner tier read %d bytes for a declined put", inner.read)
	}
}
