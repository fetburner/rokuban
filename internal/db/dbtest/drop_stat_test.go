package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 読み取りは drop_stats の行を直接見る。契約の ListRecordingDropStats は
// current な計測記録のある原本しか返さず、ビルダーの行の値を確かめられない。
func TestDropStat_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	recID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recID)

	dbtest.DropStat(t, pool, assetID)
	var packets, drops, errs, scrambled int64
	var pidType *string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `
		SELECT packets, drops, errors, scrambled, pid_type FROM drop_stats
		WHERE media_asset_id = $1 AND pid = $2`, assetID, 0x100).Scan(&packets, &drops, &errs, &scrambled, &pidType),
		"reading default drop stat")
	if packets != 10000 || drops != 0 || errs != 0 || scrambled != 0 || pidType != nil {
		t.Fatalf("default drop stat = packets %d drops %d errors %d scrambled %d pid_type %v, want 10000 0 0 0 nil",
			packets, drops, errs, scrambled, pidType)
	}

	video := "video"
	dbtest.DropStat(t, pool, assetID, func(p *dbtest.DropStatParams) {
		p.Pid = 0x101
		p.Packets = 20
		p.Drops = 3
		p.Errors = 1
		p.Scrambled = 2
		p.PidType = &video
	})
	testutil.RequireNoError(t, pool.QueryRow(ctx, `
		SELECT packets, drops, errors, scrambled, pid_type FROM drop_stats
		WHERE media_asset_id = $1 AND pid = $2`, assetID, 0x101).Scan(&packets, &drops, &errs, &scrambled, &pidType),
		"reading overridden drop stat")
	if packets != 20 || drops != 3 || errs != 1 || scrambled != 2 || pidType == nil || *pidType != "video" {
		t.Fatalf("overridden drop stat = packets %d drops %d errors %d scrambled %d pid_type %v, want 20 3 1 2 video",
			packets, drops, errs, scrambled, pidType)
	}
}
