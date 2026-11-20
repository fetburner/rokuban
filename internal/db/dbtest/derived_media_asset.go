package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// DerivedMediaAssetParams は DerivedMediaAsset の上書き対象。
type DerivedMediaAssetParams struct {
	Profile    *string
	SizeBytes  int64
	Generation int64
}

// DerivedMediaAsset は録画 recordingID の派生資産の論理行（media_assets の行。kind は encoded・thumbnail・seek_tiles）を 1 件作り、id を返す。既定値は profile = nil、サイズ 1、世代 0。opts で既定値を上書きする。物理コピーは作らない。コピーは MediaAssetCopy で作る。
func DerivedMediaAsset(t testing.TB, db sqlcgen.DBTX, recordingID int64, kind string, opts ...func(*DerivedMediaAssetParams)) int64 {
	t.Helper()
	params := DerivedMediaAssetParams{SizeBytes: 1}
	for _, opt := range opts {
		opt(&params)
	}

	id, err := sqlcgen.New(db).UpsertDerivedMediaAsset(t.Context(), sqlcgen.UpsertDerivedMediaAssetParams{
		RecordingID:       recordingID,
		Kind:              kind,
		Profile:           params.Profile,
		SizeBytes:         params.SizeBytes,
		DerivedGeneration: params.Generation,
	})
	if err != nil {
		t.Fatalf("upserting derived media asset %q for recording %d: %v", kind, recordingID, err)
	}
	return id
}
