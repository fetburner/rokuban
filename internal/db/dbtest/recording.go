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

// RecordingParams は Recording の上書き対象で、sqlc の生成型の別名。
type RecordingParams = sqlcgen.CreateRecordingParams

// MediaAssetParams は OriginalAsset の上書き対象で、sqlc の生成型の別名。
type MediaAssetParams = sqlcgen.CreateMediaAssetParams

// Recording は既定値で録画を 1 件作り、その id を返す。opts で既定値を上書きする。
func Recording(t testing.TB, db sqlcgen.DBTX, opts ...func(*RecordingParams)) int64 {
	t.Helper()
	id, err := RecordingWithError(db, opts...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return id
}

// RecordingWithError は Recording と同じ録画を作り、エラーを返す。
func RecordingWithError(db sqlcgen.DBTX, opts ...func(*RecordingParams)) (int64, error) {
	params := sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           recordingEventID.Add(1) + 1_000_000,
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

	id, err := sqlcgen.New(db).CreateRecording(context.Background(), params)
	if err != nil {
		return 0, fmt.Errorf("create test recording: %w", err)
	}
	return id, nil
}

var assetPathID atomic.Int64

// OriginalAsset は既定値で原本 media_asset を 1 件作り、その id を返す。opts で既定値を上書きする。
func OriginalAsset(t testing.TB, db sqlcgen.DBTX, recordingID int64, opts ...func(*MediaAssetParams)) int64 {
	t.Helper()
	id, err := OriginalAssetWithError(db, recordingID, opts...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return id
}

// OriginalAssetWithError は OriginalAsset と同じ原本 media_asset を作り、エラーを返す。
func OriginalAssetWithError(db sqlcgen.DBTX, recordingID int64, opts ...func(*MediaAssetParams)) (int64, error) {
	params := sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "original",
		RelPath:     fmt.Sprintf("test-original/%d.ts", assetPathID.Add(1)),
		SizeBytes:   1,
	}
	for _, opt := range opts {
		opt(&params)
	}

	id, err := sqlcgen.New(db).CreateMediaAsset(context.Background(), params)
	if err != nil {
		return 0, fmt.Errorf("create test original media asset: %w", err)
	}
	return id, nil
}
