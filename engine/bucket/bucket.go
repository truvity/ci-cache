// Package bucket is the object store behind every other tier.
//
// It is the only tier that outlives a node, so it is the one whose requests
// have to be exactly right: a disk tier that gets a header wrong loses a
// cache entry, and this one gets a 403 from a store that is not AWS and loses
// every write until somebody reads a packet capture. Most of what follows is
// therefore about the shape of the request rather than about caching, and the
// tests assert that shape rather than the behaviour of any particular store.
package bucket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

// The metadata this tier keeps beside an object, sent as x-amz-meta-<name>.
//
// An S3 store lowercases user metadata names and hands them back that way, so
// the constants are lowercase and reads are case-insensitive: a store that
// echoes the case it was given must not look like an object with no metadata.
const (
	metaModTime   = "ci-cache-modtime"
	metaImmutable = "ci-cache-immutable"
)

// spillThreshold is where staging a body for a Put stops using memory and
// uses a temp file.
//
// A body has to be staged at all because the SHA-256 goes in a signed header,
// which means it must be known before the request starts. The alternative --
// letting the SDK stream and send the sum in a trailer -- is the aws-chunked
// framing that Put refuses to ask for; see the comment there.
const spillThreshold = 8 << 20

// deleteBatch is the most keys one DeleteObjects call carries. It is the S3
// API's own limit, not a tuning knob: a batch of 1001 is rejected whole.
const deleteBatch = 1000

// S3API is the part of the S3 client this tier uses.
//
// It exists so the tests can assert the shape of a request without a store to
// send it to. That is not an abstraction for its own sake: the bug this
// package was written around was in what the SDK put on the wire, so the only
// test that could have caught it is one that inspects the request, and it has
// to be able to run in a CI job that can reach no bucket at all.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Bucket is the object-store tier.
type Bucket struct {
	api    S3API
	bucket string
	prefix string
	log    *slog.Logger
}

// options is what New and NewQueue share. One Option type covers both because
// a caller configures one bucket and its queue together, and two nearly
// identical option types would only invite passing the wrong one.
type options struct {
	api S3API
	log *slog.Logger
}

// Option configures a Bucket or its upload Queue.
type Option func(*options)

// WithClient replaces the S3 client. Tests use it; a server never does, since
// the whole point of New is that it builds the client the right way.
func WithClient(api S3API) Option {
	return func(o *options) { o.api = api }
}

// WithLogger sets where a background upload reports a failure. Nothing on the
// request path logs: a miss is not an event.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.log = l }
}

// New returns the bucket tier for cfg.
//
// Credentials come from the SDK's default chain and from nowhere else. In a
// pod that is either the static keys the chart projects from a Secret as
// environment variables, or a pod identity's web identity token; on a laptop
// it is whatever `aws configure` left. This package reads no key itself, so
// there is no path by which a credential reaches a log or a core file through
// it.
func New(ctx context.Context, cfg config.Store, opts ...Option) (*Bucket, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("bucket: store.bucket is required")
	}
	// An endpoint with no region is refused rather than defaulted, because
	// the default would work. The SDK would fall back to whatever the
	// environment carries, or to a bucket-location lookup that R2 rejects
	// outright, and the failure would arrive later and somewhere else.
	if cfg.Endpoint != "" && cfg.Region == "" {
		return nil, errors.New("bucket: store.endpoint needs store.region (use \"auto\" on Cloudflare R2)")
	}

	b := &Bucket{
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.KeyPrefix, "/"),
		log:    o.log,
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	if o.api != nil {
		b.api = o.api
		return b, nil
	}

	ac, err := awscfg.LoadDefaultConfig(ctx, loadOptions(cfg)...)
	if err != nil {
		return nil, fmt.Errorf("bucket: aws config: %w", err)
	}
	b.api = s3.NewFromConfig(ac, clientOptions(cfg))
	return b, nil
}

// loadOptions is how cfg reaches the SDK's own configuration.
//
// The checksum settings are repeated on the client in clientOptions. That is
// deliberate: these govern anything else built from the same aws.Config, and
// those govern the requests this package makes, and a reader looking at
// either place should see the decision rather than have to find the other.
func loadOptions(cfg config.Store) []func(*awscfg.LoadOptions) error {
	opts := []func(*awscfg.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awscfg.WithRegion(cfg.Region))
	}
	if cfg.Endpoint != "" {
		opts = append(opts,
			awscfg.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
			awscfg.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
	}
	return opts
}

// clientOptions is the S3 client's configuration for cfg.
//
// Everything here is conditional on a custom endpoint, because everything
// here is a concession to a store that is not AWS. On AWS the SDK's defaults
// are right and turning them off would only lose an integrity check.
func clientOptions(cfg config.Store) func(*s3.Options) {
	return func(o *s3.Options) {
		if cfg.Endpoint == "" {
			return
		}
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		// Path style is a property of the store's certificate rather than of
		// the endpoint -- does its wildcard cover a bucket subdomain? -- so
		// it is carried through as its own switch instead of being inferred.
		o.UsePathStyle = cfg.PathStyle
		// The SDK computes a CRC32 on every request and asks for one on every
		// response by default. Stores other than AWS either reject the header
		// or ignore it, and a response checksum that is never sent makes the
		// SDK fail a Get that succeeded. WhenRequired leaves the checksum we
		// ask for ourselves -- the SHA-256 on a Put -- and nothing else.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}
}

// Name implements tier.Tier.
func (b *Bucket) Name() string { return "bucket" }

// Bucket returns the bucket's name, for a log line or an admin page that has
// to say where the objects went.
func (b *Bucket) Bucket() string { return b.bucket }

// objectKey is the key as the store sees it.
func (b *Bucket) objectKey(key string) string {
	if b.prefix == "" {
		return key
	}
	return b.prefix + "/" + key
}

// Put stores the object.
//
// The one thing in this method that is not obvious is the checksum. The SDK
// offers two ways to ask for one: set ChecksumSHA256 to the sum, or set
// ChecksumAlgorithm and let it compute. Only the first is safe here.
//
// Asking for an ALGORITHM hands the SDK the choice of framing, and for a body
// that also carries a Content-Encoding it chooses the aws-chunked trailer
// encoding: the sum is sent after the body, the signature covers a payload
// literal rather than the bytes, and Cloudflare R2 answers 403
// SignatureDoesNotMatch. Every object this cache stores is a compressed blob
// under a key a build tool chose, so that is not an edge case, it is the
// normal write. Measured against a real R2 bucket on 2026-09-23; it cost an
// evening, and internal/storetest exists so it costs nobody a second one.
// ChecksumAlgorithm must stay unset.
func (b *Bucket) Put(ctx context.Context, key string, r io.Reader, m tier.Meta) error {
	body, size, sum, release, err := stage(r)
	if err != nil {
		return fmt.Errorf("bucket: stage %s: %w", key, err)
	}
	defer release()

	in := &s3.PutObjectInput{
		Bucket:         aws.String(b.bucket),
		Key:            aws.String(b.objectKey(key)),
		Body:           body,
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(sum),
		Metadata:       metadataFor(m),
	}
	if m.ContentType != "" {
		in.ContentType = aws.String(m.ContentType)
	}
	if m.Immutable {
		// The store decides the race, not us. Two runners that derived the
		// same content-addressed object will both write it, and the loser has
		// to learn that from the store rather than from a read that may be
		// answered by a replica that has not caught up.
		in.IfNoneMatch = aws.String("*")
	}

	if _, err := b.api.PutObject(ctx, in); err != nil {
		if isPreconditionFailed(err) {
			return tier.ErrExists
		}
		return fmt.Errorf("bucket: put %s: %w", key, err)
	}
	return nil
}

// stage reads the body far enough to know its length and its SHA-256, which
// the request needs before it can be signed.
//
// Small bodies stay in memory; anything larger, or of unknown length, goes to
// a temp file, so that memory is decided by how many uploads are in flight
// rather than by how big a thing somebody is building.
func stage(r io.Reader) (body io.Reader, size int64, sum string, release func(), err error) {
	h := sha256.New()
	noop := func() {}

	head, err := io.ReadAll(io.LimitReader(r, spillThreshold))
	if err != nil {
		return nil, 0, "", noop, err
	}
	if len(head) < spillThreshold {
		h.Write(head)
		return bytes.NewReader(head), int64(len(head)), base64.StdEncoding.EncodeToString(h.Sum(nil)), noop, nil
	}

	f, err := os.CreateTemp("", "ci-cache-upload-*")
	if err != nil {
		return nil, 0, "", noop, err
	}
	release = func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := f.Write(head); err != nil {
		release()
		return nil, 0, "", noop, err
	}
	h.Write(head)
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		release()
		return nil, 0, "", noop, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		release()
		return nil, 0, "", noop, err
	}
	return f, int64(len(head)) + n, base64.StdEncoding.EncodeToString(h.Sum(nil)), release, nil
}

// metadataFor is what of a tier.Meta does not fit an S3 header of its own.
//
// The modification time is the one that matters: the Go toolchain compares
// the mtime of a cached output against its inputs, so an object that comes
// back with the time it was uploaded is an object the toolchain rebuilds. It
// goes out to the nanosecond because that is what a filesystem gave us, and a
// round trip that truncates it is a rebuild.
func metadataFor(m tier.Meta) map[string]string {
	md := map[string]string{}
	if !m.ModTime.IsZero() {
		md[metaModTime] = m.ModTime.UTC().Format(time.RFC3339Nano)
	}
	if m.Immutable {
		md[metaImmutable] = "true"
	}
	return md
}

// metaFrom rebuilds a tier.Meta from what the store returned.
func metaFrom(md map[string]string, contentType *string, contentLength *int64, lastModified *time.Time) tier.Meta {
	m := tier.Meta{}
	if contentLength != nil {
		m.Size = *contentLength
	}
	if contentType != nil {
		m.ContentType = *contentType
	}
	if v := lookup(md, metaModTime); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			m.ModTime = t
		}
	}
	if m.ModTime.IsZero() && lastModified != nil {
		// Fallback for an object written by something other than this tier --
		// a bucket warmed by go-cache-plugin, say. It is worse than the real
		// time, and better than the zero time.
		m.ModTime = *lastModified
	}
	m.Immutable = lookup(md, metaImmutable) == "true"
	return m
}

// lookup reads user metadata without trusting the store's letter case.
func lookup(md map[string]string, name string) string {
	if v, ok := md[name]; ok {
		return v
	}
	for k, v := range md {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Get streams the object.
//
// The body is handed to the caller unread. Buffering it here would put the
// size of whatever is being cached into this process's memory for no gain:
// the caller is either copying it to an HTTP response or spilling it to disk,
// and both of those already know how to stream.
func (b *Bucket) Get(ctx context.Context, key string) (io.ReadCloser, tier.Meta, error) {
	out, err := b.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, tier.Meta{}, tier.ErrNotFound
		}
		return nil, tier.Meta{}, fmt.Errorf("bucket: get %s: %w", key, err)
	}
	return out.Body, metaFrom(out.Metadata, out.ContentType, out.ContentLength, out.LastModified), nil
}

// Stat reports the object's metadata without its bytes.
func (b *Bucket) Stat(ctx context.Context, key string) (tier.Meta, error) {
	out, err := b.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.objectKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return tier.Meta{}, tier.ErrNotFound
		}
		return tier.Meta{}, fmt.Errorf("bucket: stat %s: %w", key, err)
	}
	return metaFrom(out.Metadata, out.ContentType, out.ContentLength, out.LastModified), nil
}

// Delete removes the object. An absent key is success, per the tier contract:
// a wipe that runs twice must not fail the second time.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	_, err := b.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.objectKey(key)),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("bucket: delete %s: %w", key, err)
	}
	return nil
}

// DeletePrefix implements tier.Deleter.
//
// Keys are accumulated across listing pages and deleted a thousand at a time
// rather than page by page, so that a store which returns fewer keys than we
// asked for -- which the API permits at any time -- does not turn a wipe into
// one request per object.
func (b *Bucket) DeletePrefix(ctx context.Context, prefix string) (entries, removed int64, err error) {
	var (
		pending      []types.ObjectIdentifier
		pendingBytes int64
		token        *string
	)

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		out, err := b.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{Objects: pending, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("bucket: delete %d objects under %s: %w", len(pending), prefix, err)
		}
		if len(out.Errors) > 0 {
			// A batch reports per-key failures in a 200, so an unread Errors
			// field is a wipe that says it removed objects it did not.
			e := out.Errors[0]
			return fmt.Errorf("bucket: %d of %d objects under %s refused (first %s: %s)",
				len(out.Errors), len(pending), prefix, aws.ToString(e.Key), aws.ToString(e.Message))
		}
		entries += int64(len(pending))
		removed += pendingBytes
		pending, pendingBytes = pending[:0], 0
		return nil
	}

	for {
		out, err := b.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucket),
			Prefix:            aws.String(b.objectKey(prefix)),
			ContinuationToken: token,
			MaxKeys:           aws.Int32(deleteBatch),
		})
		if err != nil {
			return entries, removed, fmt.Errorf("bucket: list %s: %w", prefix, err)
		}
		for _, o := range out.Contents {
			pending = append(pending, types.ObjectIdentifier{Key: o.Key})
			if o.Size != nil {
				pendingBytes += *o.Size
			}
			if len(pending) == deleteBatch {
				if err := flush(); err != nil {
					return entries, removed, err
				}
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	if err := flush(); err != nil {
		return entries, removed, err
	}
	return entries, removed, nil
}

// Reachable reports whether the store answers, for the server's readiness
// probe.
//
// It lists one key rather than heading a known object, because there is no
// object this cache can assume exists, and an empty bucket is a healthy one.
// A pod that cannot reach its bucket should leave the Service's endpoints:
// serving misses out of a disk that nothing refills looks like a working
// cache right up until somebody measures the hit rate.
func (b *Bucket) Reachable(ctx context.Context) error {
	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		MaxKeys: aws.Int32(1),
	}
	if b.prefix != "" {
		in.Prefix = aws.String(b.prefix)
	}
	if _, err := b.api.ListObjectsV2(ctx, in); err != nil {
		return fmt.Errorf("bucket: %s unreachable: %w", b.bucket, err)
	}
	return nil
}

// isNotFound recognises a miss.
//
// Three shapes reach here for one condition: GetObject returns a typed
// NoSuchKey, HeadObject has no body to parse an error code out of and returns
// NotFound, and a store that is not AWS may send neither and only a 404.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	if code := apiErrorCode(err); code == "NoSuchKey" || code == "NotFound" {
		return true
	}
	return httpStatus(err) == http.StatusNotFound
}

// isPreconditionFailed recognises the store refusing an If-None-Match, which
// is an immutable object that is already there. It is the ordinary outcome of
// two runners deriving the same thing, and tier.ErrExists, not an error.
func isPreconditionFailed(err error) bool {
	if apiErrorCode(err) == "PreconditionFailed" {
		return true
	}
	return httpStatus(err) == http.StatusPreconditionFailed
}

func apiErrorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// Compile-time proof that this tier is what the chain expects and what the
// admin wipe needs. A tier that no longer satisfies Deleter would otherwise
// silently fall back to deleting one key at a time.
var (
	_ tier.Tier    = (*Bucket)(nil)
	_ tier.Deleter = (*Bucket)(nil)
)
