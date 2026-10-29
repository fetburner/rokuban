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
	headFn      func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	getFn       func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	completeErr error
	// completeErrs は CompleteMultipartUpload の応答を呼び出し順に返す。空なら completeErr を返す。
	completeErrs []error
	// putErrs は PutObject の応答を呼び出し順に返す。空なら成功する。
	putErrs        []error
	putInputs      []*s3.PutObjectInput
	completeInputs []*s3.CompleteMultipartUploadInput
	headCalls      int
	completeCalls  int
	abortCalls     int
	putPartCalls   int
	putCalls       int
	mutationCalls  int
}

func (f *fakeAPI) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putCalls++
	f.mutationCalls++
	f.putInputs = append(f.putInputs, in)
	if len(f.putErrs) > 0 {
		err := f.putErrs[0]
		f.putErrs = f.putErrs[1:]
		return &s3.PutObjectOutput{}, err
	}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeAPI) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.headCalls++
	if f.headFn != nil {
		return f.headFn(ctx, in)
	}
	return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
}

func (f *fakeAPI) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getFn != nil {
		return f.getFn(ctx, in)
	}
	return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
}

func (f *fakeAPI) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mutationCalls++
	return &s3.DeleteObjectOutput{}, nil
}

func (*fakeAPI) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}

func (f *fakeAPI) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mutationCalls++
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-1")}, nil
}

func (f *fakeAPI) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	f.mutationCalls++
	if _, err := io.ReadAll(in.Body); err != nil {
		return nil, err
	}
	f.putPartCalls++
	return &s3.UploadPartOutput{ETag: aws.String("part-etag")}, nil
}

func (f *fakeAPI) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.completeCalls++
	f.mutationCalls++
	f.completeInputs = append(f.completeInputs, in)
	if len(f.completeErrs) > 0 {
		err := f.completeErrs[0]
		f.completeErrs = f.completeErrs[1:]
		return &s3.CompleteMultipartUploadOutput{}, err
	}
	return &s3.CompleteMultipartUploadOutput{}, f.completeErr
}

func (f *fakeAPI) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortCalls++
	f.mutationCalls++
	return &s3.AbortMultipartUploadOutput{}, nil
}

type fakePresigner struct{}

func (fakePresigner) PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://objects.example.test/signed"}, nil
}

func (fakePresigner) PresignHeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	return &v4.PresignedHTTPRequest{URL: "https://objects.example.test/signed-head"}, nil
}

func TestReaderConstructorSkipsWriteProbeAndRejectsWrites(t *testing.T) {
	client := &fakeAPI{}
	store, err := newReaderWithClient(client, fakePresigner{}, "derived")
	if err != nil {
		t.Fatalf("newReaderWithClient: %v", err)
	}
	if client.mutationCalls != 0 {
		t.Fatalf("constructor made %d mutation calls, want none for read-only credentials", client.mutationCalls)
	}

	if _, err := store.Head(context.Background(), "encoded/1.mp4"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Head with reader store error = %v, want ErrNotFound from the read-capable fake", err)
	}
	if _, err := store.Presign(context.Background(), "encoded/1.mp4", "GET", time.Hour, "", ""); err != nil {
		t.Fatalf("Presign with reader store: %v", err)
	}
	if err := store.Put(context.Background(), "encoded/1.mp4", bytes.NewReader(nil), 0, sha256.Sum256(nil), "video/mp4"); !errors.Is(err, objstore.ErrReadOnly) {
		t.Fatalf("Put with reader store error = %v, want ErrReadOnly", err)
	}
	if err := store.Delete(context.Background(), "encoded/1.mp4"); !errors.Is(err, objstore.ErrReadOnly) {
		t.Fatalf("Delete with reader store error = %v, want ErrReadOnly", err)
	}
	if client.mutationCalls != 0 {
		t.Fatalf("read-only store made %d mutation calls, want none", client.mutationCalls)
	}
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
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
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
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
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

func TestGetStreamsObjectAndMapsMissingKeys(t *testing.T) {
	data := []byte("encoded source bytes")
	client := &fakeAPI{getFn: func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		if aws.ToString(in.Key) != "encoded/1.mp4" {
			t.Fatalf("GetObject key = %q, want encoded/1.mp4", aws.ToString(in.Key))
		}
		return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
	}}
	objects := &Store{client: client, bucket: "derived"}
	body, err := objects.Get(context.Background(), "encoded/1.mp4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(body)
	closeErr := body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("reading/closing Get body: read %v, close %v", err, closeErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("Get body = %q, want %q", got, data)
	}
	if _, err := (&Store{client: &fakeAPI{}, bucket: "derived"}).Get(context.Background(), "encoded/missing.mp4"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("missing Get error = %v, want ErrNotFound", err)
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

func TestNewMakesNoRequestsAndSucceedsWithoutReachableS3(t *testing.T) {
	client := &fakeAPI{}
	if _, err := newWithClient(client, fakePresigner{}, "derived"); err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	if client.headCalls != 0 || client.mutationCalls != 0 {
		t.Fatalf("constructor made %d HEAD and %d mutation calls, want none", client.headCalls, client.mutationCalls)
	}

	// 実際の SDK のクライアントでも、到達できない endpoint と資格情報なしで接続しない。
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	started := time.Now()
	if _, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "derived"}); err != nil {
		t.Fatalf("New with an unreachable endpoint and no credentials: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("New took %s, want no network wait", elapsed)
	}
}

func TestPutAlwaysSendsIfNoneMatchOnEmptyAndMultipartObjects(t *testing.T) {
	client := &fakeAPI{}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	empty := sha256.Sum256(nil)
	if err := store.Put(context.Background(), "thumbnails/empty.jpg", bytes.NewReader(nil), 0, empty, "image/jpeg"); err != nil {
		t.Fatalf("Put empty object: %v", err)
	}
	data := []byte("multipart source")
	if err := store.Put(context.Background(), "encoded/cond.mp4", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err != nil {
		t.Fatalf("Put multipart object: %v", err)
	}
	if len(client.putInputs) != 1 || aws.ToString(client.putInputs[0].IfNoneMatch) != "*" {
		t.Fatalf("PutObject inputs = %+v, want one call with If-None-Match: *", client.putInputs)
	}
	if len(client.completeInputs) != 1 || aws.ToString(client.completeInputs[0].IfNoneMatch) != "*" {
		t.Fatalf("CompleteMultipartUpload inputs = %+v, want one call with If-None-Match: *", client.completeInputs)
	}
}

func TestPutRetriesWithoutConditionOnlyWhenServerDoesNotSupportIt(t *testing.T) {
	notImplemented := &smithy.GenericAPIError{Code: "NotImplemented", Message: "501", Fault: smithy.FaultServer}
	client := &fakeAPI{putErrs: []error{notImplemented}, completeErrs: []error{notImplemented}}
	store := &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	empty := sha256.Sum256(nil)
	if err := store.Put(context.Background(), "thumbnails/empty2.jpg", bytes.NewReader(nil), 0, empty, "image/jpeg"); err != nil {
		t.Fatalf("Put empty object against a server without conditional writes: %v", err)
	}
	if len(client.putInputs) != 2 || client.putInputs[0].IfNoneMatch == nil || client.putInputs[1].IfNoneMatch != nil {
		t.Fatalf("PutObject inputs = %+v, want a conditional call then one without the condition", client.putInputs)
	}
	data := []byte("multipart source")
	if err := store.Put(context.Background(), "encoded/nocond.mp4", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4"); err != nil {
		t.Fatalf("Put multipart object against a server without conditional writes: %v", err)
	}
	if len(client.completeInputs) != 2 || client.completeInputs[0].IfNoneMatch == nil || client.completeInputs[1].IfNoneMatch != nil {
		t.Fatalf("CompleteMultipartUpload inputs = %+v, want a conditional call then one without the condition", client.completeInputs)
	}
	if client.abortCalls != 0 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 0 after the unconditional retry succeeded", client.abortCalls)
	}

	// 412 は非対応ではない。条件なしでやり直さず、HEAD の照合へ進む。
	precondition := &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "412", Fault: smithy.FaultClient}
	client = &fakeAPI{completeErrs: []error{precondition}}
	store = &Store{client: client, presigner: fakePresigner{}, bucket: "derived"}
	other := sha256.Sum256([]byte("other"))
	client.headFn = func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		if client.completeCalls == 0 {
			return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing", Fault: smithy.FaultClient}
		}
		return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), Metadata: map[string]string{
			checksumMetadata: hex.EncodeToString(other[:]),
		}}, nil
	}
	err := store.Put(context.Background(), "encoded/412.mp4", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data), "video/mp4")
	if !errors.Is(err, objstore.ErrChecksumMismatch) || client.completeCalls != 1 {
		t.Fatalf("Put after 412 = %v with %d Complete calls, want ErrChecksumMismatch after exactly one", err, client.completeCalls)
	}
}

func TestExpiredCredentialsAreClassifiedAndPermissionErrorsAreNot(t *testing.T) {
	expired := &smithy.GenericAPIError{Code: "ExpiredToken", Message: "expired", Fault: smithy.FaultClient}
	store := &Store{client: &fakeAPI{headFn: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		return nil, expired
	}}, presigner: fakePresigner{}, bucket: "derived"}
	if _, err := store.Head(context.Background(), "encoded/1.mp4"); !errors.Is(err, objstore.ErrCredentials) {
		t.Fatalf("Head with expired credentials = %v, want ErrCredentials", err)
	}
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied", Fault: smithy.FaultClient}
	store = &Store{client: &fakeAPI{headFn: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		return nil, denied
	}}, presigner: fakePresigner{}, bucket: "derived"}
	if _, err := store.Head(context.Background(), "encoded/1.mp4"); err == nil || errors.Is(err, objstore.ErrCredentials) {
		t.Fatalf("Head with a permission error = %v, want a non-credential error", err)
	}
}
