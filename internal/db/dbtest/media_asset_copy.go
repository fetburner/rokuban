package dbtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// MediaAssetCopyParams は MediaAssetCopy の上書き対象で、UpsertMediaAssetCopy の入力と同じ項目。
type MediaAssetCopyParams struct {
	Store      string
	RelPath    string
	Generation int64
	State      string
	CreatedAt  *time.Time
	DeletedAt  *time.Time
}

// MediaAssetCopy は mediaAssetID の物理コピー（media_asset_copies の行）を作り、その id を返す。既定値は置き場 StoreName、相対パス test-copy/<mediaAssetID>.ts、世代 0、state = 'active'、created_at は現在時刻、deleted_at は無し。opts で既定値を上書きする。
// UpsertMediaAssetCopy で作るので、(store, rel_path) の行が既にあれば新しい行は作らず、その行の created_at を更新する。更新が起きなかったら失敗する。
func MediaAssetCopy(t testing.TB, db sqlcgen.DBTX, mediaAssetID int64, opts ...func(*MediaAssetCopyParams)) int64 {
	t.Helper()
	params := MediaAssetCopyParams{
		Store:      StoreName,
		RelPath:    fmt.Sprintf("test-copy/%d.ts", mediaAssetID),
		Generation: 0,
		State:      "active",
	}
	for _, opt := range opts {
		opt(&params)
	}

	rows, err := sqlcgen.New(db).UpsertMediaAssetCopy(t.Context(), sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: mediaAssetID,
		Store:        params.Store,
		RelPath:      params.RelPath,
		Generation:   params.Generation,
		State:        params.State,
		CreatedAt:    params.CreatedAt,
		DeletedAt:    params.DeletedAt,
	})
	if err != nil {
		t.Fatalf("inserting media asset copy for asset %d in store %q: %v", mediaAssetID, params.Store, err)
	}
	if rows != 1 {
		t.Fatalf("inserting media asset copy for asset %d in store %q affected %d rows, want 1", mediaAssetID, params.Store, rows)
	}

	var id int64
	if err := db.QueryRow(t.Context(), `SELECT id FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, params.Store, params.RelPath).Scan(&id); err != nil {
		t.Fatalf("reading id of media asset copy %q in store %q: %v", params.RelPath, params.Store, err)
	}
	return id
}
