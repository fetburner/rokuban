// Package s3store is the only production package that imports the AWS S3 SDK.
package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
)

const (
	// DefaultURLExpiry is the initial playback URL lifetime selected from the #1614 browser measurements.
	DefaultURLExpiry = objstore.DefaultURLExpiry
	maxURLExpiry     = 7 * 24 * time.Hour
	minPartSize      = int64(5 << 20)
	maxPartCount     = int64(10_000)
	checksumMetadata = "rokuban-sha256"
)

// Config contains the non-secret connection settings for one S3 bucket.
type Config struct {
	Endpoint string
	Region   string
	Bucket   string
}

type api interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

type presigner interface {
	PresignGetObject(context.Context, *s3.GetObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
	PresignHeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// Store implements objstore.Store using S3 API operations.
type Store struct {
	client    api
	presigner presigner
	bucket    string
	readOnly  bool
}

// New builds a store for one bucket from the AWS default provider chain without contacting S3.
// Credentials are resolved on the first request, so a process starts even when S3 is unreachable
// or its credentials have expired; only the jobs that use the store fail.
// Credentials are supplied by the process environment, workload identity, or the standard
// AWS shared credential providers; they are not stored in config files.
func New(ctx context.Context, cfg Config) (*Store, error) {
	client, signer, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newWithClient(client, signer, cfg.Bucket)
}

// NewReader builds a store like New whose returned store rejects Put and Delete calls.
// It is intended for streamer processes that only need to inspect objects and create signed URLs.
func NewReader(ctx context.Context, cfg Config) (*Store, error) {
	client, signer, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newReaderWithClient(client, signer, cfg.Bucket)
}

func newClient(ctx context.Context, cfg Config) (api, presigner, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, nil, errors.New("S3 bucket is empty")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, nil, fmt.Errorf("loading AWS credentials and config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
			options.UsePathStyle = true
		}
	})
	return client, s3.NewPresignClient(client), nil
}

func newWithClient(client api, signer presigner, bucket string) (*Store, error) {
	if client == nil || signer == nil {
		return nil, errors.New("S3 client and presigner are required")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, errors.New("S3 bucket is empty")
	}
	return &Store{client: client, presigner: signer, bucket: bucket}, nil
}

func newReaderWithClient(client api, signer presigner, bucket string) (*Store, error) {
	if client == nil || signer == nil {
		return nil, errors.New("S3 client and presigner are required")
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, errors.New("S3 bucket is empty")
	}
	return &Store{client: client, presigner: signer, bucket: bucket, readOnly: true}, nil
}

// Put uploads a complete object using multipart upload and verifies the full SHA-256 in the process.
func (s *Store) Put(ctx context.Context, key string, src io.Reader, size int64, wantSHA [32]byte, contentType string) error {
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	if err := validateKey(key); err != nil {
		return fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	if size < 0 {
		return fmt.Errorf("negative S3 object size %d", size)
	}
	if _, err := s.Head(ctx, key); err == nil {
		return s.recoverExisting(ctx, key, size, wantSHA)
	} else if !errors.Is(err, objstore.ErrNotFound) {
		return fmt.Errorf("checking existing S3 object %q: %w", key, err)
	}
	if size == 0 {
		var extra [1]byte
		if n, readErr := io.ReadFull(src, extra[:]); n != 0 || !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("S3 object source for %q contains bytes beyond declared size 0", key)
		}
		return s.putEmptyObject(ctx, key, contentType, wantSHA)
	}

	partSize := multipartPartSize(size)
	uploadID, err := s.startMultipart(ctx, key, contentType, wantSHA)
	if err != nil {
		return err
	}
	parts := make([]types.CompletedPart, 0, (size-1)/partSize+1)
	abort := func(cause error) error {
		if abortErr := s.abortMultipart(ctx, key, uploadID); abortErr != nil {
			return errors.Join(cause, abortErr)
		}
		return cause
	}

	hasher := sha256.New()
	remaining := size
	for partNumber := int32(1); remaining > 0; partNumber++ {
		length := partSize
		if remaining < length {
			length = remaining
		}
		body := make([]byte, int(length))
		if _, err := io.ReadFull(src, body); err != nil {
			return abort(fmt.Errorf("reading S3 multipart part %d for %q: %w", partNumber, key, err))
		}
		if _, err := hasher.Write(body); err != nil {
			return abort(fmt.Errorf("hashing S3 multipart part %d for %q: %w", partNumber, key, err))
		}
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(partNumber), Body: bytes.NewReader(body), ContentLength: aws.Int64(length),
		})
		if err != nil {
			return abort(withCredentialClass(fmt.Errorf("uploading S3 multipart part %d for %q: %w", partNumber, key, err)))
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(partNumber)})
		remaining -= length
	}
	var extra [1]byte
	if n, err := io.ReadFull(src, extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return abort(fmt.Errorf("S3 object source for %q contains more than %d bytes", key, size))
	}
	var gotSHA [32]byte
	copy(gotSHA[:], hasher.Sum(nil))
	if gotSHA != wantSHA {
		return abort(fmt.Errorf("S3 object %q: %w", key, objstore.ErrChecksumMismatch))
	}

	err = s.completeMultipart(ctx, key, uploadID, parts)
	if err == nil {
		return nil
	}
	if isPreconditionFailed(err) {
		if recoveryErr := s.recoverExisting(ctx, key, size, wantSHA); recoveryErr == nil {
			return abort(nil)
		} else {
			return abort(fmt.Errorf("S3 object %q failed 412 HEAD verification: %w", key, recoveryErr))
		}
	}
	// A lost CompleteMultipartUpload response can leave a complete object even though the
	// request returned an error. HEAD is the recovery check; multipart ETags are not SHA-256.
	if recoveryErr := s.recoverExisting(ctx, key, size, wantSHA); recoveryErr == nil {
		return nil
	}
	return abort(withCredentialClass(fmt.Errorf("completing S3 multipart upload for %q: %w", key, err)))
}

// Head returns metadata for a completed key. An absent object differs from a service error.
func (s *Store) Head(ctx context.Context, key string) (objstore.Object, error) {
	if err := validateKey(key); err != nil {
		return objstore.Object{}, fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isMissingObject(err) {
			return objstore.Object{}, objstore.ErrNotFound
		}
		return objstore.Object{}, withCredentialClass(fmt.Errorf("heading S3 object %q: %w", key, err))
	}
	object := objstore.Object{Key: key}
	if output.ContentLength != nil {
		object.Size = *output.ContentLength
	}
	if output.LastModified != nil {
		object.LastModified = *output.LastModified
	}
	for name, value := range output.Metadata {
		if strings.EqualFold(name, checksumMetadata) {
			object.SHA256 = strings.ToLower(value)
			break
		}
	}
	return object, nil
}

// Get opens a completed object's response body for streaming.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetFrom(ctx, key, 0)
}

// Delete removes key idempotently.
func (s *Store) Delete(ctx context.Context, key string) error {
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	if err := validateKey(key); err != nil {
		return fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil {
		return withCredentialClass(fmt.Errorf("deleting S3 object %q: %w", key, err))
	}
	return nil
}

// List returns completed objects with the requested key prefix.
func (s *Store) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	var objects []objstore.Object
	var token *string
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, withCredentialClass(fmt.Errorf("listing S3 objects with prefix %q: %w", prefix, err))
		}
		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}
			object := objstore.Object{Key: *item.Key}
			if item.Size != nil {
				object.Size = *item.Size
			}
			if item.LastModified != nil {
				object.LastModified = *item.LastModified
			}
			objects = append(objects, object)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		token = page.NextContinuationToken
		if token == nil || *token == "" {
			return nil, errors.New("S3 list response is truncated without a continuation token")
		}
	}
	return objects, nil
}

// Presign creates a GET or HEAD URL whose lifetime is bounded by SigV4's seven-day limit.
func (s *Store) Presign(ctx context.Context, key, method string, expires time.Duration, contentType, filename string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		return "", fmt.Errorf("cannot sign S3 method %q", method)
	}
	if expires <= 0 || expires > maxURLExpiry {
		return "", fmt.Errorf("S3 signed URL expiry %s is outside (0, %s]", expires, maxURLExpiry)
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if contentType != "" {
		input.ResponseContentType = aws.String(contentType)
	}
	if filename != "" {
		value := mime.FormatMediaType("inline", map[string]string{"filename": filename})
		input.ResponseContentDisposition = aws.String(value)
	}
	var request *v4.PresignedHTTPRequest
	var err error
	if method == http.MethodHead {
		request, err = s.presigner.PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: input.Bucket, Key: input.Key},
			func(options *s3.PresignOptions) { options.Expires = expires })
	} else {
		request, err = s.presigner.PresignGetObject(ctx, input, func(options *s3.PresignOptions) { options.Expires = expires })
	}
	if err != nil {
		return "", fmt.Errorf("presigning S3 GET for %q: %w", key, err)
	}
	return request.URL, nil
}

func (s *Store) recoverExisting(ctx context.Context, key string, size int64, wantSHA [32]byte) error {
	object, err := s.Head(ctx, key)
	if err != nil {
		return err
	}
	if object.Size != size || object.SHA256 != hex.EncodeToString(wantSHA[:]) {
		return fmt.Errorf("existing object has size %d and SHA-256 %q, want %d and %x: %w",
			object.Size, object.SHA256, size, wantSHA, objstore.ErrChecksumMismatch)
	}
	return nil
}

func multipartPartSize(size int64) int64 {
	minimumForCount := size / maxPartCount
	if size%maxPartCount != 0 {
		minimumForCount++
	}
	if minimumForCount < minPartSize {
		return minPartSize
	}
	return minimumForCount
}

func validateKey(key string) error {
	_, err := mediapath.Resolve("/rokuban-object-store", key)
	return err
}

func isMissingObject(err error) bool {
	code := apiErrorCode(err)
	return code == "NoSuchKey" || code == "NotFound" || code == "NoSuchObject"
}

func isNoSuchUpload(err error) bool { return apiErrorCode(err) == "NoSuchUpload" }

func isPreconditionFailed(err error) bool {
	return apiErrorCode(err) == "PreconditionFailed" || httpStatusCode(err) == http.StatusPreconditionFailed
}

// withCredentialClass は、資格情報が取れない・失効している・拒否された失敗に objstore.ErrCredentials を足す。
// 呼び出し側は errors.Is で、権限や接続の失敗と区別して数える。
func withCredentialClass(err error) error {
	if isCredentialFailure(err) {
		return errors.Join(objstore.ErrCredentials, err)
	}
	return err
}

func isCredentialFailure(err error) bool {
	var signing *v4.SigningError
	if errors.As(err, &signing) {
		return true
	}
	switch apiErrorCode(err) {
	case "ExpiredToken", "ExpiredTokenException", "InvalidToken", "InvalidAccessKeyId", "InvalidClientTokenId",
		"UnrecognizedClientException", "RequestExpired", "TokenRefreshRequired":
		return true
	default:
		return false
	}
}

func isConditionalUnsupported(err error) bool {
	switch apiErrorCode(err) {
	case "NotImplemented", "NotSupported", "InvalidRequest", "InvalidArgument":
		return true
	default:
		return httpStatusCode(err) == http.StatusNotImplemented
	}
}

func apiErrorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func httpStatusCode(err error) int {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

// putEmptyObject は 0 バイトの object を PutObject で公開する。条件付き書き込みに未対応なら条件を外して送り直す。
func (s *Store) putEmptyObject(ctx context.Context, key, contentType string, wantSHA [32]byte) error {
	emptySHA := sha256.Sum256(nil)
	if emptySHA != wantSHA {
		return objstore.ErrChecksumMismatch
	}
	newInput := func(conditional bool) *s3.PutObjectInput {
		input := &s3.PutObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(nil), ContentLength: aws.Int64(0),
			ContentType: aws.String(contentType), Metadata: map[string]string{checksumMetadata: hex.EncodeToString(wantSHA[:])},
		}
		if conditional {
			input.IfNoneMatch = aws.String("*")
		}
		return input
	}
	_, err := s.client.PutObject(ctx, newInput(true))
	if err != nil && isConditionalUnsupported(err) {
		// 呼び出し側は key を使い回さないので、既存の object が無い前提で条件を外す。
		_, err = s.client.PutObject(ctx, newInput(false))
	}
	if err != nil {
		if isPreconditionFailed(err) {
			return s.recoverExisting(ctx, key, 0, wantSHA)
		}
		if recoverErr := s.recoverExisting(ctx, key, 0, wantSHA); recoverErr == nil {
			return nil
		}
		return withCredentialClass(fmt.Errorf("putting empty S3 object %q: %w", key, err))
	}
	return nil
}

// startMultipart は ContentType と SHA-256 のメタデータを付けて multipart upload を始め、UploadId を返す。
func (s *Store) startMultipart(ctx context.Context, key, contentType string, wantSHA [32]byte) (string, error) {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
		Metadata:    map[string]string{checksumMetadata: hex.EncodeToString(wantSHA[:])},
	})
	if err != nil {
		return "", withCredentialClass(fmt.Errorf("starting S3 multipart upload for %q: %w", key, err))
	}
	if created.UploadId == nil || *created.UploadId == "" {
		return "", fmt.Errorf("starting S3 multipart upload for %q: server returned an empty upload ID", key)
	}
	return *created.UploadId, nil
}

// completeMultipart は条件付きで CompleteMultipartUpload を送り、条件に未対応のサーバーでは条件を外して送り直す。
func (s *Store) completeMultipart(ctx context.Context, key, uploadID string, parts []types.CompletedPart) error {
	input := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		IfNoneMatch:     aws.String("*"),
	}
	_, err := s.client.CompleteMultipartUpload(ctx, input)
	if err != nil && isConditionalUnsupported(err) {
		// 送信した入力は fake や呼び出し側が保持しうるので、条件だけ外したコピーで送り直す。
		unconditional := *input
		unconditional.IfNoneMatch = nil
		_, err = s.client.CompleteMultipartUpload(ctx, &unconditional)
	}
	return err
}

// abortMultipart は UploadId を 1 回 Abort する。途中で止まったパートが残りうるので、
// 呼び出し側は必要なら繰り返す。ctx が終わっていても Abort は試みる。
func (s *Store) abortMultipart(ctx context.Context, key, uploadID string) error {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	if err != nil && !isNoSuchUpload(err) {
		return fmt.Errorf("aborting S3 multipart upload for %q: %w", key, err)
	}
	return nil
}

// uploadState は s3store の再開状態。UploadId・パートの大きさ・送り終えたパートの一覧を DB に残す正の記録で、
// Complete は ListParts ではなくこの一覧で完了する。
type uploadState struct {
	Key         string       `json:"key"`
	Size        int64        `json:"size"`
	SHA256      string       `json:"sha256"`
	ContentType string       `json:"content_type"`
	UploadID    string       `json:"upload_id"`
	PartSize    int64        `json:"part_size"`
	Parts       []uploadPart `json:"parts"`
	Offset      int64        `json:"offset"`
	Hash        []byte       `json:"hash"`
}

// uploadPart は UploadPart が成功したパートの番号と ETag。
type uploadPart struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
}

// StartUpload は multipart upload を始め、パートの大きさを state に固定する。0 バイトなら UploadId を作らない。
func (s *Store) StartUpload(ctx context.Context, key string, size int64, wantSHA [32]byte, contentType string) ([]byte, error) {
	if s.readOnly {
		return nil, objstore.ErrReadOnly
	}
	if err := validateKey(key); err != nil {
		return nil, fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	if size < 0 {
		return nil, fmt.Errorf("negative S3 object size %d", size)
	}
	hashState, err := marshalHash(sha256.New())
	if err != nil {
		return nil, err
	}
	st := uploadState{
		Key: key, Size: size, SHA256: hex.EncodeToString(wantSHA[:]), ContentType: contentType, Hash: hashState,
	}
	if size > 0 {
		st.PartSize = multipartPartSize(size)
		if st.UploadID, err = s.startMultipart(ctx, key, contentType, wantSHA); err != nil {
			return nil, err
		}
	}
	return encodeUploadState(st)
}

// ContinueUpload は state の offset から、パートを 1 つずつ送る。UploadPart が成功したパートだけを
// 一覧・hash・offset に反映するので、返す state と offset は S3 に残ったパートと常に一致する。
func (s *Store) ContinueUpload(ctx context.Context, raw []byte, src io.Reader) ([]byte, int64, error) {
	st, err := decodeUploadState(raw)
	if err != nil {
		return raw, 0, err
	}
	if s.readOnly {
		return raw, st.Offset, objstore.ErrReadOnly
	}
	original := st.Offset
	h, err := restoreHash(st.Hash)
	if err != nil {
		return raw, original, err
	}
	if st.Offset < st.Size && (st.UploadID == "" || st.PartSize <= 0 || st.Offset != int64(len(st.Parts))*st.PartSize) {
		return raw, original, fmt.Errorf("S3 upload state for %q does not match its %d parts: %w", st.Key, len(st.Parts), objstore.ErrUploadInvalid)
	}
	for err == nil && st.Offset < st.Size {
		err = s.sendNextPart(ctx, &st, h, src)
	}
	if err == nil {
		err = checkNoExtraByte(ctx, src, st.Key, st.Size)
	}
	hashState, hashErr := marshalHash(h)
	if hashErr != nil {
		return raw, original, hashErr
	}
	st.Hash = hashState
	out, encErr := encodeUploadState(st)
	if encErr != nil {
		return raw, original, encErr
	}
	return out, st.Offset, err
}

// sendNextPart は次のパートを読んで送る。UploadPart が成功してから一覧・hash・offset を進める。
func (s *Store) sendNextPart(ctx context.Context, st *uploadState, h hash.Hash, src io.Reader) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("uploading S3 object %q: %w", st.Key, err)
	}
	number := int32(len(st.Parts) + 1)
	length := min(st.PartSize, st.Size-st.Offset)
	body := make([]byte, length)
	if n, err := io.ReadFull(src, body); err != nil {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("reading S3 multipart part %d for %q: %w", number, st.Key, ctx.Err())
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			return fmt.Errorf("S3 object source for %q ended after %d of %d bytes: %w", st.Key, st.Offset+int64(n), st.Size, objstore.ErrChecksumMismatch)
		default:
			return fmt.Errorf("reading S3 multipart part %d for %q: %w", number, st.Key, err)
		}
	}
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(s.bucket), Key: aws.String(st.Key), UploadId: aws.String(st.UploadID),
		PartNumber: aws.Int32(number), Body: bytes.NewReader(body), ContentLength: aws.Int64(length),
	})
	if err != nil {
		return withCredentialClass(fmt.Errorf("uploading S3 multipart part %d for %q: %w", number, st.Key, err))
	}
	if out.ETag == nil || *out.ETag == "" {
		return fmt.Errorf("uploading S3 multipart part %d for %q: server returned no ETag", number, st.Key)
	}
	st.Parts = append(st.Parts, uploadPart{Number: number, ETag: *out.ETag})
	_, _ = h.Write(body) // hash.Hash の Write は error を返さない。
	st.Offset += length
	return nil
}

// checkNoExtraByte は、size に達した後の src に余分なバイトが無いことを確かめる。
func checkNoExtraByte(ctx context.Context, src io.Reader, key string, size int64) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("checking S3 object source for %q: %w", key, err)
	}
	var extra [1]byte
	n, err := io.ReadFull(src, extra[:])
	if n != 0 {
		return fmt.Errorf("S3 object source for %q contains more than %d bytes: %w", key, size, objstore.ErrChecksumMismatch)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading S3 object source for %q: %w", key, err)
	}
	return nil
}

// CompleteUpload は state のパート一覧で multipart upload を完了する。
// 送りながら計算した SHA-256 が一致したときだけ公開し、同じ key に同じ内容があれば成功、違えば ErrAlreadyExists。
// 412 で既存を採用したときは UploadId を Abort する。
func (s *Store) CompleteUpload(ctx context.Context, raw []byte) error {
	if s.readOnly {
		return objstore.ErrReadOnly
	}
	st, err := decodeUploadState(raw)
	if err != nil {
		return err
	}
	if st.Offset != st.Size {
		return fmt.Errorf("S3 upload for %q has %d of %d bytes", st.Key, st.Offset, st.Size)
	}
	want, err := decodeSHA256(st.SHA256)
	if err != nil {
		return err
	}
	h, err := restoreHash(st.Hash)
	if err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return fmt.Errorf("S3 object %q: %w", st.Key, objstore.ErrChecksumMismatch)
	}
	if err := s.existingObject(ctx, st); err == nil || !errors.Is(err, objstore.ErrNotFound) {
		return err
	}
	if st.UploadID == "" {
		return s.putEmptyObject(ctx, st.Key, st.ContentType, want)
	}
	if st.PartSize <= 0 || int64(len(st.Parts)) != (st.Size+st.PartSize-1)/st.PartSize {
		return fmt.Errorf("S3 upload for %q has %d parts for %d bytes: %w", st.Key, len(st.Parts), st.Size, objstore.ErrUploadInvalid)
	}
	parts := make([]types.CompletedPart, 0, len(st.Parts))
	for _, part := range st.Parts {
		parts = append(parts, types.CompletedPart{ETag: aws.String(part.ETag), PartNumber: aws.Int32(part.Number)})
	}
	err = s.completeMultipart(ctx, st.Key, st.UploadID, parts)
	switch {
	case err == nil:
		return nil
	case isPreconditionFailed(err):
		if err := s.existingObject(ctx, st); err != nil {
			return err
		}
		return s.abortMultipart(ctx, st.Key, st.UploadID)
	case s.existingObject(ctx, st) == nil:
		// 応答を失っただけで完了していた。HEAD で同じ内容なら成功とする。
		return nil
	case isUploadGone(err):
		return fmt.Errorf("completing S3 multipart upload for %q: %w: %w", st.Key, objstore.ErrUploadInvalid, err)
	default:
		return withCredentialClass(fmt.Errorf("completing S3 multipart upload for %q: %w", st.Key, err))
	}
}

// AbortUpload は UploadId を 1 回 Abort する。NoSuchUpload なら gone、成功なら送信中のパートが残りうるので gone は false。
func (s *Store) AbortUpload(ctx context.Context, raw []byte) (bool, error) {
	st, err := decodeUploadState(raw)
	if err != nil {
		return false, err
	}
	if st.UploadID == "" {
		return true, nil
	}
	_, err = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(st.Key), UploadId: aws.String(st.UploadID),
	})
	switch {
	case err == nil:
		return false, nil
	case isNoSuchUpload(err):
		return true, nil
	default:
		return false, withCredentialClass(fmt.Errorf("aborting S3 multipart upload for %q: %w", st.Key, err))
	}
}

// GetFrom は completed object の offset から読める body を返す。offset が 0 のときは Range を付けない。
func (s *Store) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, fmt.Errorf("invalid S3 object key %q: %w", key, err)
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative read offset %d", offset)
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if offset > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	output, err := s.client.GetObject(ctx, input)
	if err != nil {
		if isMissingObject(err) {
			return nil, objstore.ErrNotFound
		}
		return nil, withCredentialClass(fmt.Errorf("getting S3 object %q: %w", key, err))
	}
	if output.Body == nil {
		return nil, fmt.Errorf("getting S3 object %q: server returned an empty body", key)
	}
	return output.Body, nil
}

// existingObject は key の object が st と同じ内容なら nil、違えば ErrAlreadyExists、無ければ ErrNotFound を返す。
func (s *Store) existingObject(ctx context.Context, st uploadState) error {
	object, err := s.Head(ctx, st.Key)
	if err != nil {
		return err
	}
	if object.Size != st.Size || object.SHA256 != st.SHA256 {
		return fmt.Errorf("S3 object %q already exists with different content: %w", st.Key, objstore.ErrAlreadyExists)
	}
	return nil
}

func decodeUploadState(raw []byte) (uploadState, error) {
	var st uploadState
	if err := json.Unmarshal(raw, &st); err != nil {
		return uploadState{}, fmt.Errorf("decoding S3 upload state: %w: %w", objstore.ErrUploadInvalid, err)
	}
	if st.Key == "" || st.Size < 0 || st.Offset < 0 || st.Offset > st.Size || (st.Size > 0 && st.UploadID == "") {
		return uploadState{}, fmt.Errorf("S3 upload state is inconsistent: %w", objstore.ErrUploadInvalid)
	}
	return st, nil
}

func encodeUploadState(st uploadState) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("encoding S3 upload state: %w", err)
	}
	return raw, nil
}

func decodeSHA256(hexSHA string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(hexSHA)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("S3 upload state has an invalid SHA-256 %q: %w", hexSHA, objstore.ErrUploadInvalid)
	}
	copy(out[:], raw)
	return out, nil
}

func marshalHash(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("hash %T cannot be marshaled", h)
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshaling SHA-256 state: %w", err)
	}
	return state, nil
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("hash %T cannot be unmarshaled", h)
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, fmt.Errorf("restoring SHA-256 state: %w: %w", objstore.ErrUploadInvalid, err)
	}
	return h, nil
}

func isUploadGone(err error) bool {
	switch apiErrorCode(err) {
	case "InvalidPart", "InvalidPartOrder", "NoSuchUpload":
		return true
	default:
		return false
	}
}

var _ objstore.Resumable = (*Store)(nil)
var _ objstore.RangeReader = (*Store)(nil)

var _ objstore.Store = (*Store)(nil)
var _ objstore.SignedURL = (*Store)(nil)
