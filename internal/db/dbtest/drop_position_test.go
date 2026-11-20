package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 読み取りは drop_positions の行を直接見る。契約の ListRecordingDropPositions は
// current な計測記録のある原本しか返さず、ビルダーの行の値を確かめられない。
func TestDropPosition_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	recID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recID)

	dbtest.DropPosition(t, pool, assetID)
	var elapsed *int64
	testutil.RequireNoError(t, pool.QueryRow(ctx, `
		SELECT elapsed_ms FROM drop_positions
		WHERE media_asset_id = $1 AND pid = $2 AND byte_offset = 0`, assetID, 0x100).Scan(&elapsed),
		"reading default drop position")
	if elapsed != nil {
		t.Fatalf("default drop position elapsed_ms = %d, want NULL", *elapsed)
	}

	want := int64(1000)
	dbtest.DropPosition(t, pool, assetID, func(p *dbtest.DropPositionParams) {
		p.Pid = 0x101
		p.ByteOffset = 188
		p.ElapsedMs = &want
	})
	testutil.RequireNoError(t, pool.QueryRow(ctx, `
		SELECT elapsed_ms FROM drop_positions
		WHERE media_asset_id = $1 AND pid = $2 AND byte_offset = 188`, assetID, 0x101).Scan(&elapsed),
		"reading overridden drop position")
	if elapsed == nil || *elapsed != want {
		t.Fatalf("overridden drop position elapsed_ms = %v, want %d", elapsed, want)
	}
}
