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

// Recording は既定値で録画を 1 件作り、その id を返す。opts で既定値を上書きする。
func Recording(t testing.TB, db sqlcgen.DBTX, opts ...func(*sqlcgen.CreateRecordingParams)) int64 {
	t.Helper()
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
		t.Fatalf("create test recording: %v", err)
	}
	return id
}

var assetPathID atomic.Int64

// OriginalAsset は既定値で原本 media_asset を 1 件作り、その id を返す。opts で既定値を上書きする。
func OriginalAsset(t testing.TB, db sqlcgen.DBTX, recordingID int64, opts ...func(*sqlcgen.CreateMediaAssetParams)) int64 {
	t.Helper()
	asset := mediaAssetParams(t, db, recordingID)
	params := sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        "original",
		RelPath:     fmt.Sprintf("sites/%s/test/%d.ts", asset, assetPathID.Add(1)),
		SizeBytes:   1,
	}
	for _, opt := range opts {
		opt(&params)
	}

	id, err := sqlcgen.New(db).CreateMediaAsset(context.Background(), params)
	if err != nil {
		t.Fatalf("create test original media asset: %v", err)
	}
	return id
}

func mediaAssetParams(t testing.TB, db sqlcgen.DBTX, recordingID int64) string {
	t.Helper()
	var site string
	if err := db.QueryRow(context.Background(), `SELECT site FROM recordings WHERE id = $1`, recordingID).Scan(&site); err != nil {
		t.Fatalf("get test recording site for media asset: %v", err)
	}
	return site
}
