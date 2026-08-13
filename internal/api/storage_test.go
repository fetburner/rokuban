package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// insertStorageSyncFixture は storage_sync 行を直接 INSERT する（worker の観測
// ループを経由しない。GET /api/storage は射影を読むだけなので、行の直接投入で
// レスポンス整形だけをテストできる）。
func insertStorageSyncFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, root, path string, total, used, available int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes)
VALUES ($1, $2, $3, $4, $5)`, root, path, total, used, available); err != nil {
		t.Fatalf("inserting storage_sync fixture %q: %v", root, err)
	}
}

// storageRootResp は GET /api/storage のレスポンス要素。
type storageRootResp struct {
	Root           string    `json:"root"`
	Path           string    `json:"path"`
	TotalBytes     int64     `json:"totalBytes"`
	UsedBytes      int64     `json:"usedBytes"`
	AvailableBytes int64     `json:"availableBytes"`
	ObservedAt     time.Time `json:"observedAt"`
}

// 1. 観測済みの root が一覧に現れ、バイト数と observedAt が読める。
func TestGetStorage_ReturnsObservedRoots(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	before := time.Now().Add(-time.Second)
	insertStorageSyncFixture(t, pool, ctx, "media", "/mnt/media", 1000, 400, 600)
	insertStorageSyncFixture(t, pool, ctx, "scratch", "/var/tmp/rokuban", 100, 10, 90)

	resp, err := http.Get(srv.URL + "/api/storage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got []storageRootResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	byRoot := map[string]storageRootResp{}
	for _, r := range got {
		byRoot[r.Root] = r
	}

	media, ok := byRoot["media"]
	if !ok {
		t.Fatal("media root missing from response")
	}
	if media.Path != "/mnt/media" || media.TotalBytes != 1000 || media.UsedBytes != 400 || media.AvailableBytes != 600 {
		t.Errorf("media = %+v, want path=/mnt/media total=1000 used=400 available=600", media)
	}
	if media.ObservedAt.Before(before) {
		t.Errorf("media.ObservedAt = %v, want after %v", media.ObservedAt, before)
	}

	scratch, ok := byRoot["scratch"]
	if !ok {
		t.Fatal("scratch root missing from response")
	}
	if scratch.Path != "/var/tmp/rokuban" || scratch.TotalBytes != 100 {
		t.Errorf("scratch = %+v, want path=/var/tmp/rokuban total=100", scratch)
	}
}

// 2. 何も観測されていなければ空配列（null ではない）が返る --- worker がまだ
// 1 パスも回っていない、または全 root が observe に失敗し続けている状態。
func TestGetStorage_EmptyWhenNoObservation(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/storage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if body != "[]\n" && body != "[]" {
		t.Errorf("body = %q, want literal empty array (not null)", body)
	}
}
