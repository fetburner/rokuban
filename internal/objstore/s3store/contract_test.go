//go:build s3conformance

package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
)

func TestS3StoreContract(t *testing.T) {
	cfg := conformanceConfig(t)
	contracttest.Run(t, func(t *testing.T) objstore.Store {
		objects, err := New(t.Context(), cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return objects
	})
}

// TestS3PresignedGetReturnsDownloadName は、保存名を付けた署名 URL への GET に対し、
// 実 S3 互換サーバーが同じ Content-Disposition を返すことを確かめる。
// streamer は保存名を S3 の応答に載せさせるので、ここが崩れると保存名が消える。
func TestS3PresignedGetReturnsDownloadName(t *testing.T) {
	store, err := New(t.Context(), conformanceConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	key := fmt.Sprintf("conformance/download-name/%d.mkv", time.Now().UnixNano())
	t.Cleanup(func() { _ = store.Delete(context.WithoutCancel(ctx), key) })
	data := []byte("download name round trip")
	digest := sha256.Sum256(data)
	if err := store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), digest, "video/x-matroska"); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	target, err := store.Presign(ctx, key, http.MethodGet, time.Minute, "video/x-matroska", "120000_番組名_1024_h264.g2.mkv")
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building GET: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET signed URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET signed URL status = %d, want 200", resp.StatusCode)
	}
	want := "attachment; filename*=utf-8''120000_%E7%95%AA%E7%B5%84%E5%90%8D_1024_h264.g2.mkv"
	if got := resp.Header.Get("Content-Disposition"); got != want {
		t.Fatalf("Content-Disposition = %q, want %q", got, want)
	}
}

// TestS3PutStoresUploadToken は、実 S3 API が Put の upload token を metadata として返すことを確かめる。
// 所有判定は HEAD の token に頼るので、非空の object と 0 バイトの object の両方を見る。
func TestS3PutStoresUploadToken(t *testing.T) {
	store, err := New(t.Context(), conformanceConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	stamp := time.Now().UnixNano()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "non-empty", data: []byte("upload token round trip")},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("conformance/upload-token/%d-%s", stamp, tc.name)
			t.Cleanup(func() { _ = store.Delete(context.WithoutCancel(ctx), key) })
			digest := sha256.Sum256(tc.data)
			if err := store.Put(ctx, key, bytes.NewReader(tc.data), int64(len(tc.data)), digest, "application/octet-stream"); err != nil {
				t.Fatalf("Put(%q): %v", key, err)
			}
			_, token, err := store.head(ctx, key)
			if err != nil {
				t.Fatalf("head(%q): %v", key, err)
			}
			if token == "" {
				t.Fatalf("head(%q) upload token is empty, want the token Put wrote", key)
			}
		})
	}
}

// conformanceConfig は環境変数から接続先を読む。未設定なら、この test をスキップする。
func conformanceConfig(t *testing.T) Config {
	t.Helper()
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")), "/")
	bucket := strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET"))
	if endpoint == "" || bucket == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT and ROKUBAN_S3_CONFORMANCE_BUCKET are required")
	}
	region := strings.TrimSpace(os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION"))
	if region == "" {
		region = "us-east-1"
	}
	return Config{Endpoint: endpoint, Region: region, Bucket: bucket}
}
