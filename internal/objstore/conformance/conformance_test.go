//go:build s3conformance

package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/google/uuid"
)

const (
	statusSupported   = "supported"
	statusUnsupported = "unsupported"
	statusPartial     = "partial"
	// statusUnknown は、想定外のエラーで判定できなかった機能の記録。
	statusUnknown = "unknown"

	partSize = 5 << 20
)

const (
	featureConditionalWrites = "conditional-writes"
	featureMultipart         = "multipart-upload-and-abort"
	featureHeadChecksum      = "head-checksum-sha256"
	featurePresignedRange    = "presigned-get-range"
)

var featureNames = []string{featureConditionalWrites, featureMultipart, featureHeadChecksum, featurePresignedRange}

type featureResult struct {
	Feature string            `json:"feature"`
	Status  string            `json:"status"`
	Checks  map[string]string `json:"checks,omitempty"`
	Note    string            `json:"note,omitempty"`
}

type testConfig struct {
	endpoint     string
	region       string
	bucket       string
	access       string
	secret       string
	createBucket bool
	// expect は機能名ごとに期待する status。空なら照合しない。
	expect map[string]string
}

// target は 1 つの S3 endpoint と、そこに作るオブジェクトの置き場所である。
type target struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
	prefix    string
}

// TestS3Conformance measures the S3 API behaviors used by the storage adapter.
// It only records each feature's status; it fails only when
// ROKUBAN_S3_CONFORMANCE_EXPECT is set and a recorded status differs from it.
func TestS3Conformance(t *testing.T) {
	cfg := readTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tg := newTarget(t, ctx, cfg)

	probes := []struct {
		name string
		run  func(*testing.T) (featureResult, error)
	}{
		{featureConditionalWrites, func(t *testing.T) (featureResult, error) { return probeConditionalWrites(t, ctx, tg) }},
		{featureMultipart, func(t *testing.T) (featureResult, error) { return probeMultipart(t, ctx, tg) }},
		{featureHeadChecksum, func(t *testing.T) (featureResult, error) { return probeHeadChecksum(t, ctx, tg) }},
		{featurePresignedRange, func(t *testing.T) (featureResult, error) { return probePresignedRange(t, ctx, tg) }},
	}
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			result, err := probe.run(t)
			if err != nil {
				// 判定できなかったことも記録だけにする。失敗にするのは期待値との不一致だけ。
				result = featureResult{Feature: probe.name, Status: statusUnknown, Note: err.Error()}
			}
			logResult(t, result)
			if err := expectationError(result, cfg.expect); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestConditionalWritesUnsupportedIsRecorded は、条件付き書き込みに対応しない
// サーバーが、実 API の判定経路でも失敗にならず unsupported と記録されることを確かめる。
// SigV4 は If-None-Match も署名対象なので、署名の前に SDK の middleware でヘッダを外す
// （条件を無視するサーバー）。NotImplemented を返すサーバーは、実 endpoint の前に置く
// 小さな proxy で作る。
func TestConditionalWritesUnsupportedIsRecorded(t *testing.T) {
	cfg := readTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	base := newTarget(t, ctx, cfg)

	t.Run("condition ignored", func(t *testing.T) {
		tg := base.withClient(newClient(cfg, cfg.endpoint, func(options *s3.Options) {
			options.APIOptions = append(options.APIOptions, func(stack *middleware.Stack) error {
				return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("DropIfNoneMatch",
					func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
						if req, ok := in.Request.(*smithyhttp.Request); ok {
							req.Header.Del("If-None-Match")
						}
						return next.HandleFinalize(ctx, in)
					}), "Signing", middleware.Before)
			})
		}))
		requireConditionalUnsupported(t, ctx, tg)
	})

	t.Run("NotImplemented response", func(t *testing.T) {
		upstream, err := url.Parse(cfg.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		reverse := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = r.In.Host // 署名済みの Host を保つ
		}}
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") != "" {
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NotImplemented</Code><Message>stub</Message></Error>`)
				return
			}
			reverse.ServeHTTP(w, r)
		}))
		t.Cleanup(proxy.Close) // オブジェクトの削除（proxy 経由）より後に閉じる
		requireConditionalUnsupported(t, ctx, base.withClient(newClient(cfg, proxy.URL)))
	})
}

func requireConditionalUnsupported(t *testing.T, ctx context.Context, tg target) {
	t.Helper()
	result, err := probeConditionalWrites(t, ctx, tg)
	if err != nil {
		t.Fatalf("unsupported conditional writes must be recorded, not fail: %v", err)
	}
	if result.Status != statusUnsupported {
		t.Fatalf("status = %q, want %q (checks %v)", result.Status, statusUnsupported, result.Checks)
	}
	logResult(t, result)
}

func (tg target) withClient(client *s3.Client) target {
	tg.client = client
	tg.presigner = s3.NewPresignClient(client)
	return tg
}

func newClient(cfg testConfig, endpoint string, opts ...func(*s3.Options)) *s3.Client {
	sdkConfig := aws.Config{
		Region:      cfg.region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.access, cfg.secret, ""),
		HTTPClient:  http.DefaultClient,
	}
	return s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		options.BaseEndpoint = stringPointer(endpoint)
		options.UsePathStyle = true
	}, func(options *s3.Options) {
		for _, opt := range opts {
			opt(options)
		}
	})
}

func newTarget(t *testing.T, ctx context.Context, cfg testConfig) target {
	t.Helper()
	client := newClient(cfg, cfg.endpoint)
	if cfg.createBucket {
		createBucket := &s3.CreateBucketInput{Bucket: stringPointer(cfg.bucket)}
		if cfg.region != "us-east-1" {
			createBucket.CreateBucketConfiguration = &types.CreateBucketConfiguration{
				LocationConstraint: types.BucketLocationConstraint(cfg.region),
			}
		}
		if _, err := client.CreateBucket(ctx, createBucket); err != nil && apiErrorCode(err) != "BucketAlreadyOwnedByYou" {
			t.Fatalf("create conformance bucket: %v", err)
		}
	}
	return target{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.bucket,
		prefix:    "rokuban-conformance/" + uuid.NewString(),
	}
}

func readTestConfig(t *testing.T) testConfig {
	t.Helper()
	cfg := testConfig{
		endpoint:     strings.TrimRight(strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")), "/"),
		region:       strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION")),
		bucket:       strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET")),
		access:       strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_ACCESS_KEY_ID")),
		secret:       strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_SECRET_ACCESS_KEY")),
		createBucket: strings.EqualFold(strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_CREATE_BUCKET")), "true"),
	}
	if cfg.endpoint == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT is not set")
	}
	parsed, err := url.ParseRequestURI(cfg.endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		t.Fatal("ROKUBAN_S3_CONFORMANCE_ENDPOINT must be an http(s) URL")
	}
	if cfg.region == "" || cfg.bucket == "" || cfg.access == "" || cfg.secret == "" {
		t.Fatal("S3 conformance requires region, bucket, access key ID, and secret access key")
	}
	if cfg.expect, err = parseExpectations(os.Getenv("ROKUBAN_S3_CONFORMANCE_EXPECT")); err != nil {
		t.Fatalf("ROKUBAN_S3_CONFORMANCE_EXPECT: %v", err)
	}
	return cfg
}

func logResult(t *testing.T, result featureResult) {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode conformance result: %v", err)
	}
	t.Logf("S3_CONFORMANCE %s", encoded)
}

// parseExpectations は "feature=status,feature=status" 形式の期待値を読む。空なら期待値なし。
func parseExpectations(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	expect := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		feature, status, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || !slices.Contains(featureNames, feature) {
			return nil, fmt.Errorf("%q is not feature=status with a known feature (%s)", item, strings.Join(featureNames, ", "))
		}
		if !slices.Contains([]string{statusSupported, statusUnsupported, statusPartial}, status) {
			return nil, fmt.Errorf("%q has unknown status %q", item, status)
		}
		expect[feature] = status
	}
	return expect, nil
}

// expectationError は、期待値が指定された機能の記録が期待と違うときだけ error を返す。
func expectationError(result featureResult, expect map[string]string) error {
	want, ok := expect[result.Feature]
	if !ok || result.Status == want {
		return nil
	}
	return fmt.Errorf("feature %q is %s, expected %s", result.Feature, result.Status, want)
}

func combineStatuses(statuses ...string) string {
	if len(statuses) == 0 {
		return statusUnsupported
	}
	allSupported := true
	allUnsupported := true
	for _, status := range statuses {
		allSupported = allSupported && status == statusSupported
		allUnsupported = allUnsupported && status == statusUnsupported
	}
	if allSupported {
		return statusSupported
	}
	if allUnsupported {
		return statusUnsupported
	}
	return statusPartial
}

func probeConditionalWrites(t *testing.T, ctx context.Context, tg target) (featureResult, error) {
	client, bucket := tg.client, tg.bucket
	putKey := tg.prefix + "/conditional-put"
	registerObjectCleanup(t, client, bucket, putKey)
	putStatus, err := conditionalWriteStatus("conditional PutObject", func() error {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      stringPointer(bucket),
			Key:         stringPointer(putKey),
			Body:        bytes.NewReader([]byte("conditional-write-probe")),
			IfNoneMatch: stringPointer("*"),
		})
		return err
	})
	if err != nil {
		return featureResult{}, err
	}
	multipartKey := tg.prefix + "/conditional-multipart"
	registerObjectCleanup(t, client, bucket, multipartKey)
	body := makeMultipartPart(1)
	multipartStatus, err := conditionalWriteStatus("conditional CompleteMultipartUpload", func() error {
		_, err := uploadMultipart(ctx, client, bucket, multipartKey, [][]byte{body}, true)
		return err
	})
	if err != nil {
		return featureResult{}, err
	}
	return featureResult{
		Feature: featureConditionalWrites,
		Status:  combineStatuses(putStatus, multipartStatus),
		Checks: map[string]string{
			"put-object-if-none-match":         putStatus,
			"complete-multipart-if-none-match": multipartStatus,
		},
		Note: "unsupported is non-fatal because every write uses a unique object key",
	}, nil
}

// conditionalWriteStatus は、同じキーへの If-None-Match: * 付き書き込みを 2 回行い、
// 条件付き書き込みの対応を判定する。1 回目が成功し 2 回目が PreconditionFailed なら対応、
// 2 回目も成功（条件の無視）や NotImplemented 系なら非対応。
func conditionalWriteStatus(operation string, write func() error) (string, error) {
	if err := write(); err != nil {
		if isUnsupportedFeature(err) {
			return statusUnsupported, nil
		}
		return "", unexpectedS3Error(operation, err)
	}
	err := write()
	switch {
	case err == nil, isUnsupportedFeature(err):
		return statusUnsupported, nil
	case isPreconditionFailed(err):
		return statusSupported, nil
	}
	return "", unexpectedS3Error(operation+" retry", err)
}

func probeMultipart(t *testing.T, ctx context.Context, tg target) (featureResult, error) {
	client, bucket := tg.client, tg.bucket
	completeKey := tg.prefix + "/multipart-complete"
	registerObjectCleanup(t, client, bucket, completeKey)
	firstPart := makeMultipartPart(1)
	secondPart := makeMultipartPart(2)
	body := append(firstPart, secondPart...)
	started, err := uploadMultipart(ctx, client, bucket, completeKey, [][]byte{body[:partSize], body[partSize:]}, false)
	if err != nil {
		if !isUnsupportedFeature(err) {
			return featureResult{}, unexpectedS3Error("multipart upload and complete", err)
		}
		return probeMultipartAbort(t, ctx, tg, statusAfterUnsupported(started))
	}
	output, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: stringPointer(bucket), Key: stringPointer(completeKey)})
	if err != nil {
		if isUnsupportedFeature(err) {
			return probeMultipartAbort(t, ctx, tg, statusPartial)
		}
		return featureResult{}, unexpectedS3Error("read completed multipart object", err)
	}
	got, readErr := io.ReadAll(output.Body)
	closeErr := output.Body.Close()
	if readErr != nil {
		return featureResult{}, fmt.Errorf("read completed multipart object body: %w", readErr)
	}
	if closeErr != nil {
		return featureResult{}, fmt.Errorf("close completed multipart object body: %w", closeErr)
	}
	if !bytes.Equal(got, body) {
		return featureResult{}, errors.New("completed multipart object differs from uploaded parts")
	}
	return probeMultipartAbort(t, ctx, tg, statusSupported)
}

func probeMultipartAbort(t *testing.T, ctx context.Context, tg target, uploadStatus string) (featureResult, error) {
	client, bucket := tg.client, tg.bucket
	key := tg.prefix + "/multipart-abort"
	registerObjectCleanup(t, client, bucket, key)
	upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: stringPointer(bucket),
		Key:    stringPointer(key),
	})
	if err != nil {
		if isUnsupportedFeature(err) {
			return featureResult{
				Feature: featureMultipart,
				Status:  combineStatuses(uploadStatus, statusUnsupported),
				Checks:  map[string]string{"upload-and-complete": uploadStatus, "abort": statusUnsupported},
			}, nil
		}
		return featureResult{}, unexpectedS3Error("CreateMultipartUpload for abort", err)
	}
	if upload.UploadId == nil {
		return featureResult{}, errors.New("CreateMultipartUpload returned no upload ID")
	}

	_, uploadPartErr := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     stringPointer(bucket),
		Key:        stringPointer(key),
		UploadId:   upload.UploadId,
		PartNumber: int32Pointer(1),
		Body:       bytes.NewReader(makeMultipartPart(3)),
	})
	if uploadPartErr != nil && !isUnsupportedFeature(uploadPartErr) {
		abortMultipart(ctx, client, bucket, key, *upload.UploadId)
		return featureResult{}, unexpectedS3Error("UploadPart before abort", uploadPartErr)
	}

	_, abortErr := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   stringPointer(bucket),
		Key:      stringPointer(key),
		UploadId: upload.UploadId,
	})
	var abortStatus string
	if abortErr != nil {
		if !isUnsupportedFeature(abortErr) {
			return featureResult{}, unexpectedS3Error("AbortMultipartUpload", abortErr)
		}
		abortStatus = statusUnsupported
	} else {
		parts, listErr := client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   stringPointer(bucket),
			Key:      stringPointer(key),
			UploadId: upload.UploadId,
		})
		if abortStatus, err = abortedStatus(parts, listErr); err != nil {
			return featureResult{}, err
		}
	}

	return featureResult{
		Feature: featureMultipart,
		Status:  combineStatuses(uploadStatus, abortStatus),
		Checks: map[string]string{
			"upload-and-complete": uploadStatus,
			"abort":               abortStatus,
		},
	}, nil
}

// abortedStatus は abort 後の ListParts の結果から abort の対応を判定する。
// AWS の AbortMultipartUpload の API リファレンスが示す確認方法は、ListParts の
// parts が空であること。NoSuchUpload も、upload が残っていない証拠として対応とする。
func abortedStatus(parts *s3.ListPartsOutput, listErr error) (string, error) {
	switch {
	case isNoSuchUpload(listErr):
		return statusSupported, nil
	case listErr != nil:
		return "", unexpectedS3Error("verify aborted multipart upload", listErr)
	case len(parts.Parts) == 0:
		return statusSupported, nil
	}
	return statusUnsupported, nil
}

func probeHeadChecksum(t *testing.T, ctx context.Context, tg target) (featureResult, error) {
	client, bucket := tg.client, tg.bucket
	key := tg.prefix + "/head-checksum"
	registerObjectCleanup(t, client, bucket, key)
	body := []byte("whole-object-sha256-checksum-probe")
	digest := sha256.Sum256(body)
	checksum := base64.StdEncoding.EncodeToString(digest[:])
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            stringPointer(bucket),
		Key:               stringPointer(key),
		Body:              bytes.NewReader(body),
		ChecksumSHA256:    stringPointer(checksum),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		if isUnsupportedFeature(err) {
			return featureResult{Feature: featureHeadChecksum, Status: statusUnsupported}, nil
		}
		return featureResult{}, unexpectedS3Error("PutObject with SHA-256 checksum", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       stringPointer(bucket),
		Key:          stringPointer(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		if isUnsupportedFeature(err) {
			return featureResult{Feature: featureHeadChecksum, Status: statusUnsupported}, nil
		}
		return featureResult{}, unexpectedS3Error("HeadObject checksum", err)
	}
	if head.ChecksumSHA256 == nil {
		return featureResult{Feature: featureHeadChecksum, Status: statusUnsupported}, nil
	}
	if *head.ChecksumSHA256 != checksum {
		return featureResult{}, errors.New("HeadObject returned a SHA-256 checksum different from the PutObject checksum")
	}
	return featureResult{
		Feature: featureHeadChecksum,
		Status:  statusSupported,
		Note:    "checked with single-part PutObject; a multipart SHA-256 is always a composite value and is not compared with the full-object digest",
	}, nil
}

// probePresignedRange は、Range を署名に含めずに作った署名付き URL へ、
// 署名後にクライアントが任意の Range を付けて読めるかを測る（302 先のブラウザや転送の読み方）。
func probePresignedRange(t *testing.T, ctx context.Context, tg target) (featureResult, error) {
	client, bucket := tg.client, tg.bucket
	key := tg.prefix + "/presigned-range"
	registerObjectCleanup(t, client, bucket, key)
	body := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: stringPointer(bucket),
		Key:    stringPointer(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		return featureResult{}, unexpectedS3Error("PutObject for presigned GET", err)
	}
	presigned, err := tg.presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: stringPointer(bucket), Key: stringPointer(key)},
		func(options *s3.PresignOptions) { options.Expires = 2 * time.Minute })
	if err != nil {
		return featureResult{}, fmt.Errorf("presign GET: %w", err)
	}

	full, err := doPresignedGet(ctx, presigned, "")
	if err != nil {
		return featureResult{}, err
	}
	if full.StatusCode == http.StatusNotImplemented {
		return featureResult{
			Feature: featurePresignedRange,
			Status:  statusUnsupported,
			Checks:  map[string]string{"presigned-get": statusUnsupported},
		}, nil
	}
	if full.StatusCode != http.StatusOK || !bytes.Equal(full.Body, body) {
		return featureResult{}, fmt.Errorf("presigned GET returned HTTP %d or an unexpected body", full.StatusCode)
	}
	checks := map[string]string{"presigned-get": statusSupported}
	for name, spec := range map[string]struct {
		header string
		want   []byte
		cr     string
	}{
		"range-closed":     {"bytes=3-9", body[3:10], "bytes 3-9/36"},
		"range-open-ended": {"bytes=30-", body[30:], "bytes 30-35/36"},
	} {
		response, err := doPresignedGet(ctx, presigned, spec.header)
		if err != nil {
			return featureResult{}, err
		}
		checks[name] = rangeStatus(response, spec.want, spec.cr)
	}
	statuses := make([]string, 0, len(checks))
	for _, status := range checks {
		statuses = append(statuses, status)
	}
	return featureResult{
		Feature: featurePresignedRange,
		Status:  combineStatuses(statuses...),
		Checks:  checks,
		Note:    "Range is added by the client after signing and is not part of the signature",
	}, nil
}

func rangeStatus(response presignedResponse, wantBody []byte, wantContentRange string) string {
	if response.StatusCode == http.StatusPartialContent && bytes.Equal(response.Body, wantBody) && response.ContentRange == wantContentRange {
		return statusSupported
	}
	return statusUnsupported
}

// doPresignedGet は署名付き URL を GET する。URL は署名を含むので、
// このエラーには URL を載せない。
func doPresignedGet(ctx context.Context, presigned *v4.PresignedHTTPRequest, rangeHeader string) (presignedResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, presigned.URL, nil)
	if err != nil {
		return presignedResponse{}, errors.New("construct request for presigned GET")
	}
	for name, values := range presigned.SignedHeader {
		if strings.EqualFold(name, "host") {
			continue
		}
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return presignedResponse{}, fmt.Errorf("send presigned GET: %w", urlErr.Err)
		}
		return presignedResponse{}, fmt.Errorf("send presigned GET: %T", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return presignedResponse{}, fmt.Errorf("read presigned GET response: %w", readErr)
	}
	if closeErr != nil {
		return presignedResponse{}, fmt.Errorf("close presigned GET response: %w", closeErr)
	}
	return presignedResponse{
		StatusCode:   response.StatusCode,
		Body:         body,
		ContentRange: response.Header.Get("Content-Range"),
	}, nil
}

type presignedResponse struct {
	StatusCode   int
	Body         []byte
	ContentRange string
}

func uploadMultipart(ctx context.Context, client *s3.Client, bucket, key string, parts [][]byte, conditional bool) (started bool, retErr error) {
	input := &s3.CreateMultipartUploadInput{Bucket: stringPointer(bucket), Key: stringPointer(key)}
	upload, err := client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return false, err
	}
	started = true
	if upload.UploadId == nil {
		return true, errors.New("CreateMultipartUpload returned no upload ID")
	}
	completed := false
	defer func() {
		if !completed {
			abortMultipart(ctx, client, bucket, key, *upload.UploadId)
		}
	}()

	completedParts := make([]types.CompletedPart, 0, len(parts))
	for index, part := range parts {
		partNumber := int32(index + 1)
		output, uploadErr := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     stringPointer(bucket),
			Key:        stringPointer(key),
			UploadId:   upload.UploadId,
			PartNumber: int32Pointer(partNumber),
			Body:       bytes.NewReader(part),
		})
		if uploadErr != nil {
			return true, uploadErr
		}
		if output.ETag == nil {
			return true, errors.New("UploadPart returned no ETag")
		}
		completedParts = append(completedParts, types.CompletedPart{
			ETag:       output.ETag,
			PartNumber: int32Pointer(partNumber),
		})
	}
	complete := &s3.CompleteMultipartUploadInput{
		Bucket:          stringPointer(bucket),
		Key:             stringPointer(key),
		UploadId:        upload.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completedParts},
	}
	if conditional {
		complete.IfNoneMatch = stringPointer("*")
	}
	if _, err := client.CompleteMultipartUpload(ctx, complete); err != nil {
		return true, err
	}
	completed = true
	return true, nil
}

func abortMultipart(ctx context.Context, client *s3.Client, bucket, key, uploadID string) {
	_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   stringPointer(bucket),
		Key:      stringPointer(key),
		UploadId: stringPointer(uploadID),
	})
}

func makeMultipartPart(seed byte) []byte {
	body := make([]byte, partSize)
	for index := range body {
		body[index] = byte((index*31+index/251)%251) ^ seed
	}
	return body
}

func statusAfterUnsupported(started bool) string {
	if started {
		return statusPartial
	}
	return statusUnsupported
}

func registerObjectCleanup(t *testing.T, client *s3.Client, bucket, key string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: stringPointer(bucket),
			Key:    stringPointer(key),
		}); err != nil {
			t.Errorf("delete conformance object %q: %v", key, err)
		}
	})
}

func unexpectedS3Error(operation string, err error) error {
	return fmt.Errorf("%s returned unexpected S3 error (code %q): %w", operation, apiErrorCode(err), err)
}

func apiErrorCode(err error) string {
	var apiErr interface {
		error
		ErrorCode() string
	}
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return "unknown"
}

func stringPointer(value string) *string {
	return &value
}

func int32Pointer(value int32) *int32 {
	return &value
}

func isPreconditionFailed(err error) bool {
	return apiErrorCode(err) == "PreconditionFailed"
}

func isNoSuchUpload(err error) bool {
	return apiErrorCode(err) == "NoSuchUpload"
}

func isUnsupportedFeature(err error) bool {
	switch apiErrorCode(err) {
	case "NotImplemented", "NotSupported", "UnsupportedHeader", "UnsupportedOperation":
		return true
	default:
		return false
	}
}
