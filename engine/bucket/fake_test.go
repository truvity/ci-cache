package bucket

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeS3 records what the tier asked the store to do.
//
// It answers requests rather than simulating a store: there is no bucket in
// here, only the inputs, because what these tests are about is the request
// the SDK would have sent. A fake that tried to behave like S3 would invite
// assertions about the fake.
type fakeS3 struct {
	mu sync.Mutex

	puts    []*s3.PutObjectInput
	deletes [][]types.ObjectIdentifier
	lists   []*s3.ListObjectsV2Input

	// stored is what a Put kept, so that a Get can be answered with it and
	// metadata can be checked for a round trip rather than only on the way
	// out.
	stored map[string]storedObject

	// putErr, when set, decides the answer to a Put. It is how the
	// precondition path is exercised without a store to race against.
	putErr func(in *s3.PutObjectInput) error

	// stall, when set, holds every Put until it is closed. It is what turns
	// this fake into a store that has stopped answering, which is the state
	// the upload queue's bounds exist for.
	stall chan struct{}

	// listPages is handed out one per ListObjectsV2 call, for the paging and
	// batching tests.
	listPages []*s3.ListObjectsV2Output
}

type storedObject struct {
	body        []byte
	meta        map[string]string
	contentType *string
	modified    time.Time
}

func newFake() *fakeS3 { return &fakeS3{stored: map[string]storedObject{}} }

func (f *fakeS3) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	f.puts = append(f.puts, in)
	stall, putErr := f.stall, f.putErr
	f.mu.Unlock()

	if stall != nil {
		select {
		case <-stall:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if putErr != nil {
		if err := putErr(in); err != nil {
			return nil, err
		}
	}

	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored[aws.ToString(in.Key)] = storedObject{
		body:        body,
		meta:        in.Metadata,
		contentType: in.ContentType,
		modified:    time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
	}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.stored[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{
		Body:          &countingReader{r: bytes.NewReader(o.body)},
		ContentLength: aws.Int64(int64(len(o.body))),
		ContentType:   o.contentType,
		Metadata:      o.meta,
		LastModified:  aws.Time(o.modified),
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.stored[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(o.body))),
		ContentType:   o.contentType,
		Metadata:      o.meta,
		LastModified:  aws.Time(o.modified),
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stored, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, in.Delete.Objects)
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, in)
	if len(f.listPages) == 0 {
		return &s3.ListObjectsV2Output{}, nil
	}
	out := f.listPages[0]
	f.listPages = f.listPages[1:]
	return out, nil
}

func (f *fakeS3) putInputs() []*s3.PutObjectInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*s3.PutObjectInput(nil), f.puts...)
}

func (f *fakeS3) deleteBatches() [][]types.ObjectIdentifier {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]types.ObjectIdentifier(nil), f.deletes...)
}

// countingReader tells a test whether anything read the body before it did,
// which is how "Get streams" is stated as an assertion rather than a hope.
type countingReader struct {
	r    *bytes.Reader
	n    int
	done bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.n++
	return c.r.Read(p)
}

func (c *countingReader) Close() error {
	c.done = true
	return nil
}

// preconditionFailed is what a store answers to an If-None-Match that did not
// hold. Both shapes are built here because the tier has to recognise both: a
// store that parses into a typed error and one that only gives a status.
func preconditionFailed() error {
	return &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
}

func statusError(code int) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
			Err:      fmt.Errorf("http %d", code),
		},
	}
}

var _ S3API = (*fakeS3)(nil)
