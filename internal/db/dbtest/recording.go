// Package dbtest provides PostgreSQL fixtures for database-backed tests.
package dbtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

var recordingEventID atomic.Int32

// 既定の event_id は ARIB の 16 bit に収め、明示値を使うテストが選ばない 60000 以上の帯から採る。
const (
	defaultEventIDBase = 60000
	defaultEventIDSpan = 65536 - defaultEventIDBase
)

// RecordingParams は Recording の上書き対象で、sqlc の生成型の別名。
type RecordingParams = sqlcgen.CreateRecordingParams

// MediaAssetParams は MediaAsset の上書き対象で、sqlc の生成型の別名。
type MediaAssetParams = sqlcgen.CreateMediaAssetParams

// StoreName はテストの media_asset_copies が使う置き場の名前。コピー行は実在の置き場名が要る。
const StoreName = "test-store"

// Recording は既定値で録画を 1 件作り、その id を返す。opts で既定値を上書きする。
func Recording(t testing.TB, db sqlcgen.DBTX, opts ...func(*RecordingParams)) int64 {
	t.Helper()
	id, err := RecordingWithError(t.Context(), db, opts...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return id
}

// RecordingWithError は Recording と同じ録画を作り、エラーを返す。
func RecordingWithError(ctx context.Context, db sqlcgen.DBTX, opts ...func(*RecordingParams)) (int64, error) {
	params := sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           defaultEventIDBase + recordingEventID.Add(1)%defaultEventIDSpan,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		IsFree:            true,
		ProgramStartAt:    time.Now().Truncate(time.Second),
		ProgramDurationMs: 1_800_000,
		Status:            "finished",
	}
	for _, opt := range opts {
		opt(&params)
	}

	id, err := sqlcgen.New(db).CreateRecording(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("create test recording: %w", err)
	}
	return id, nil
}

var assetPathID atomic.Int64

// MediaAsset は既定値で原本（kind=original）の media_asset を 1 件作り、その id を返す。opts で既定値を上書きする。
func MediaAsset(t testing.TB, db sqlcgen.DBTX, recordingID int64, opts ...func(*MediaAssetParams)) int64 {
	t.Helper()
	id, err := MediaAssetWithError(t.Context(), db, recordingID, opts...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return id
}

// MediaAssetCopyID は置き場にある物理コピーの ID を返す。
func MediaAssetCopyID(t testing.TB, db sqlcgen.DBTX, mediaAssetID int64, store string) int64 {
	t.Helper()
	var copyID int64
	if err := db.QueryRow(t.Context(), `
SELECT copy.id
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = $1 AND copy.store = $2
ORDER BY (copy.generation = COALESCE(asset.derived_generation, 0)) DESC,
         (copy.state = 'active') DESC,
         copy.id DESC
LIMIT 1`, mediaAssetID, store).Scan(&copyID); err != nil {
		t.Fatalf("get media asset copy ID for asset %d in store %q: %v", mediaAssetID, store, err)
	}
	return copyID
}

// ReplaceDerivedMediaAssetCopy はテスト用に派生資産の新世代コピーを公開し、旧コピー行を残す。
func ReplaceDerivedMediaAssetCopy(t testing.TB, db sqlcgen.DBTX, mediaAssetID int64, store, relPath string, generation int64) int64 {
	t.Helper()
	var copyID int64
	err := db.QueryRow(t.Context(), `
WITH logical AS (
    UPDATE media_assets
    SET rel_path = $3, derived_generation = $4, updated_at = now()
    WHERE id = $1 AND kind <> 'original'
    RETURNING id
)
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT id, $2, $3, $4, 'active' FROM logical
RETURNING id`, mediaAssetID, store, relPath, generation).Scan(&copyID)
	if err != nil {
		t.Fatalf("replacing derived media asset %d with generation %d at %q: %v", mediaAssetID, generation, relPath, err)
	}
	return copyID
}

// MediaAssetWithError は MediaAsset と同じ media_asset を作り、エラーを返す。
func MediaAssetWithError(ctx context.Context, db sqlcgen.DBTX, recordingID int64, opts ...func(*MediaAssetParams)) (int64, error) {
	params := sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "original",
		RelPath:     fmt.Sprintf("test-original/%d.ts", assetPathID.Add(1)),
		SizeBytes:   1,
	}
	for _, opt := range opts {
		opt(&params)
	}

	id, err := sqlcgen.New(db).CreateMediaAsset(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("create test media asset: %w", err)
	}
	if _, err := sqlcgen.New(db).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: id, Store: StoreName, RelPath: params.RelPath, Generation: 0, State: "active",
	}); err != nil {
		return 0, fmt.Errorf("create test media asset copy: %w", err)
	}
	return id, nil
}

// SetMediaAssetState はテスト資産とすべてのコピーの状態を同じ tx で変更する。
func SetMediaAssetState(t testing.TB, db sqlcgen.DBTX, assetID int64, state string) {
	t.Helper()
	_, err := db.Exec(t.Context(), `
WITH logical AS (
    UPDATE media_assets
    SET state = $2,
        deleted_at = CASE WHEN $2 = 'deleted' THEN COALESCE(deleted_at, now()) ELSE NULL END
    WHERE id = $1
    RETURNING id
)
UPDATE media_asset_copies copy
SET state = $2,
    deleted_at = CASE WHEN $2 = 'deleted' THEN COALESCE(copy.deleted_at, now()) ELSE NULL END
FROM logical
WHERE copy.media_asset_id = logical.id`, assetID, state)
	if err != nil {
		t.Fatalf("setting media asset %d and copy state to %s: %v", assetID, state, err)
	}
}

// SetRecordingMediaAssetState は録画の指定種別のテスト資産とコピーの状態を同じ tx で変更する。
func SetRecordingMediaAssetState(t testing.TB, db sqlcgen.DBTX, recordingID int64, kind, state string) {
	t.Helper()
	_, err := db.Exec(t.Context(), `
WITH logical AS (
    UPDATE media_assets
    SET state = $3,
        deleted_at = CASE WHEN $3 = 'deleted' THEN COALESCE(deleted_at, now()) ELSE NULL END
    WHERE recording_id = $1 AND ($2 = '' OR kind = $2)
    RETURNING id
)
UPDATE media_asset_copies copy
SET state = $3,
    deleted_at = CASE WHEN $3 = 'deleted' THEN COALESCE(copy.deleted_at, now()) ELSE NULL END
FROM logical
WHERE copy.media_asset_id = logical.id`, recordingID, kind, state)
	if err != nil {
		t.Fatalf("setting recording %d %s media asset and copy states to %s: %v", recordingID, kind, state, err)
	}
}

// SetMediaAssetRelPath はテスト資産とすべてのコピーの相対パスを同じ tx で変更する。
func SetMediaAssetRelPath(t testing.TB, db sqlcgen.DBTX, assetID int64, relPath string) {
	t.Helper()
	_, err := db.Exec(t.Context(), `
WITH logical AS (
    UPDATE media_assets SET rel_path = $2 WHERE id = $1 RETURNING id
)
UPDATE media_asset_copies copy
SET rel_path = $2
FROM logical
WHERE copy.media_asset_id = logical.id`, assetID, relPath)
	if err != nil {
		t.Fatalf("setting media asset %d and copy path to %q: %v", assetID, relPath, err)
	}
}
