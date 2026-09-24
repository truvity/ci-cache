// Package gobuild is the key mapping the Go build cache uses.
//
// It is not an HTTP front-end. The Go toolchain's cache protocol is spoken by
// the runner's agent and by the Connect service in server/; both of them need
// to agree, byte for byte, on where an action and its output live and on what
// an action record says. That agreement is this package, written once, so a
// runner built from one commit and a server built from another still find each
// other's objects.
//
// The layout and the record format are deliberately go-cache-plugin's, the
// tool this service replaces: a bucket that tool already filled keeps
// answering through the switch instead of starting cold, which for a Go
// monorepo is the difference between a first CI run that takes minutes and one
// that takes an hour.
package gobuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

// Prefix is where this front-end's keys live. Everything else in the bucket is
// another front-end's, and the garbage collector's per-front-end budgets are
// expressed as this prefix, so it is a constant rather than a literal spelled
// out at each call site.
const Prefix = "go/build"

// ErrInvalidID is returned for an action or output id that cannot be part of a
// key.
//
// It is an error and not a miss on purpose: a miss says "build it yourself and
// come back", which would quietly paper over a client that is sending
// nonsense, while an error says so once and is visible in the logs.
var ErrInvalidID = errors.New("gobuild: invalid cache id")

// Cache maps the Go build cache's two kinds of object onto a tier.
type Cache struct {
	tier tier.Tier

	// legacy, when set, is consulted on a miss. It is the key prefix a
	// go-cache-plugin bucket used, which is the same layout under a different
	// root; writes never go there, so the old layout stops growing the moment
	// this service is in front of it.
	legacy string

	log *slog.Logger
}

// Option configures a Cache.
type Option func(*Cache)

// WithLogger sets the logger used for the two things worth saying out loud: a
// record that pointed at bytes which are gone, and a read that only the legacy
// layout could answer. The second is how an estate learns when the legacy
// prefix has stopped earning its lookups and can be dropped.
func WithLogger(l *slog.Logger) Option {
	return func(c *Cache) {
		if l != nil {
			c.log = l
		}
	}
}

// New returns a Cache over the tier.
func New(t tier.Tier, cfg config.GoBuild, opts ...Option) *Cache {
	c := &Cache{
		tier:   t,
		legacy: strings.Trim(cfg.LegacyPrefix, "/"),
		log:    slog.Default().WithGroup("gobuild"),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name identifies the front-end in metrics and in the GC's budgets.
func (c *Cache) Name() string { return "go/build" }

// Get resolves an actionID to its output.
//
// A miss reports ok false with a nil error: the toolchain asks for far more
// actions than any cache holds, and a miss is the ordinary answer, not a
// failure. The only errors here are a malformed id and a tier that is broken.
func (c *Cache) Get(ctx context.Context, actionID string) (
	outputID string, body io.ReadCloser, m tier.Meta, ok bool, err error,
) {
	return c.GetIf(ctx, actionID, nil)
}

// GetIf is Get, except that once the action record has resolved it asks
// `need` whether the output bytes are actually wanted.
//
// It exists for one caller and one measured cost. The agent materialises
// every object it serves into a directory the compiler opens by path, and
// within a single build it is often asked again for something already
// sitting there. Get gives it no way to find that out without the body, so
// it was fetching the object -- over the network, in the remote case -- and
// throwing it away, because the record is the only thing that maps an action
// to an output id.
//
// A nil `need` means the body is always wanted, which is what Get is.
//
// When `need` declines, the returned Meta carries the record's modification
// time but no size: nothing was read, so there is no size to report, and a
// caller that declined the bytes is by construction one that already has
// them and can measure them itself.
func (c *Cache) GetIf(
	ctx context.Context, actionID string,
	need func(outputID string, modTime time.Time) bool,
) (outputID string, body io.ReadCloser, m tier.Meta, ok bool, err error) {
	rec, from, ok, err := c.action(ctx, actionID)
	if err != nil || !ok {
		return "", nil, tier.Meta{}, false, err
	}

	if need != nil && !need(rec.outputID, rec.modTime) {
		// Deliberately NOT verified against the tier. The caller is saying it
		// already holds these bytes; a Stat here to confirm the tier agrees
		// would put back a round trip on exactly the path this exists to
		// remove, and would answer a question the caller did not ask.
		return rec.outputID, nil, c.meta(tier.Meta{}, rec), true, nil
	}

	rc, om, ok, err := c.output(ctx, rec.outputID, from)
	if err != nil {
		return "", nil, tier.Meta{}, false, err
	}
	if !ok {
		// The record resolved and the bytes are gone. That is a miss and not
		// an error -- the toolchain will rebuild and re-put -- but the record
		// must go, because leaving it costs every later reader the same two
		// round trips to reach the same miss.
		c.dropDangling(ctx, actionID, rec.outputID, from)
		return "", nil, tier.Meta{}, false, nil
	}

	return rec.outputID, rc, c.meta(om, rec), true, nil
}

// Stat answers whether an action is present, without its bytes.
//
// It checks the output too, for the same reason Get does: an action record
// alone is not a hit, and a caller told "present" would then be handed
// nothing.
func (c *Cache) Stat(ctx context.Context, actionID string) (outputID string, m tier.Meta, ok bool, err error) {
	rec, from, ok, err := c.action(ctx, actionID)
	if err != nil || !ok {
		return "", tier.Meta{}, false, err
	}

	om, ok, err := c.statOutput(ctx, rec.outputID, from)
	if err != nil {
		return "", tier.Meta{}, false, err
	}
	if !ok {
		c.dropDangling(ctx, actionID, rec.outputID, from)
		return "", tier.Meta{}, false, nil
	}

	return rec.outputID, c.meta(om, rec), true, nil
}

// Put stores the output bytes and then the action record.
//
// The order is the whole point. A record names bytes; writing it first means a
// crash, a cancelled context or a full disk in between leaves a record
// pointing at nothing, which every later Get pays for. Written this way the
// worst a crash leaves behind is an output nobody references yet, which the
// next build of the same action adopts and which the garbage collector
// eventually reclaims.
func (c *Cache) Put(ctx context.Context, actionID, outputID string, size int64, modTime time.Time, r io.Reader) error {
	if err := validID(actionID); err != nil {
		return err
	}
	if err := validID(outputID); err != nil {
		return err
	}
	if modTime.IsZero() {
		// The record stores the modification time as Unix nanoseconds, and the
		// zero Time is a large negative number there rather than "unknown".
		// The toolchain compares that timestamp against the output on disk, so
		// a bogus one is worse than an approximate one.
		modTime = time.Now()
	}

	err := c.tier.Put(ctx, key(Prefix, "output", outputID), r, tier.Meta{
		Size:    size,
		ModTime: modTime,
		// The output is named by a hash of its own bytes, so a second writer
		// is writing the same thing. Immutable turns that race into ErrExists
		// instead of two writers overwriting one object.
		Immutable: true,
	})
	if err != nil && !errors.Is(err, tier.ErrExists) {
		return fmt.Errorf("gobuild: put output %s: %w", outputID, err)
	}

	rec := formatAction(outputID, modTime)
	err = c.tier.Put(ctx, key(Prefix, "action", actionID), strings.NewReader(rec), tier.Meta{
		Size:        int64(len(rec)),
		ModTime:     modTime,
		ContentType: "text/plain; charset=utf-8",
	})
	if err != nil && !errors.Is(err, tier.ErrExists) {
		return fmt.Errorf("gobuild: put action %s: %w", actionID, err)
	}
	return nil
}

// action reads and parses the action record, from the natural layout or, on a
// miss, from the legacy one. It reports which layout answered so that the
// output is looked for there first.
func (c *Cache) action(ctx context.Context, actionID string) (rec action, from string, ok bool, err error) {
	if err := validID(actionID); err != nil {
		return action{}, "", false, err
	}

	for _, prefix := range c.prefixes() {
		rc, _, err := c.tier.Get(ctx, key(prefix, "action", actionID))
		if errors.Is(err, tier.ErrNotFound) {
			continue
		}
		if err != nil {
			return action{}, "", false, fmt.Errorf("gobuild: read action %s: %w", actionID, err)
		}
		// An action record is two short fields; reading it whole is a
		// fixed-size cost and saves every caller a Close they would forget.
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return action{}, "", false, fmt.Errorf("gobuild: read action %s: %w", actionID, err)
		}
		a, err := parseAction(data)
		if err != nil {
			// A record this service cannot parse was written by something
			// else, or was truncated. Treating it as a miss is what keeps a
			// single bad object from failing every build that hashes to it.
			c.log.Warn("discarding unparsable action record", "action", actionID, "prefix", prefix, "error", err)
			c.deleteQuietly(ctx, key(prefix, "action", actionID))
			continue
		}
		if err := validID(a.outputID); err != nil {
			c.log.Warn("discarding action record with an unusable output id", "action", actionID, "output", a.outputID)
			c.deleteQuietly(ctx, key(prefix, "action", actionID))
			continue
		}
		if prefix != Prefix {
			c.log.Debug("served from the legacy layout", "action", actionID, "prefix", prefix)
		}
		return a, prefix, true, nil
	}
	return action{}, "", false, nil
}

// output reads the output bytes, preferring the layout the record came from:
// a legacy record was written beside legacy bytes, and looking there first
// saves a round trip in the case that is by definition the older one.
func (c *Cache) output(ctx context.Context, outputID, from string) (io.ReadCloser, tier.Meta, bool, error) {
	for _, prefix := range c.prefixesFrom(from) {
		rc, m, err := c.tier.Get(ctx, key(prefix, "output", outputID))
		if errors.Is(err, tier.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, tier.Meta{}, false, fmt.Errorf("gobuild: read output %s: %w", outputID, err)
		}
		return rc, m, true, nil
	}
	return nil, tier.Meta{}, false, nil
}

func (c *Cache) statOutput(ctx context.Context, outputID, from string) (tier.Meta, bool, error) {
	for _, prefix := range c.prefixesFrom(from) {
		m, err := c.tier.Stat(ctx, key(prefix, "output", outputID))
		if errors.Is(err, tier.ErrNotFound) {
			continue
		}
		if err != nil {
			return tier.Meta{}, false, fmt.Errorf("gobuild: stat output %s: %w", outputID, err)
		}
		return m, true, nil
	}
	return tier.Meta{}, false, nil
}

// meta is what the caller is told about the object.
//
// The size comes from the stored object because the record does not carry one,
// and the modification time comes from the record because that is the one the
// toolchain recorded when it produced the output; a tier is free to have
// written the object at any later moment.
func (c *Cache) meta(stored tier.Meta, rec action) tier.Meta {
	return tier.Meta{
		Size:      stored.Size,
		ModTime:   rec.modTime,
		Immutable: true,
	}
}

func (c *Cache) dropDangling(ctx context.Context, actionID, outputID, from string) {
	c.log.Warn("action record pointed at a missing output", "action", actionID, "output", outputID, "prefix", from)
	c.deleteQuietly(ctx, key(from, "action", actionID))
}

// deleteQuietly removes a key whose absence is the point. Failing to remove it
// costs a later reader one wasted lookup, which is not worth failing a build
// over.
func (c *Cache) deleteQuietly(ctx context.Context, k string) {
	if err := c.tier.Delete(ctx, k); err != nil && !errors.Is(err, tier.ErrNotFound) {
		c.log.Warn("failed to remove a stale key", "key", k, "error", err)
	}
}

// prefixes is the read order: the natural layout, then the legacy one if there
// is one.
func (c *Cache) prefixes() []string {
	if c.legacy == "" {
		return []string{Prefix}
	}
	return []string{Prefix, c.legacy}
}

func (c *Cache) prefixesFrom(first string) []string {
	all := c.prefixes()
	out := make([]string, 0, len(all))
	out = append(out, first)
	for _, p := range all {
		if p != first {
			out = append(out, p)
		}
	}
	return out
}

// key is the layout, and the only place it is spelled out.
//
// The two-character shard is the first two characters of the id. It buys
// nothing on an object store addressed by hash, but it is what the old tool
// wrote and what a human reading the bucket expects, and a cache whose keys
// changed would be an empty cache.
func key(prefix, kind, id string) string {
	return prefix + "/" + kind + "/" + id[:2] + "/" + id
}

// action is an action record: which output this action produced, and when that
// output was made.
type action struct {
	outputID string
	modTime  time.Time
}

// formatAction writes the record go-cache-plugin writes: the output id and the
// modification time in Unix nanoseconds, separated by a single space and with
// nothing else on the line.
//
// Nothing more may be added to it. The old tool parses the record by splitting
// on whitespace and rejecting anything that is not exactly two fields, so a
// third field -- the size, which is the one thing this format is missing --
// would make every record this service writes unreadable by the tool it is
// replacing, in a direction nobody notices until a rollback.
func formatAction(outputID string, modTime time.Time) string {
	return outputID + " " + strconv.FormatInt(modTime.UnixNano(), 10)
}

// parseAction reads that record.
//
// It is deliberately as lenient as the old tool about spacing -- fields are
// whitespace-separated, so a record written with a newline at the end still
// parses -- and exactly as strict about the field count.
func parseAction(data []byte) (action, error) {
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return action{}, fmt.Errorf("gobuild: action record has %d fields, want 2", len(fields))
	}
	ns, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return action{}, fmt.Errorf("gobuild: action record timestamp: %w", err)
	}
	return action{outputID: fields[0], modTime: time.Unix(ns/1e9, ns%1e9)}, nil
}

// validID refuses an id that cannot safely be part of a key.
//
// The ids the toolchain sends are hexadecimal hashes, but this is a network
// front-end and the check is what stops a caller from choosing keys: anything
// with a slash, a dot or a control character in it could name an object
// belonging to another front-end.
func validID(id string) error {
	if len(id) < 2 {
		return fmt.Errorf("%w: %q is shorter than two characters", ErrInvalidID, id)
	}
	if len(id) > 128 {
		return fmt.Errorf("%w: %q is longer than 128 characters", ErrInvalidID, id)
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch == '-', ch == '_':
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidID, id, string(ch))
		}
	}
	return nil
}
