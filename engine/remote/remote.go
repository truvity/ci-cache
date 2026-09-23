// Package remote is the tier that lives on the other end of a network.
//
// It is what makes a runner's agent the front of the same chain the server
// runs: disk in front of remote, where remote is the server's disk in front of
// the bucket. Nothing above it knows the difference, which is the whole reason
// tier.Tier is one interface and not two.
//
// The transport is HTTP/2 without TLS. Inside a cluster the data port carries
// no certificate -- it is reached through a Service, and the boundary is the
// NetworkPolicy rather than a handshake -- so the client speaks h2c directly.
// net/http has done this natively since Go 1.24, through Protocols with
// unencrypted HTTP/2 set, so no HTTP/2 library is imported here.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truvity/ci-cache/engine/tier"
	cachev1 "github.com/truvity/ci-cache/gen/cache/v1"
	"github.com/truvity/ci-cache/gen/cache/v1/cachev1connect"
)

const (
	// chunkSize matches the server's. The two do not have to agree -- the
	// protocol is a stream of chunks of any size -- but a mismatch means one
	// side is reassembling what the other just split, for nothing.
	chunkSize = 1 << 20

	// DefaultStatTimeout bounds a metadata lookup end to end.
	//
	// Stat is on the hot path of every cache decision the toolchain makes, and
	// a Stat that takes longer than this has already cost more than the miss
	// it was trying to avoid. It is a TOTAL timeout, unlike the transfers,
	// because the answer is one small message: there is no legitimate reason
	// for it to arrive slowly.
	DefaultStatTimeout = 2 * time.Second

	// DefaultIdleTimeout bounds a transfer that has stopped making progress.
	//
	// It is an IDLE timeout and not a total one, and the distinction is the
	// point: a two-hundred-megabyte object over a busy link legitimately takes
	// minutes, and a total deadline would cut it off partway through and leave
	// the build to fetch it again from further away. What is never legitimate
	// is a minute in which not one byte arrived.
	DefaultIdleTimeout = 60 * time.Second

	// DefaultSentinelKey is the key Probe asks about. It is never written, so
	// a healthy server answers exists=false -- which is the answer Probe wants:
	// it proves the round trip, not the contents.
	DefaultSentinelKey = "probe/_healthz"
)

// ErrDeleteUnsupported is returned by Delete.
//
// The wire protocol has no Delete, deliberately: a runner that could delete
// from the shared cache could empty it for every other job, and eviction is
// the server's business. Returning an error rather than silently succeeding
// matters because tier.Tier says deleting an absent key is fine, so a quiet
// no-op here would make a failed wipe look like a finished one.
var ErrDeleteUnsupported = errors.New("remote: delete is not part of the wire protocol")

// Remote is a tier.Tier backed by the cache.v1.Cache service.
type Remote struct {
	client  cachev1connect.CacheServiceClient
	http    *http.Client
	owned   bool // whether Close may shut the transport down
	baseURL string

	label        string
	statTimeout  time.Duration
	idleTimeout  time.Duration
	sentinel     string
	clientOpts   []connect.ClientOption
	dialTimeout  time.Duration
	maxIdleConns int
}

// Option configures a Remote.
type Option func(*Remote)

// WithHTTPClient supplies the HTTP client.
//
// The caller then owns the transport, including its connection pool: pass one
// in when several tiers point at the same service and should share
// connections, and leave it out otherwise. A client passed here must be able
// to speak unencrypted HTTP/2, or gRPC will not work over it.
func WithHTTPClient(c *http.Client) Option {
	return func(r *Remote) {
		r.http = c
		r.owned = false
	}
}

// WithName overrides the tier's label in metrics and logs. The default is
// "remote"; a chain with two remotes wants them told apart.
func WithName(name string) Option {
	return func(r *Remote) { r.label = name }
}

// WithStatTimeout overrides DefaultStatTimeout.
func WithStatTimeout(d time.Duration) Option {
	return func(r *Remote) { r.statTimeout = d }
}

// WithIdleTimeout overrides DefaultIdleTimeout. It is the gap between bytes a
// transfer may have, never its total duration.
func WithIdleTimeout(d time.Duration) Option {
	return func(r *Remote) { r.idleTimeout = d }
}

// WithSentinelKey overrides the key Probe stats.
func WithSentinelKey(key string) Option {
	return func(r *Remote) { r.sentinel = key }
}

// WithDialTimeout bounds establishing the TCP connection. It is separate from
// the idle timeout because a server that is not there should be discovered in
// seconds, while a server that is there may legitimately be slow.
func WithDialTimeout(d time.Duration) Option {
	return func(r *Remote) { r.dialTimeout = d }
}

// WithClientOptions adds Connect client options -- an interceptor, or
// connect.WithGRPC to speak gRPC rather than the Connect protocol.
func WithClientOptions(opts ...connect.ClientOption) Option {
	return func(r *Remote) { r.clientOpts = append(r.clientOpts, opts...) }
}

// New returns a tier backed by the service at baseURL.
//
// baseURL includes the front-end's path prefix, because the service is mounted
// under one: http://ci-cache.ns.svc:8080/go/build. The generated client appends
// /cache.v1.Cache/<Method> to whatever it is given, so a base URL without the
// prefix reaches a 404 and reports it as Unimplemented, which reads like a
// version mismatch and is not one.
func New(baseURL string, opts ...Option) (*Remote, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("remote: parse %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("remote: %q must be an absolute http or https URL", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("remote: %q has no host", baseURL)
	}

	r := &Remote{
		baseURL:      strings.TrimRight(baseURL, "/"),
		owned:        true,
		label:        "remote",
		statTimeout:  DefaultStatTimeout,
		idleTimeout:  DefaultIdleTimeout,
		sentinel:     DefaultSentinelKey,
		dialTimeout:  5 * time.Second,
		maxIdleConns: 16,
	}
	for _, o := range opts {
		o(r)
	}
	if r.http == nil {
		r.http = newH2CClient(r.dialTimeout, r.maxIdleConns)
	}
	r.client = cachev1connect.NewCacheServiceClient(r.http, r.baseURL, r.clientOpts...)
	return r, nil
}

// newH2CClient builds the transport this tier uses on its own.
//
// One transport per Remote, and so one connection pool: HTTP/2 multiplexes
// every concurrent action of a build onto a handful of connections, which is
// the reason this is worth having over HTTP/1.1 at all. Sharing a pool across
// unrelated Remotes would be worse, not better -- their idle-connection
// lifetimes and their failure domains are different.
func newH2CClient(dialTimeout time.Duration, maxIdle int) *http.Client {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
	}
	// HTTP/1.1 is deliberately absent. With it included, net/http would prefer
	// HTTP/1.1 for http:// URLs and gRPC -- which needs trailers, and so needs
	// HTTP/2 -- would fail at the first call with a protocol error that names
	// neither of these lines.
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	protos.SetHTTP2(true)
	tr.Protocols = protos

	// No Client.Timeout. It is a deadline on the whole exchange including the
	// body, so setting it would truncate exactly the large transfers this tier
	// exists to carry. Bounding is done per call, and by idleness.
	return &http.Client{Transport: tr}
}

// Name implements tier.Tier.
func (r *Remote) Name() string { return r.label }

// Close releases the connection pool, when this Remote owns it.
func (r *Remote) Close() error {
	if r.owned {
		r.http.CloseIdleConnections()
	}
	return nil
}

// Stat implements tier.Tier. A miss is tier.ErrNotFound, not an exists=false
// the caller has to remember to check.
func (r *Remote) Stat(ctx context.Context, key string) (tier.Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, r.statTimeout)
	defer cancel()

	resp, err := r.client.Stat(ctx, connect.NewRequest(&cachev1.StatRequest{Key: key}))
	if err != nil {
		return tier.Meta{}, fromConnectError(err)
	}
	if !resp.Msg.GetExists() {
		return tier.Meta{}, fmt.Errorf("%w: %s", tier.ErrNotFound, key)
	}
	return metaFromProto(resp.Msg.GetMeta()), nil
}

// Get implements tier.Tier, streaming.
//
// The returned reader is the live Connect stream, not a buffer. Buffering here
// would defeat the point twice: the caller could not start writing the object
// to its own disk until the last byte arrived, and this process's memory would
// be decided by the size of whatever is being built.
func (r *Remote) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	ctx, cancel := context.WithCancel(ctx)
	idle := newIdleTimer(r.idleTimeout, cancel)

	fail := func(err error) (io.ReadCloser, tier.Meta, error) {
		idle.stop()
		cancel()
		return nil, tier.Meta{}, fromConnectError(err)
	}

	stream, err := r.client.Get(ctx, connect.NewRequest(&cachev1.GetRequest{Key: key}))
	if err != nil {
		return fail(err)
	}

	// The first message is the metadata and carries no bytes. Reading it here
	// rather than inside the reader is what lets Get report a miss as an error
	// -- a caller that got a ReadCloser back would otherwise not learn the
	// object was absent until its first Read.
	if !stream.Receive() {
		err := stream.Err()
		_ = stream.Close()
		if err == nil {
			err = fmt.Errorf("remote: %s: stream ended before its metadata", key)
		}
		return fail(err)
	}
	idle.tick()

	meta := stream.Msg().GetMeta()
	if meta == nil {
		_ = stream.Close()
		return fail(fmt.Errorf("remote: %s: first message was a chunk, expected metadata", key))
	}

	return &getStream{stream: stream, idle: idle, cancel: cancel, key: key}, metaFromProto(meta), nil
}

// getStream is the reader Get hands back: one Connect stream, read lazily.
type getStream struct {
	stream *connect.ServerStreamForClient[cachev1.GetResponse]
	idle   *idleTimer
	cancel context.CancelFunc
	key    string
	buf    []byte
	err    error
	once   sync.Once
}

func (g *getStream) Read(p []byte) (int, error) {
	for len(g.buf) == 0 {
		if g.err != nil {
			return 0, g.err
		}
		if !g.stream.Receive() {
			if err := g.stream.Err(); err != nil {
				g.err = fromConnectError(err)
			} else {
				g.err = io.EOF
			}
			return 0, g.err
		}
		g.idle.tick()
		g.buf = g.stream.Msg().GetChunk()
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	return n, nil
}

// Close ends the stream. It must be called even after io.EOF: the HTTP/2
// stream and its flow-control window are held until it is, and a caller that
// reads every object to the end and never closes runs the connection out of
// streams rather than leaking anything a heap profile would show.
func (g *getStream) Close() error {
	var err error
	g.once.Do(func() {
		g.idle.stop()
		err = g.stream.Close()
		// Cancelling after Close, not before: cancelling first would abort a
		// stream that had finished cleanly and turn a complete read into a
		// context error in the server's log.
		g.cancel()
	})
	return err
}

// Put implements tier.Tier, streaming the body as it is read.
func (r *Remote) Put(ctx context.Context, key string, body io.Reader, m tier.Meta) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := newIdleTimer(r.idleTimeout, cancel)
	defer idle.stop()

	stream := r.client.Put(ctx)
	if err := stream.Send(&cachev1.PutRequest{
		Part: &cachev1.PutRequest_Header{Header: &cachev1.PutHeader{
			Key:  key,
			Meta: metaToProto(m),
		}},
	}); err != nil {
		return r.closePut(stream, err)
	}
	idle.tick()

	buf := make([]byte, chunkSize)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if err := stream.Send(&cachev1.PutRequest{
				Part: &cachev1.PutRequest_Chunk{Chunk: buf[:n]},
			}); err != nil {
				return r.closePut(stream, err)
			}
			idle.tick()
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			// The source failed, not the wire. The stream is still closed so
			// the server is not left waiting, but the caller's error is what
			// is reported: it is the one that says what actually went wrong.
			_, _ = stream.CloseAndReceive()
			return fmt.Errorf("remote: read body for %s: %w", key, readErr)
		}
	}

	if _, err := stream.CloseAndReceive(); err != nil {
		return fromConnectError(err)
	}
	return nil
}

// closePut turns a failed Send into the error that explains it.
//
// Connect reports a send on a stream the server has already ended as io.EOF
// and keeps the real error -- AlreadyExists for an immutable key, say -- for
// CloseAndReceive. Returning the io.EOF instead would tell the caller the
// upload finished, which is the opposite of what happened.
func (r *Remote) closePut(
	stream *connect.ClientStreamForClient[cachev1.PutRequest, cachev1.PutResponse],
	sendErr error,
) error {
	if _, err := stream.CloseAndReceive(); err != nil {
		return fromConnectError(err)
	}
	if errors.Is(sendErr, io.EOF) {
		// The server ended the stream early but reported success, so the
		// object is stored. Reporting the io.EOF would tell the caller its
		// upload failed when it did not.
		return nil
	}
	return fromConnectError(sendErr)
}

// Delete implements tier.Tier by refusing. See ErrDeleteUnsupported.
func (r *Remote) Delete(_ context.Context, key string) error {
	return fmt.Errorf("%w: %s", ErrDeleteUnsupported, key)
}

// Probe reports whether the service is reachable and answering.
//
// A miss on the sentinel key is a healthy answer, and the expected one: what
// is being tested is the round trip, not the contents. The agent's fail-open
// check calls this, and the distinction matters there -- treating "no such
// object" as unreachable would have every runner fall back to a direct build
// against a cache that is working perfectly.
func (r *Remote) Probe(ctx context.Context) error {
	_, err := r.Stat(ctx, r.sentinel)
	if err == nil || errors.Is(err, tier.ErrNotFound) {
		return nil
	}
	return err
}

// idleTimer cancels a call when nothing has happened for a while.
//
// This is what makes the transfer timeout an idle one. A context deadline
// cannot express it -- a deadline is absolute -- so the timer is pushed
// forward on every message and only fires when the stream has genuinely
// stalled.
type idleTimer struct {
	mu      sync.Mutex
	timer   *time.Timer
	d       time.Duration
	stopped bool
}

func newIdleTimer(d time.Duration, onIdle func()) *idleTimer {
	t := &idleTimer{d: d}
	if d <= 0 {
		// A zero or negative timeout means no idle bound at all. Starting a
		// timer that fires immediately would cancel every call.
		t.stopped = true
		return t
	}
	t.timer = time.AfterFunc(d, onIdle)
	return t
}

// tick pushes the deadline out. It is called from whichever goroutine is
// reading the stream, which is why the timer is behind a mutex: Timer.Reset is
// not safe to race with the timer firing.
func (t *idleTimer) tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.timer == nil {
		return
	}
	t.timer.Reset(t.d)
}

func (t *idleTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
	}
	t.stopped = true
}

// fromConnectError turns the wire's codes back into the tier vocabulary.
//
// This is the other half of the server's mapping, and it has to be exact: a
// chain carries on past ErrExists and ErrNoSpace and gives up on anything
// else, so an AlreadyExists that came back as a plain error would abort a
// write that in fact succeeded -- somebody else got there first, which is the
// normal outcome of two runners building the same thing.
func fromConnectError(err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err
	}
	switch cerr.Code() {
	case connect.CodeNotFound:
		return fmt.Errorf("%w: %s", tier.ErrNotFound, cerr.Message())
	case connect.CodeAlreadyExists:
		return fmt.Errorf("%w: %s", tier.ErrExists, cerr.Message())
	case connect.CodeResourceExhausted:
		return fmt.Errorf("%w: %s", tier.ErrNoSpace, cerr.Message())
	}
	// Every other code is returned as it arrived, Connect error and all, so
	// that a caller can still read Unavailable off a server over its
	// concurrency limit and decide to try later.
	return err
}

func metaToProto(m tier.Meta) *cachev1.Meta {
	p := &cachev1.Meta{
		Size:        m.Size,
		ContentType: m.ContentType,
		Immutable:   m.Immutable,
	}
	if !m.ModTime.IsZero() {
		p.ModTime = timestamppb.New(m.ModTime)
	}
	return p
}

func metaFromProto(p *cachev1.Meta) tier.Meta {
	if p == nil {
		return tier.Meta{}
	}
	m := tier.Meta{
		Size:        p.GetSize(),
		ContentType: p.GetContentType(),
		Immutable:   p.GetImmutable(),
	}
	if ts := p.GetModTime(); ts != nil {
		m.ModTime = ts.AsTime()
	}
	return m
}

var _ tier.Tier = (*Remote)(nil)
