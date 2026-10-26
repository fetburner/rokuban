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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

const (
	statusSupported   = "supported"
	statusUnsupported = "unsupported"
	statusPartial     = "partial"

	partSize = 5 << 20
)

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
}

// TestS3Conformance measures the S3 API behaviors used by the storage adapter.
func TestS3Conformance(t *testing.T) {
	cfg := readTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	sdkConfig := aws.Config{
		Region:      cfg.region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.access, cfg.secret, ""),
		HTTPClient:  http.DefaultClient,
	}
	client := s3.NewFromConfig(sdkConfig, func(options *s3.Options) {
		options.BaseEndpoint = stringPointer(cfg.endpoint)
		options.UsePathStyle = true
	})
	if cfg.createBucket {
		createBucket := &s3.CreateBucketInput{Bucket: stringPointer(cfg.bucket)}
		if cfg.region != "us-east-1" {
			createBucket.CreateBucketConfiguration = &types.CreateBucketConfiguration{
				LocationConstraint: types.BucketLocationConstraint(cfg.region),
			}
		}
		if _, err := client.CreateBucket(ctx, createBucket); err != nil && apiErrorCode(err) != "BucketAlreadyOwnedByYou" {
			t.Fatalf("create conformance bucket: %s", apiErrorCode(err))
		}
	}
	presigner := s3.NewPresignClient(client)
	prefix := "rokuban-conformance/" + uuid.NewString()

	probes := []struct {
		name     string
		required bool
		run      func(*testing.T) (featureResult, error)
	}{
		{
			name: "conditional-writes",
			run: func(t *testing.T) (featureResult, error) {
				return probeConditionalWrites(t, ctx, client, cfg.bucket, prefix)
			},
		},
		{
			name:     "multipart-upload-and-abort",
			required: true,
			run: func(t *testing.T) (featureResult, error) {
				return probeMultipart(t, ctx, client, cfg.bucket, prefix)
			},
		},
		{
			name:     "head-checksum-sha256",
			required: true,
			run: func(t *testing.T) (featureResult, error) {
				return probeHeadChecksum(t, ctx, client, cfg.bucket, prefix)
			},
		},
		{
			name:     "presigned-get-range",
			required: true,
			run: func(t *testing.T) (featureResult, error) {
				return probePresignedRange(t, ctx, client, presigner, cfg.bucket, prefix)
			},
		},
	}

	for _, probe := range probes {
		probe := probe
		t.Run(probe.name, func(t *testing.T) {
			result, err := probe.run(t)
			if err != nil {
				t.Fatalf("probe failed: %v", err)
			}
			logResult(t, result)
			if err := requiredFeatureError(result, probe.required); err != nil {
				t.Error(err)
			}
		})
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

func requiredFeatureError(result featureResult, required bool) error {
	if !required || result.Status == statusSupported {
		return nil
	}
	return fmt.Errorf("required S3 feature %q is %s", result.Feature, result.Status)
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

func probeConditionalWrites(t *testing.T, ctx context.Context, client *s3.Client, bucket, prefix string) (featureResult, error) {
	putKey := prefix + "/conditional-put"
	registerObjectCleanup(t, client, bucket, putKey)
	putStatus, err := probeConditionalPut(ctx, client, bucket, putKey)
	if err != nil {
		return featureResult{}, err
	}
	multipartKey := prefix + "/conditional-multipart"
	registerObjectCleanup(t, client, bucket, multipartKey)
	multipartStatus, err := probeConditionalMultipart(ctx, client, bucket, multipartKey)
	if err != nil {
		return featureResult{}, err
	}
	return featureResult{
		Feature: "conditional-writes",
		Status:  combineStatuses(putStatus, multipartStatus),
		Checks: map[string]string{
			"put-object-if-none-match":         putStatus,
			"complete-multipart-if-none-match": multipartStatus,
		},
		Note: "unsupported is non-fatal because every write uses a unique object key",
	}, nil
}

func probeConditionalPut(ctx context.Context, client *s3.Client, bucket, key string) (string, error) {
	body := []byte("conditional-write-probe")
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      stringPointer(bucket),
		Key:         stringPointer(key),
		Body:        bytes.NewReader(body),
		IfNoneMatch: stringPointer("*"),
	})
	if err != nil {
		return unsupportedOrError("conditional PutObject", err)
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      stringPointer(bucket),
		Key:         stringPointer(key),
		Body:        bytes.NewReader([]byte("second-write")),
		IfNoneMatch: stringPointer("*"),
	})
	return conditionalRetryStatus(err)
}

func probeConditionalMultipart(ctx context.Context, client *s3.Client, bucket, key string) (string, error) {
	body := makeMultipartPart(1)
	started, err := uploadMultipart(ctx, client, bucket, key, [][]byte{body}, true)
	if err != nil {
		if isUnsupportedFeature(err) {
			return statusAfterUnsupported(started), nil
		}
		return "", unexpectedS3Error("conditional CompleteMultipartUpload", err)
	}
	started, err = uploadMultipart(ctx, client, bucket, key, [][]byte{body}, true)
	if err == nil {
		return statusUnsupported, nil
	}
	if isPreconditionFailed(err) {
		return statusSupported, nil
	}
	if isUnsupportedFeature(err) {
		return statusAfterUnsupported(started), nil
	}
	return "", unexpectedS3Error("conditional CompleteMultipartUpload retry", err)
}

func conditionalRetryStatus(err error) (string, error) {
	if err == nil {
		return statusUnsupported, nil
	}
	if isPreconditionFailed(err) {
		return statusSupported, nil
	}
	if isUnsupportedFeature(err) {
		return statusUnsupported, nil
	}
	return "", unexpectedS3Error("conditional PutObject retry", err)
}

func probeMultipart(t *testing.T, ctx context.Context, client *s3.Client, bucket, prefix string) (featureResult, error) {
	completeKey := prefix + "/multipart-complete"
	registerObjectCleanup(t, client, bucket, completeKey)
	firstPart := makeMultipartPart(1)
	secondPart := makeMultipartPart(2)
	body := append(firstPart, secondPart...)
	started, err := uploadMultipart(ctx, client, bucket, completeKey, [][]byte{body[:partSize], body[partSize:]}, false)
	if err != nil {
		if !isUnsupportedFeature(err) {
			return featureResult{}, unexpectedS3Error("multipart upload and complete", err)
		}
		return probeMultipartAbort(t, ctx, client, bucket, prefix, statusAfterUnsupported(started))
	}
	output, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: stringPointer(bucket), Key: stringPointer(completeKey)})
	if err != nil {
		if isUnsupportedFeature(err) {
			return probeMultipartAbort(t, ctx, client, bucket, prefix, statusPartial)
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
	return probeMultipartAbort(t, ctx, client, bucket, prefix, statusSupported)
}

func probeMultipartAbort(t *testing.T, ctx context.Context, client *s3.Client, bucket, prefix string, uploadStatus string) (featureResult, error) {
	key := prefix + "/multipart-abort"
	registerObjectCleanup(t, client, bucket, key)
	upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: stringPointer(bucket),
		Key:    stringPointer(key),
	})
	if err != nil {
		if isUnsupportedFeature(err) {
			return featureResult{
				Feature: "multipart-upload-and-abort",
				Status:  combineStatuses(uploadStatus, statusUnsupported),
				Checks:  map[string]string{"upload-and-complete": uploadStatus, "abort": statusUnsupported},
			}, nil
		}
		return featureResult{}, unexpectedS3Error("CreateMultipartUpload for abort", err)
	}
	if upload.UploadId == nil {
		return featureResult{}, errors.New("CreateMultipartUpload returned no upload ID")
	}

	part := makeMultipartPart(3)
	_, uploadPartErr := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     stringPointer(bucket),
		Key:        stringPointer(key),
		UploadId:   upload.UploadId,
		PartNumber: int32Pointer(1),
		Body:       bytes.NewReader(part),
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
	abortStatus := statusSupported
	if abortErr != nil {
		if isUnsupportedFeature(abortErr) {
			abortStatus = statusUnsupported
		} else {
			return featureResult{}, unexpectedS3Error("AbortMultipartUpload", abortErr)
		}
	} else {
		_, listErr := client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   stringPointer(bucket),
			Key:      stringPointer(key),
			UploadId: upload.UploadId,
		})
		if !isNoSuchUpload(listErr) {
			if listErr != nil {
				return featureResult{}, unexpectedS3Error("verify aborted multipart upload", listErr)
			}
			abortStatus = statusUnsupported
		}
	}

	return featureResult{
		Feature: "multipart-upload-and-abort",
		Status:  combineStatuses(uploadStatus, abortStatus),
		Checks: map[string]string{
			"upload-and-complete": uploadStatus,
			"abort":               abortStatus,
		},
	}, nil
}

func probeHeadChecksum(t *testing.T, ctx context.Context, client *s3.Client, bucket, prefix string) (featureResult, error) {
	key := prefix + "/head-checksum"
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
			return featureResult{Feature: "head-checksum-sha256", Status: statusUnsupported}, nil
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
			return featureResult{Feature: "head-checksum-sha256", Status: statusUnsupported}, nil
		}
		return featureResult{}, unexpectedS3Error("HeadObject checksum", err)
	}
	if head.ChecksumSHA256 == nil {
		return featureResult{Feature: "head-checksum-sha256", Status: statusUnsupported}, nil
	}
	if *head.ChecksumSHA256 != checksum {
		return featureResult{}, errors.New("HeadObject returned a SHA-256 checksum different from the PutObject checksum")
	}
	return featureResult{
		Feature: "head-checksum-sha256",
		Status:  statusSupported,
		Note:    "checked with single-part PutObject; multipart SHA-256 values can be composite and are not compared with the full-object digest",
	}, nil
}

func probePresignedRange(t *testing.T, ctx context.Context, client *s3.Client, presigner *s3.PresignClient, bucket, prefix string) (featureResult, error) {
	key := prefix + "/presigned-range"
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

	getResponse, err := doPresignedGet(ctx, presigner, bucket, key, "")
	if err != nil {
		return featureResult{}, err
	}
	getStatus := statusSupported
	if getResponse.StatusCode == http.StatusNotImplemented {
		getStatus = statusUnsupported
	} else if getResponse.StatusCode != http.StatusOK || !bytes.Equal(getResponse.Body, body) {
		return featureResult{}, fmt.Errorf("presigned GET returned HTTP %d or an unexpected body", getResponse.StatusCode)
	}
	if getStatus == statusUnsupported {
		return featureResult{
			Feature: "presigned-get-range",
			Status:  statusUnsupported,
			Checks:  map[string]string{"presigned-get": getStatus, "range": statusUnsupported},
		}, nil
	}

	const rangeHeader = "bytes=3-9"
	rangeResponse, err := doPresignedGet(ctx, presigner, bucket, key, rangeHeader)
	if err != nil {
		return featureResult{}, err
	}
	rangeStatus := statusSupported
	if rangeResponse.StatusCode == http.StatusOK || rangeResponse.StatusCode == http.StatusNotImplemented {
		rangeStatus = statusUnsupported
	} else if rangeResponse.StatusCode != http.StatusPartialContent ||
		!bytes.Equal(rangeResponse.Body, body[3:10]) || rangeResponse.ContentRange != "bytes 3-9/36" {
		rangeStatus = statusUnsupported
	}
	return featureResult{
		Feature: "presigned-get-range",
		Status:  combineStatuses(getStatus, rangeStatus),
		Checks:  map[string]string{"presigned-get": getStatus, "range": rangeStatus},
	}, nil
}

func doPresignedGet(ctx context.Context, presigner *s3.PresignClient, bucket, key, rangeHeader string) (presignedResponse, error) {
	input := &s3.GetObjectInput{Bucket: stringPointer(bucket), Key: stringPointer(key)}
	if rangeHeader != "" {
		input.Range = stringPointer(rangeHeader)
	}
	presigned, err := presigner.PresignGetObject(ctx, input, func(options *s3.PresignOptions) {
		options.Expires = 2 * time.Minute
	})
	if err != nil {
		return presignedResponse{}, fmt.Errorf("presign GET: %w", err)
	}
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
		body[index] = byte((index*31 + index/251) % 251) ^ seed
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

func unsupportedOrError(operation string, err error) (string, error) {
	if isUnsupportedFeature(err) {
		return statusUnsupported, nil
	}
	return "", unexpectedS3Error(operation, err)
}

func unexpectedS3Error(operation string, err error) error {
	return fmt.Errorf("%s returned unexpected S3 error code %q", operation, apiErrorCode(err))
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
