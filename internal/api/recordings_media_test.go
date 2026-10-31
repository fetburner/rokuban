package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 録画 API が返す配信 URL の置き場は、active な原本のコピーがある置き場である（設定の置き場ではない）。
// 原本とコマは置き場の URL、派生物（サムネイル・シークタイル・encoded）は置き場の無い URL。
// コピーが無ければ原本とコマの URL は省く。
func TestRecordingMediaURLsUseTheStoreHoldingTheOriginal(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{
		RecordingQuery: pgstore.NewRecordings(pool, ""), // api は置き場に束ねない
	}))
	defer srv.Close()

	moved := dbtest.Recording(t, pool)
	assetID := seedIngested(t, pool, moved, 123, nil)
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET store = 'takamatsu' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("moving the original copy to takamatsu: %v", err)
	}
	noOriginal := dbtest.Recording(t, pool)

	var detail Recording
	if resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, moved), &detail); resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}
	wantOriginal := fmt.Sprintf("/api/media/stores/takamatsu/recordings/%d/file", moved)
	wantFrame := fmt.Sprintf("/api/media/stores/takamatsu/recordings/%d/frame", moved)
	if detail.Media.OriginalUrl == nil || *detail.Media.OriginalUrl != wantOriginal {
		t.Errorf("originalUrl = %v, want %q", detail.Media.OriginalUrl, wantOriginal)
	}
	if detail.Media.FrameUrl == nil || *detail.Media.FrameUrl != wantFrame {
		t.Errorf("frameUrl = %v, want %q", detail.Media.FrameUrl, wantFrame)
	}
	if want := fmt.Sprintf("/api/media/recordings/%d/thumbnail", moved); detail.Media.ThumbnailUrl != want {
		t.Errorf("thumbnailUrl = %q, want %q", detail.Media.ThumbnailUrl, want)
	}
	if want := fmt.Sprintf("/api/media/recordings/%d/seek-tiles", moved); detail.Media.SeekTilesUrl != want {
		t.Errorf("seekTilesUrl = %q, want %q", detail.Media.SeekTilesUrl, want)
	}

	var none Recording
	if resp := getJSON(t, fmt.Sprintf("%s/api/recordings/%d", srv.URL, noOriginal), &none); resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d", resp.StatusCode)
	}
	if none.Media.OriginalUrl != nil || none.Media.FrameUrl != nil {
		t.Errorf("a recording without an original has originalUrl/frameUrl = %v/%v, want both omitted", none.Media.OriginalUrl, none.Media.FrameUrl)
	}
	if len(none.Media.Encoded) != 0 {
		t.Errorf("encoded media = %v, want none", none.Media.Encoded)
	}
}
