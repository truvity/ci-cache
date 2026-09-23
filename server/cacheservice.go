package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truvity/ci-cache/engine/tier"
	cachev1 "github.com/truvity/ci-cache/gen/cache/v1"
	"github.com/truvity/ci-cache/gen/cache/v1/cachev1connect"
)

// chunkSize is how much of an object goes in one stream message.
//
// A megabyte is large enough that the per-message overhead -- a length prefix,
// a protobuf tag and a trip through the codec -- disappears against the
// payload, and small enough that it is not what decides the process's memory
// ceiling when the concurrency limit's worth of transfers are in flight at
// once. At 256 concurrent streams this is 256 MiB of buffers in the worst
// case, which is a number a pod's memory limit can be written against; at
// 16 MiB chunks it would be four gigabytes, which is not.
const chunkSize = 1 << 20

// cacheService serves cache.v1.Cache over the server's chain.
//
// It holds a tier.Tier rather than a *chain.Chain on purpose: the wire service
// is one tier speaking to another, and a test that wants to assert what the
// server does with a not-found should be able to hand it something that only
// returns not-founds.
type cacheService struct {
	chain tier.Tier
	log   *slog.Logger
}

// Stat answers exists=false for a miss rather than an error.
//
// That is what the proto promises, and it matters: a miss is the ordinary case
// for a cache, and a client that has to distinguish "absent" from "broken" by
// inspecting an error code will eventually get it wrong in the direction that
// fails the build.
func (s *cacheService) Stat(
	ctx context.Context,
	req *connect.Request[cachev1.StatRequest],
) (*connect.Response[cachev1.StatResponse], error) {
	key := req.Msg.GetKey()
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("key is required"))
	}
	m, err := s.chain.Stat(ctx, key)
	if errors.Is(err, tier.ErrNotFound) {
		return connect.NewResponse(&cachev1.StatResponse{Exists: false}), nil
	}
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&cachev1.StatResponse{Exists: true, Meta: metaToProto(m)}), nil
}

// Get streams the metadata message, then the object's bytes.
func (s *cacheService) Get(
	ctx context.Context,
	req *connect.Request[cachev1.GetRequest],
	stream *connect.ServerStream[cachev1.GetResponse],
) error {
	key := req.Msg.GetKey()
	if key == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("key is required"))
	}

	rc, m, err := s.chain.Get(ctx, key)
	if err != nil {
		return toConnectError(err)
	}
	defer func() { _ = rc.Close() }()

	// Metadata goes first and alone. Sending it before a single byte is read
	// from the tier is what lets the client size its buffer, and what makes a
	// slow bucket show up as a slow body rather than a slow response.
	if err := stream.Send(&cachev1.GetResponse{
		Part: &cachev1.GetResponse_Meta{Meta: metaToProto(m)},
	}); err != nil {
		return err
	}

	// One buffer for the whole stream. Send marshals the message before it
	// returns, so the bytes are copied out of here on every call and reusing it
	// cannot hand the codec a chunk that has since been overwritten.
	buf := make([]byte, chunkSize)
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			if err := stream.Send(&cachev1.GetResponse{
				Part: &cachev1.GetResponse_Chunk{Chunk: buf[:n]},
			}); err != nil {
				// A send failure is the client having gone away, not a cache
				// fault. Returning it unwrapped lets Connect decide the code;
				// wrapping it as Internal would fill the logs with clients that
				// cancelled a build.
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			s.log.Error("read object from chain", "key", key, "error", readErr)
			return connect.NewError(connect.CodeInternal, fmt.Errorf("read %s: %w", key, readErr))
		}
	}
}

// Put reads the header message, then streams the body straight into the tier.
//
// The body is never assembled here. An object may be hundreds of megabytes,
// and buffering it would put this process's memory ceiling in the hands of
// whatever is being built -- which is exactly the failure mode the streaming
// protocol exists to avoid.
func (s *cacheService) Put(
	ctx context.Context,
	stream *connect.ClientStream[cachev1.PutRequest],
) (*connect.Response[cachev1.PutResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("empty stream: expected a header message"))
	}
	header := stream.Msg().GetHeader()
	if header == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must be the header, not a chunk"))
	}
	key := header.GetKey()
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("key is required"))
	}

	body := &streamBody{stream: stream}
	if err := s.chain.Put(ctx, key, body, metaFromProto(header.GetMeta())); err != nil {
		return nil, toConnectError(err)
	}
	// The body reader may have stopped early -- a tier that refuses an existing
	// immutable key never reads it -- so a transport error it saw is reported
	// here rather than lost behind a successful Put.
	if err := body.err; err != nil && !errors.Is(err, io.EOF) {
		return nil, toConnectError(err)
	}

	// What was stored, not what the client claimed: a client that miscounted
	// its own body learns so now rather than on the next read.
	return connect.NewResponse(&cachev1.PutResponse{Size: body.n}), nil
}

// streamBody turns the client stream's chunk messages into an io.Reader, so
// that a tier which knows nothing about Connect can be written against.
type streamBody struct {
	stream *connect.ClientStream[cachev1.PutRequest]
	buf    []byte
	n      int64
	err    error
}

func (b *streamBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if !b.stream.Receive() {
			b.err = b.stream.Err()
			if b.err == nil {
				b.err = io.EOF
			}
			return 0, b.err
		}
		msg := b.stream.Msg()
		if h := msg.GetHeader(); h != nil {
			// A second header mid-body would mean the client is writing two
			// objects onto one stream. Refusing is the only safe reading; the
			// alternative is storing half of one under the name of the other.
			b.err = connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected second header in body"))
			return 0, b.err
		}
		b.buf = msg.GetChunk()
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	b.n += int64(n)
	return n, nil
}

// toConnectError maps the tier vocabulary onto the codes the proto documents.
//
// The mapping is load-bearing in both directions: an agent's remote tier turns
// these codes straight back into the same errors, and a chain decides whether
// to carry on -- ErrExists and ErrNoSpace are survivable, anything else is not
// -- by matching them. A tier error that arrived here as Internal would make a
// full cache look like a broken one.
func toConnectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tier.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, tier.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, tier.ErrNoSpace):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	// Already a Connect error -- from the body reader, say -- keeps its code.
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	return connect.NewError(connect.CodeInternal, err)
}

func metaToProto(m tier.Meta) *cachev1.Meta {
	p := &cachev1.Meta{
		Size:        m.Size,
		ContentType: m.ContentType,
		Immutable:   m.Immutable,
	}
	// A zero time is left unset rather than sent as the epoch: the Go toolchain
	// compares an output's modification time, and 1970 is a value it would
	// believe.
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

// The generated handler constructor would catch this too, but only where it is
// called; here the failure names the type.
var _ cachev1connect.CacheServiceHandler = (*cacheService)(nil)
