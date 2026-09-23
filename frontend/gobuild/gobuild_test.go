package gobuild

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
)

// upstreamRecord is a golden action record in go-cache-plugin's format: the
// output id and the modification time in Unix nanoseconds, one space between
// them and nothing else. It is written out here as a literal rather than
// produced by this package's own formatter, because a test that formats what
// it parses proves only that this package agrees with itself.
const (
	upstreamActionID = "1f0c6d1b2a3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394a5b6c7d8"
	upstreamOutputID = "9a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a9"
	upstreamRecord   = "9a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a9 1700000000123456789"
)

var upstreamModTime = time.Unix(1700000000, 123456789)

// TestGoldenUpstreamRecord reads a record exactly as the tool this service
// replaces would have left it in the bucket, at exactly the key that tool
// would have used. If either the layout or the record format drifts, an
// estate's warm bucket goes cold on the day of the switch, and this is the
// test that says so.
func TestGoldenUpstreamRecord(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	body := []byte("compiled output bytes")

	mustPutRaw(t, mem, "go/build/action/1f/"+upstreamActionID, []byte(upstreamRecord), tier.Meta{})
	mustPutRaw(t, mem, "go/build/output/9a/"+upstreamOutputID, body, tier.Meta{
		Size:      int64(len(body)),
		Immutable: true,
	})

	c := New(mem, config.GoBuild{})
	outputID, rc, m, ok, err := c.Get(ctx, upstreamActionID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v, want a hit", ok, err)
	}
	defer func() { _ = rc.Close() }()

	if outputID != upstreamOutputID {
		t.Errorf("outputID = %q, want %q", outputID, upstreamOutputID)
	}
	if !m.ModTime.Equal(upstreamModTime) {
		t.Errorf("ModTime = %v, want %v", m.ModTime, upstreamModTime)
	}
	if m.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", m.Size, len(body))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}

	// The other direction: what this package writes must be byte-identical to
	// what the old tool parses, because a rollback reads it.
	if formatted := formatAction(upstreamOutputID, upstreamModTime); formatted != upstreamRecord {
		t.Errorf("formatAction = %q, want %q", formatted, upstreamRecord)
	}
}

func TestParseActionShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"golden", upstreamRecord, true},
		{"trailing newline", upstreamRecord + "\n", true},
		{"extra whitespace", upstreamOutputID + "   1700000000123456789\n", true},
		{"negative nanoseconds", upstreamOutputID + " -1000000000", true},
		{"one field", upstreamOutputID, false},
		{"three fields", upstreamRecord + " 4096", false},
		{"unparsable time", upstreamOutputID + " yesterday", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAction([]byte(tc.in))
			if (err == nil) != tc.ok {
				t.Fatalf("parseAction(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

// TestUnparsableRecordIsAMiss covers the record a future version of something
// else might write: it must cost one build a miss, not fail it.
func TestUnparsableRecordIsAMiss(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	key := "go/build/action/1f/" + upstreamActionID
	mustPutRaw(t, mem, key, []byte(upstreamRecord+" 4096"), tier.Meta{})

	c := New(mem, config.GoBuild{})
	if _, _, _, ok, err := c.Get(ctx, upstreamActionID); ok || err != nil {
		t.Fatalf("Get: ok=%v err=%v, want a clean miss", ok, err)
	}
	if _, err := mem.Stat(ctx, key); !errors.Is(err, tier.ErrNotFound) {
		t.Errorf("record still present after an unparsable read: %v", err)
	}
}

// TestRoundTrip is the property that matters: whatever was put comes back, and
// an output that disappears underneath the cache is a miss whose record is
// cleaned up rather than left to cost every later reader two lookups.
func TestRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		mem := tiertest.NewMemory()
		c := New(mem, config.GoBuild{})

		actionID := rapid.StringMatching(`[0-9a-f]{2,64}`).Draw(rt, "actionID")
		outputID := rapid.StringMatching(`[0-9a-f]{2,64}`).Draw(rt, "outputID")
		body := rapid.SliceOfN(rapid.Byte(), 0, 512).Draw(rt, "body")
		ns := rapid.Int64Range(0, 1<<44).Draw(rt, "modTimeNanos")
		modTime := time.Unix(ns/1e9, ns%1e9)

		if err := c.Put(ctx, actionID, outputID, int64(len(body)), modTime, bytes.NewReader(body)); err != nil {
			rt.Fatalf("Put: %v", err)
		}

		gotID, rc, m, ok, err := c.Get(ctx, actionID)
		if err != nil || !ok {
			rt.Fatalf("Get after Put: ok=%v err=%v", ok, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			rt.Fatalf("read body: %v", err)
		}
		if gotID != outputID {
			rt.Errorf("outputID = %q, want %q", gotID, outputID)
		}
		if !bytes.Equal(got, body) {
			rt.Errorf("body = %q, want %q", got, body)
		}
		if !m.ModTime.Equal(modTime) {
			rt.Errorf("ModTime = %v, want %v", m.ModTime, modTime)
		}
		if m.Size != int64(len(body)) {
			rt.Errorf("Size = %d, want %d", m.Size, len(body))
		}

		statID, _, ok, err := c.Stat(ctx, actionID)
		if err != nil || !ok || statID != outputID {
			rt.Fatalf("Stat: id=%q ok=%v err=%v", statID, ok, err)
		}

		// Now take the bytes away without telling the cache, which is what a
		// lifecycle rule on the bucket or an eviction on the disk does.
		if err := mem.Delete(ctx, "go/build/output/"+outputID[:2]+"/"+outputID); err != nil {
			rt.Fatalf("delete output: %v", err)
		}

		if _, _, _, ok, err := c.Get(ctx, actionID); ok || err != nil {
			rt.Fatalf("Get with the output gone: ok=%v err=%v, want a clean miss", ok, err)
		}
		actionKey := "go/build/action/" + actionID[:2] + "/" + actionID
		if _, err := mem.Stat(ctx, actionKey); !errors.Is(err, tier.ErrNotFound) {
			rt.Errorf("dangling record %q survived the miss: %v", actionKey, err)
		}
	})
}

// TestPutWritesOutputBeforeAction pins the ordering that makes a crash
// survivable. A tier that fails every action write stands in for the crash:
// afterwards the output must be there on its own, never a record pointing at
// nothing.
func TestPutWritesOutputBeforeAction(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	mem.BeforePut = func(_ context.Context, key string) error {
		if strings.Contains(key, "/action/") {
			return errors.New("tier is on fire")
		}
		return nil
	}

	c := New(mem, config.GoBuild{})
	err := c.Put(ctx, upstreamActionID, upstreamOutputID, 3, upstreamModTime, strings.NewReader("abc"))
	if err == nil {
		t.Fatal("Put: want the action write to be reported")
	}
	if _, err := mem.Stat(ctx, "go/build/output/9a/"+upstreamOutputID); err != nil {
		t.Errorf("output was not written before the action record: %v", err)
	}
}

// TestLegacyPrefix is the migration: a bucket filled by go-cache-plugin under
// its own prefix keeps answering, and the read path leaves it exactly as it
// found it.
func TestLegacyPrefix(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	body := []byte("legacy output")
	mustPutRaw(t, mem, "old/gocache/action/1f/"+upstreamActionID, []byte(upstreamRecord), tier.Meta{})
	mustPutRaw(t, mem, "old/gocache/output/9a/"+upstreamOutputID, body, tier.Meta{Size: int64(len(body))})

	c := New(mem, config.GoBuild{LegacyPrefix: "old/gocache"})
	outputID, rc, _, ok, err := c.Get(ctx, upstreamActionID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v, want the legacy layout to answer", ok, err)
	}
	_ = rc.Close()
	if outputID != upstreamOutputID {
		t.Errorf("outputID = %q, want %q", outputID, upstreamOutputID)
	}

	// A read does not copy anything into the natural layout. Copying would
	// double every hit's write cost during a migration to save a lookup that
	// the next real Put makes anyway, and it would make a read path that can
	// fail on a full disk.
	for _, key := range []string{
		"go/build/action/1f/" + upstreamActionID,
		"go/build/output/9a/" + upstreamOutputID,
	} {
		if _, err := mem.Stat(ctx, key); !errors.Is(err, tier.ErrNotFound) {
			t.Errorf("the read path wrote %q; reads must not write", key)
		}
	}

	// Writes go to the natural layout only, so the legacy prefix stops
	// growing the moment this service is in front of it.
	if err := c.Put(ctx, upstreamActionID, upstreamOutputID, 3, upstreamModTime, strings.NewReader("new")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := mem.Stat(ctx, "go/build/action/1f/"+upstreamActionID); err != nil {
		t.Errorf("Put did not write the natural layout: %v", err)
	}
	if got := mem.Bytes("old/gocache/action/1f/" + upstreamActionID); !bytes.Equal(got, []byte(upstreamRecord)) {
		t.Errorf("Put touched the legacy layout: %q", got)
	}
}

// TestLegacyOutputAfterNaturalAction covers the half-migrated bucket: a record
// this service wrote, whose output only the old layout still has.
func TestLegacyOutputAfterNaturalAction(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	body := []byte("legacy output")
	mustPutRaw(t, mem, "go/build/action/1f/"+upstreamActionID, []byte(upstreamRecord), tier.Meta{})
	mustPutRaw(t, mem, "old/gocache/output/9a/"+upstreamOutputID, body, tier.Meta{Size: int64(len(body))})

	c := New(mem, config.GoBuild{LegacyPrefix: "old/gocache"})
	_, rc, _, ok, err := c.Get(ctx, upstreamActionID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v, want the legacy output to answer", ok, err)
	}
	_ = rc.Close()
}

func TestInvalidIDs(t *testing.T) {
	ctx := t.Context()
	c := New(tiertest.NewMemory(), config.GoBuild{})
	for _, id := range []string{"", "a", "../../etc/passwd", "ab/cd", "ab.cd", "ab cd"} {
		if _, _, _, _, err := c.Get(ctx, id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Get(%q) error = %v, want ErrInvalidID", id, err)
		}
		if err := c.Put(ctx, id, upstreamOutputID, 0, upstreamModTime, strings.NewReader("")); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Put(%q) error = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestZeroModTimeIsReplaced(t *testing.T) {
	ctx := t.Context()
	mem := tiertest.NewMemory()
	c := New(mem, config.GoBuild{})
	if err := c.Put(ctx, upstreamActionID, upstreamOutputID, 0, time.Time{}, strings.NewReader("")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, m, ok, err := c.Stat(ctx, upstreamActionID)
	if err != nil || !ok {
		t.Fatalf("Stat: ok=%v err=%v", ok, err)
	}
	// The zero Time is a large negative number in Unix nanoseconds, which the
	// toolchain would compare against a real file's timestamp.
	if m.ModTime.Year() < 2000 {
		t.Errorf("ModTime = %v, want a plausible timestamp", m.ModTime)
	}
}

func mustPutRaw(t *testing.T, mem *tiertest.Memory, key string, body []byte, m tier.Meta) {
	t.Helper()
	mem.Seed(key, body, m)
}
