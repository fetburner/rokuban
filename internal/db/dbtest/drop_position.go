package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// DropPositionParams は DropPosition の上書き対象で、drop_positions の 1 行（ドロップ観測位置）と同じ項目。
type DropPositionParams struct {
	Pid        int32
	ByteOffset int64
	ElapsedMs  *int64
}

// DropPosition は原本 mediaAssetID のドロップ観測位置を 1 行作る。既定値は pid = 0x100、byte_offset = 0、elapsed_ms = NULL（PCR を観測できなかった位置）。opts で既定値を上書きする。
func DropPosition(t testing.TB, db sqlcgen.DBTX, mediaAssetID int64, opts ...func(*DropPositionParams)) {
	t.Helper()
	params := DropPositionParams{Pid: 0x100}
	for _, opt := range opts {
		opt(&params)
	}
	var execErr error
	batch := sqlcgen.New(db).InsertDropPosition(t.Context(), []sqlcgen.InsertDropPositionParams{{
		MediaAssetID: mediaAssetID,
		ByteOffset:   params.ByteOffset,
		Pid:          params.Pid,
		ElapsedMs:    params.ElapsedMs,
	}})
	batch.Exec(func(_ int, err error) { execErr = err })
	if execErr != nil {
		t.Fatalf("inserting drop position for media asset %d pid %#x: %v", mediaAssetID, params.Pid, execErr)
	}
	if err := batch.Close(); err != nil {
		t.Fatalf("closing drop position batch for media asset %d: %v", mediaAssetID, err)
	}
}
