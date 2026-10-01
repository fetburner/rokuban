package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 同じ局の 3 録画: 検出結果 + 有効な原本 / 検出結果 + 実体の無い原本 / 検出結果なし。
// detectedCount は結果のある録画（2）、redetectableCount はそのうち原本を取れるもの（1）。
func TestCMLogoAPICountsDetectedAndRedetectable(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	q := sqlcgen.New(pool)
	ids := make([]int64, 3)
	for i := range ids {
		ids[i] = seedRecording(t, pool, "件数", time.Now().Truncate(time.Second), "finished", int32(990+i))
		if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
			RecordingID: ids[i], Kind: db.AssetKindOriginal,
			RelPath: fmt.Sprintf("test/%d.ts", ids[i]), SizeBytes: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids[:2] {
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO missing_media_assets (media_asset_id)
		SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, ids[1]); err != nil {
		t.Fatal(err)
	}

	logos := fetchCMLogos(t, srv.URL)
	if len(logos) != 1 {
		t.Fatalf("logo list rows = %d, want 1", len(logos))
	}
	if logos[0].DetectedCount != 2 || logos[0].RedetectableCount != 1 {
		t.Errorf("detected/redetectable = %d/%d, want 2/1", logos[0].DetectedCount, logos[0].RedetectableCount)
	}
}
