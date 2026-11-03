//go:build s3conformance

package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/contracttest"
)

func TestStoreContract(t *testing.T) {
	cfg := conformanceConfig(t)
	contracttest.Run(t, func(t *testing.T) objstore.Store {
		objects, err := New(t.Context(), cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return objects
	})
}

// TestPutStoresUploadToken は、実 S3 API が Put の upload token を metadata として返すことを確かめる。
// 所有判定は HEAD の token に頼るので、非空の object と 0 バイトの object の両方を見る。
func TestPutStoresUploadToken(t *testing.T) {
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
