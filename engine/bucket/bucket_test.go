package bucket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier"
)

func newTestBucket(t *testing.T, cfg config.Store, f *fakeS3) *Bucket {
	t.Helper()
	if cfg.Bucket == "" {
		cfg.Bucket = "cache"
	}
	b, err := New(context.Background(), cfg, WithClient(f))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// TestPutSuppliesTheSumAndNeverTheAlgorithm is the regression test for a
// bucket CI cannot reach.
//
// ChecksumAlgorithm is what lets the SDK choose aws-chunked framing, which
// R2 answers with 403 SignatureDoesNotMatch. There is no unit test that can
// see the 403; this one instead holds the tier to the request that does not
// provoke it, and internal/storetest checks the other end against a real
// store when somebody has one.
func TestPutSuppliesTheSumAndNeverTheAlgorithm(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)

	body := []byte("a compressed object, as far as the store is concerned")
	if err := b.Put(context.Background(), "go/build/v1/abc", bytes.NewReader(body), tier.Meta{Size: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	puts := f.putInputs()
	if len(puts) != 1 {
		t.Fatalf("PutObject calls: %d, want 1", len(puts))
	}
	in := puts[0]

	sum := sha256.Sum256(body)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := aws.ToString(in.ChecksumSHA256); got != want {
		t.Errorf("ChecksumSHA256 = %q, want %q", got, want)
	}
	if in.ChecksumAlgorithm != "" {
		t.Errorf("ChecksumAlgorithm = %q, want empty: asking for an algorithm lets the SDK pick aws-chunked framing", in.ChecksumAlgorithm)
	}
	if got := aws.ToInt64(in.ContentLength); got != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", got, len(body))
	}
}

// TestPutSpilledBodyKeepsItsSum covers the staging path a large object takes.
// The sum has to be the sum of the whole object whether it was staged in
// memory or in a temp file, and the body the SDK gets has to start at zero.
func TestPutSpilledBodyKeepsItsSum(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)

	body := bytes.Repeat([]byte("0123456789abcdef"), (spillThreshold/16)+64)
	// Size is deliberately left unset, which is also what forces the spill
	// path for a body of unknown length.
	if err := b.Put(context.Background(), "big", bytes.NewReader(body), tier.Meta{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	in := f.putInputs()[0]
	sum := sha256.Sum256(body)
	if got, want := aws.ToString(in.ChecksumSHA256), base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("ChecksumSHA256 = %q, want %q", got, want)
	}
	if got := aws.ToInt64(in.ContentLength); got != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", got, len(body))
	}
	if got := len(f.stored[aws.ToString(in.Key)].body); got != len(body) {
		t.Errorf("stored %d bytes, want %d: the staged file was not rewound", got, len(body))
	}
}

func TestPutImmutableCarriesIfNoneMatch(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)

	body := []byte("content addressed")
	if err := b.Put(context.Background(), "mutable", bytes.NewReader(body), tier.Meta{Size: int64(len(body))}); err != nil {
		t.Fatalf("Put mutable: %v", err)
	}
	if got := f.putInputs()[0].IfNoneMatch; got != nil {
		t.Errorf("IfNoneMatch on a mutable put = %q, want unset", *got)
	}

	m := tier.Meta{Size: int64(len(body)), Immutable: true}
	if err := b.Put(context.Background(), "immutable", bytes.NewReader(body), m); err != nil {
		t.Fatalf("Put immutable: %v", err)
	}
	in := f.putInputs()[1]
	if got := aws.ToString(in.IfNoneMatch); got != "*" {
		t.Errorf("IfNoneMatch on an immutable put = %q, want %q", got, "*")
	}
	if in.Metadata[metaImmutable] != "true" {
		t.Errorf("metadata %s = %q, want \"true\"", metaImmutable, in.Metadata[metaImmutable])
	}
}

// TestPutExistingImmutableIsNotAnError pins the two shapes a refused
// precondition arrives in. Both mean the object is there, which is what two
// runners deriving the same thing looks like, and neither is a failure.
func TestPutExistingImmutableIsNotAnError(t *testing.T) {
	for name, answer := range map[string]error{
		"typed api error": preconditionFailed(),
		"bare 412":        statusError(412),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.putErr = func(*s3.PutObjectInput) error { return answer }
			b := newTestBucket(t, config.Store{}, f)

			err := b.Put(context.Background(), "k", strings.NewReader("x"), tier.Meta{Size: 1, Immutable: true})
			if !errors.Is(err, tier.ErrExists) {
				t.Fatalf("Put = %v, want %v", err, tier.ErrExists)
			}
		})
	}
}

func TestPutOtherErrorsSurvive(t *testing.T) {
	f := newFake()
	f.putErr = func(*s3.PutObjectInput) error { return statusError(403) }
	b := newTestBucket(t, config.Store{}, f)

	err := b.Put(context.Background(), "k", strings.NewReader("x"), tier.Meta{Size: 1})
	if err == nil || errors.Is(err, tier.ErrExists) {
		t.Fatalf("Put = %v, want the store's error", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("Put error %q does not carry the store's answer", err)
	}
}

// TestMetaRoundTrip is the assertion the Go toolchain depends on: an object
// faulted back out of the bucket has to carry the time whatever produced it
// stamped on it, to the nanosecond, or the toolchain rebuilds it and the
// cache did nothing.
func TestMetaRoundTrip(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)

	want := tier.Meta{
		Size:        9,
		ModTime:     time.Date(2026, 9, 23, 18, 4, 5, 123456789, time.UTC),
		ContentType: "application/zstd",
		Immutable:   true,
	}
	if err := b.Put(context.Background(), "k", strings.NewReader("123456789"), want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, got, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if !got.ModTime.Equal(want.ModTime) {
		t.Errorf("ModTime = %s, want %s", got.ModTime.Format(time.RFC3339Nano), want.ModTime.Format(time.RFC3339Nano))
	}
	if got.ContentType != want.ContentType || got.Immutable != want.Immutable || got.Size != want.Size {
		t.Errorf("Meta = %+v, want %+v", got, want)
	}

	st, err := b.Stat(context.Background(), "k")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !st.ModTime.Equal(want.ModTime) || st.ContentType != want.ContentType || st.Immutable != want.Immutable {
		t.Errorf("Stat = %+v, want %+v", st, want)
	}
}

// TestMetaFallsBackToLastModified covers a bucket somebody else filled -- one
// warmed by go-cache-plugin, say. The store's own timestamp is worse than the
// one we would have written and much better than the zero time.
func TestMetaFallsBackToLastModified(t *testing.T) {
	stamp := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	m := metaFrom(map[string]string{}, aws.String("application/octet-stream"), aws.Int64(7), aws.Time(stamp))
	if !m.ModTime.Equal(stamp) {
		t.Errorf("ModTime = %s, want %s", m.ModTime, stamp)
	}
}

// TestMetaIsReadCaseInsensitively guards against a store that echoes the
// letter case it was given instead of lowercasing user metadata. Reading it
// strictly would turn every object from such a store into one with no
// modification time.
func TestMetaIsReadCaseInsensitively(t *testing.T) {
	stamp := time.Date(2026, 9, 23, 18, 4, 5, 500000000, time.UTC)
	md := map[string]string{"Ci-Cache-Modtime": stamp.Format(time.RFC3339Nano), "CI-CACHE-IMMUTABLE": "true"}
	m := metaFrom(md, nil, nil, nil)
	if !m.ModTime.Equal(stamp) || !m.Immutable {
		t.Errorf("metaFrom = %+v, want the stamp and immutable", m)
	}
}

// TestGetStreams states the thing that is easiest to lose in a refactor: the
// body is handed over unread. Buffering it here would put the size of
// whatever somebody is building into this process's memory.
func TestGetStreams(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)
	body := bytes.Repeat([]byte("x"), 4096)
	if err := b.Put(context.Background(), "k", bytes.NewReader(body), tier.Meta{Size: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, _, err := b.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	cr, ok := rc.(*countingReader)
	if !ok {
		t.Fatalf("Get returned %T, want the store's own body", rc)
	}
	if cr.n != 0 {
		t.Fatalf("body was read %d times before the caller saw it", cr.n)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = rc.Close()
	if !bytes.Equal(got, body) {
		t.Errorf("body round trip differs")
	}
}

func TestMissesAreErrNotFound(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{}, f)

	if _, _, err := b.Get(context.Background(), "absent"); !errors.Is(err, tier.ErrNotFound) {
		t.Errorf("Get = %v, want %v", err, tier.ErrNotFound)
	}
	if _, err := b.Stat(context.Background(), "absent"); !errors.Is(err, tier.ErrNotFound) {
		t.Errorf("Stat = %v, want %v", err, tier.ErrNotFound)
	}
	// A store that gives only a status has to be understood too.
	if !isNotFound(statusError(404)) {
		t.Errorf("a bare 404 is not recognised as a miss")
	}
	// Deleting what is not there is the second run of a wipe, not a failure.
	if err := b.Delete(context.Background(), "absent"); err != nil {
		t.Errorf("Delete of an absent key = %v, want nil", err)
	}
}

func TestKeyPrefix(t *testing.T) {
	f := newFake()
	// The trailing slash is what somebody writes in a values file; it must
	// not become an empty path segment in every key.
	b := newTestBucket(t, config.Store{KeyPrefix: "shared/ci-cache/"}, f)

	if err := b.Put(context.Background(), "go/build/v1/abc", strings.NewReader("x"), tier.Meta{Size: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, want := aws.ToString(f.putInputs()[0].Key), "shared/ci-cache/go/build/v1/abc"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

// TestDeletePrefixBatches is about a wipe that finishes. Ten thousand objects
// deleted one request at a time is the difference between a wipe and
// something somebody gives up on, and a batch over a thousand keys is
// rejected whole.
func TestDeletePrefixBatches(t *testing.T) {
	f := newFake()
	const n = 2500
	contents := make([]types.Object, n)
	var want int64
	for i := range contents {
		size := int64(i + 1)
		contents[i] = types.Object{Key: aws.String(fmt.Sprintf("go/build/v1/%04d", i)), Size: aws.Int64(size)}
		want += size
	}
	// One page with more keys than a delete batch holds: a store may return
	// any number it likes regardless of MaxKeys, and the batching must be
	// ours rather than a side effect of paging.
	f.listPages = []*s3.ListObjectsV2Output{{Contents: contents}}
	b := newTestBucket(t, config.Store{}, f)

	entries, removed, err := b.DeletePrefix(context.Background(), "go/build")
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if entries != n {
		t.Errorf("entries = %d, want %d", entries, n)
	}
	if removed != want {
		t.Errorf("bytes = %d, want %d", removed, want)
	}

	batches := f.deleteBatches()
	sizes := make([]int, len(batches))
	for i, b := range batches {
		sizes[i] = len(b)
		if len(b) > deleteBatch {
			t.Errorf("batch %d carries %d keys, want at most %d", i, len(b), deleteBatch)
		}
	}
	if fmt.Sprint(sizes) != "[1000 1000 500]" {
		t.Errorf("batch sizes = %v, want [1000 1000 500]", sizes)
	}
}

func TestDeletePrefixPagesAndPrefixes(t *testing.T) {
	f := newFake()
	f.listPages = []*s3.ListObjectsV2Output{
		{
			Contents:              []types.Object{{Key: aws.String("p/go/a"), Size: aws.Int64(10)}},
			IsTruncated:           aws.Bool(true),
			NextContinuationToken: aws.String("next"),
		},
		{Contents: []types.Object{{Key: aws.String("p/go/b"), Size: aws.Int64(32)}}},
	}
	b := newTestBucket(t, config.Store{KeyPrefix: "p"}, f)

	entries, removed, err := b.DeletePrefix(context.Background(), "go/")
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if entries != 2 || removed != 42 {
		t.Errorf("entries, bytes = %d, %d; want 2, 42", entries, removed)
	}
	if got := aws.ToString(f.lists[0].Prefix); got != "p/go/" {
		t.Errorf("list prefix = %q, want %q", got, "p/go/")
	}
	if got := aws.ToString(f.lists[1].ContinuationToken); got != "next" {
		t.Errorf("second list continuation token = %q, want %q", got, "next")
	}
}

// TestDeletePrefixReportsPerKeyErrors matters because DeleteObjects answers
// 200 and lists what it refused. An unread Errors field is a wipe that
// reports objects it did not remove.
func TestDeletePrefixReportsPerKeyErrors(t *testing.T) {
	f := &refusingFake{fakeS3: newFake()}
	f.listPages = []*s3.ListObjectsV2Output{
		{Contents: []types.Object{{Key: aws.String("go/a"), Size: aws.Int64(10)}}},
	}
	b, err := New(context.Background(), config.Store{Bucket: "cache"}, WithClient(f))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := b.DeletePrefix(context.Background(), "go"); err == nil {
		t.Fatal("DeletePrefix = nil, want the refusal the store reported")
	}
}

type refusingFake struct{ *fakeS3 }

func (f *refusingFake) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	return &s3.DeleteObjectsOutput{Errors: []types.Error{{
		Key: in.Delete.Objects[0].Key, Code: aws.String("AccessDenied"), Message: aws.String("nope"),
	}}}, nil
}

func TestReachableListsOneKey(t *testing.T) {
	f := newFake()
	b := newTestBucket(t, config.Store{KeyPrefix: "p"}, f)

	if err := b.Reachable(context.Background()); err != nil {
		t.Fatalf("Reachable: %v", err)
	}
	if got := aws.ToInt32(f.lists[0].MaxKeys); got != 1 {
		t.Errorf("MaxKeys = %d, want 1: a readiness probe must not page a bucket", got)
	}
	if got := aws.ToString(f.lists[0].Prefix); got != "p" {
		t.Errorf("Prefix = %q, want %q", got, "p")
	}
}

func TestReachableReportsFailure(t *testing.T) {
	f := &unreachableFake{fakeS3: newFake()}
	b, err := New(context.Background(), config.Store{Bucket: "cache"}, WithClient(f))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Reachable(context.Background()); err == nil {
		t.Fatal("Reachable = nil on a store that answers nothing")
	}
}

type unreachableFake struct{ *fakeS3 }

func (f *unreachableFake) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// TestClientOptionsForACustomEndpoint is the other half of the R2 story. The
// SDK's default CRC32 on every request and its insistence on validating one
// on every response are both wrong against a store that is not AWS: one is
// rejected, the other is never sent and fails a Get that worked.
func TestClientOptionsForACustomEndpoint(t *testing.T) {
	var o s3.Options
	clientOptions(config.Store{
		Bucket:    "cache",
		Region:    "auto",
		Endpoint:  "https://account.r2.cloudflarestorage.com",
		PathStyle: true,
	})(&o)

	if got := aws.ToString(o.BaseEndpoint); got != "https://account.r2.cloudflarestorage.com" {
		t.Errorf("BaseEndpoint = %q", got)
	}
	if !o.UsePathStyle {
		t.Errorf("UsePathStyle = false, want true")
	}
	if o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Errorf("RequestChecksumCalculation = %v, want WhenRequired", o.RequestChecksumCalculation)
	}
	if o.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Errorf("ResponseChecksumValidation = %v, want WhenRequired", o.ResponseChecksumValidation)
	}

	// The same settings reach anything else built from the shared aws.Config.
	lo := loadedOptions(t, config.Store{Bucket: "cache", Region: "auto", Endpoint: "https://account.r2.cloudflarestorage.com"})
	if lo.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired ||
		lo.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Errorf("load options = %+v, want both WhenRequired", lo)
	}
}

// loadedOptions applies what New would hand LoadDefaultConfig, so the
// settings can be read back without loading anybody's real credentials.
func loadedOptions(t *testing.T, cfg config.Store) awscfg.LoadOptions {
	t.Helper()
	var lo awscfg.LoadOptions
	for _, f := range loadOptions(cfg) {
		if err := f(&lo); err != nil {
			t.Fatalf("load option: %v", err)
		}
	}
	return lo
}

// TestClientOptionsOnAWSChangeNothing: on AWS the defaults are right, and
// turning the checksums off there would lose an integrity check for nothing.
func TestClientOptionsOnAWSChangeNothing(t *testing.T) {
	var o s3.Options
	clientOptions(config.Store{Bucket: "cache", Region: "eu-central-1", PathStyle: true})(&o)

	if o.BaseEndpoint != nil {
		t.Errorf("BaseEndpoint = %q, want unset", *o.BaseEndpoint)
	}
	if o.UsePathStyle {
		t.Errorf("UsePathStyle = true: path style is for a store whose certificate needs it")
	}
	if o.RequestChecksumCalculation != 0 || o.ResponseChecksumValidation != 0 {
		t.Errorf("checksum settings = %v/%v, want the SDK's defaults", o.RequestChecksumCalculation, o.ResponseChecksumValidation)
	}
}

func TestNewRefusesAnEndpointWithoutARegion(t *testing.T) {
	_, err := New(context.Background(), config.Store{Bucket: "cache", Endpoint: "https://example.invalid"}, WithClient(newFake()))
	if err == nil {
		t.Fatal("New = nil, want a refusal: the SDK would default the region and fail later, elsewhere")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestNewRefusesAnEmptyBucket(t *testing.T) {
	if _, err := New(context.Background(), config.Store{Region: "auto"}, WithClient(newFake())); err == nil {
		t.Fatal("New = nil, want a refusal")
	}
}
