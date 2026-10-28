package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/fetburner/rokuban/internal/objstore"
)

type fakeAPI struct {
	headFn        func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	completeErr   error
	completeCalls int
	abortCalls    int
	putPartCalls  int
	putCalls      int
}

func (f *fakeAPI) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putCalls++
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeAPI) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if f.headFn != nil {
		return f.headFn(ctx, in)
	}
	return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
}

func (*fakeAPI) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}

func (*fakeAPI) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}

func (*fakeAPI) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (f *fakeAPI) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if _, err := io.ReadAll(in.Body); err != nil {
		return nil, err
	}
	f.putPartCalls++
	return &s3.UploadPartOutput{ETag: aws.String("part-etag")}, nil
}

func (f *fakeAPI) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.completeCalls++
	return &s3.CompleteMultipartUploadOutput{}, f.completeErr
}

func (f *fakeAPI) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortCalls++
	return &s3.AbortMultipartUploadOutput{}, nil
}

type fakePresigner struct{}

func (fakePresigner) PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://objects.example.test/signed"}, nil
}

func (fakePresigner) PresignHeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://objects.example.test/signed-head"}, nil
}

func TestPutAbortsMultipartUploadWhenFullObjectChecksumDiffers(t *testing.T) {
	client := &fakeAPI{}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	data := []byte("multipart source")
	err := store.Put(context.Background(), "encoded/1.mp4", bytes.NewReader(data), int64(len(data)), [32]byte{1}, "video/mp4")
	if !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
	if client.putPartCalls != 1 {
		t.Fatalf("uploaded parts = %d, want 1", client.putPartCalls)
	}
	if client.completeCalls != 0 {
		t.Fatalf("CompleteMultipartUpload calls = %d, want 0", client.completeCalls)
	}
	if client.abortCalls != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1 after checksum mismatch", client.abortCalls)
	}
}

type delayedTrailingByteReader struct {
	data     *bytes.Reader
	zeroOnce bool
	trailing bool
}

func (r *delayedTrailingByteReader) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	if !r.zeroOnce {
		r.zeroOnce = true
		return 0, nil
	}
	if !r.trailing {
		r.trailing = true
		p[0] = 'x'
		return 1, nil
	}
	return 0, io.EOF
}

func TestPutRejectsTrailingByteAfterZeroProgressRead(t *testing.T) {
	client := &fakeAPI{}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	data := []byte("multipart source")
	digest := sha256.Sum256(data)
	src := &delayedTrailingByteReader{data: bytes.NewReader(data)}
	err := store.Put(context.Background(), "encoded/trailing.mp4", src, int64(len(data)), digest, "video/mp4")
	if err == nil {
		t.Fatal("Put accepted bytes beyond the declared object size after a zero-progress read")
	}
	if client.completeCalls != 0 || client.abortCalls != 1 {
		t.Fatalf("complete calls = %d, abort calls = %d; want 0 and 1", client.completeCalls, client.abortCalls)
	}
}

func TestPutRejectsNonEmptySourceForDeclaredEmptyObject(t *testing.T) {
	client := &fakeAPI{}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	emptySHA := sha256.Sum256(nil)
	err := store.Put(context.Background(), "thumbnails/empty.jpg", bytes.NewReader([]byte("unexpected")), 0, emptySHA, "image/jpeg")
	if err == nil {
		t.Fatal("Put accepted source bytes for a declared empty object")
	}
	if client.putCalls != 0 {
		t.Fatalf("PutObject calls = %d, want 0 after rejecting the extra source bytes", client.putCalls)
	}
}

func TestPutRecoversPreconditionFailedOnlyAfterHeadChecksumMatch(t *testing.T) {
	data := []byte("same bytes")
	digest := sha256.Sum256(data)
	client := &fakeAPI{completeErr: &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "412", Fault: smithy.FaultClient}}
	client.headFn = func(_ context.Context, _ *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		if client.completeCalls == 0 {
			return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
		}
		return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), Metadata: map[string]string{
			checksumMetadata: hex.EncodeToString(digest[:]),
		}}, nil
	}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived", conditionalWrites: true}
	if err := store.Put(context.Background(), "encoded/2.mp4", bytes.NewReader(data), int64(len(data)), digest, "video/mp4"); err != nil {
		t.Fatalf("Put after matching 412 recovery: %v", err)
	}
	if client.abortCalls != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want cleanup after 412", client.abortCalls)
	}
}

func TestPutDoesNotAccept412WhenHeadChecksumDiffers(t *testing.T) {
	data := []byte("new content")
	want := sha256.Sum256(data)
	client := &fakeAPI{completeErr: &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "412", Fault: smithy.FaultClient}}
	client.headFn = func(_ context.Context, _ *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		if client.completeCalls == 0 {
			return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
		}
		other := sha256.Sum256([]byte("old content"))
		return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), Metadata: map[string]string{
			checksumMetadata: hex.EncodeToString(other[:]),
		}}, nil
	}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived", conditionalWrites: true}
	err := store.Put(context.Background(), "encoded/3.mp4", bytes.NewReader(data), int64(len(data)), want, "video/mp4")
	if !errors.Is(err, objstore.ErrChecksumMismatch) {
		t.Fatalf("Put error = %v, want ErrChecksumMismatch", err)
	}
	if client.abortCalls != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", client.abortCalls)
	}
}

func TestMultipartPartSizeRespectsMaximumPartCount(t *testing.T) {
	if got := multipartPartSize(1); got != minPartSize {
		t.Fatalf("small part size = %d, want %d", got, minPartSize)
	}
	large := minPartSize*maxPartCount + 1
	got := multipartPartSize(large)
	if got < minPartSize || (large-1)/got+1 > maxPartCount {
		t.Fatalf("large part size = %d, yields %d parts; want >= %d and <= %d parts", got, (large-1)/got+1, minPartSize, maxPartCount)
	}
}

func TestPresignGetUsesBoundedURLLifetime(t *testing.T) {
	store := &Store{client: &fakeAPI{}, presigner: fakePresigner{}, bucket: "derived"}
	if _, err := store.Presign(context.Background(), "encoded/file.mp4", "GET", DefaultURLExpiry, "video/mp4", "file.mp4"); err != nil {
		t.Fatalf("Presign default TTL: %v", err)
	}
	if _, err := store.Presign(context.Background(), "encoded/file.mp4", "GET", maxURLExpiry+time.Second, "video/mp4", "file.mp4"); err == nil {
		t.Fatal("Presign accepted a URL lifetime longer than SigV4's maximum")
	}
}
