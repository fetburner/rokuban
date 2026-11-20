package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// DropStatParams は DropStat の上書き対象で、drop_stats の 1 行（PID 別の統計）と同じ項目。
type DropStatParams struct {
	Pid       int32
	Packets   int64
	Drops     int64
	Errors    int64
	Scrambled int64
	PidType   *string
}

// DropStat は原本 mediaAssetID の PID 別ドロップ統計を 1 行作る。既定値は pid = 0x100、packets = 10000、drops・errors・scrambled = 0、pid_type = NULL。opts で既定値を上書きする。
// 同じ原本・PID の行が既にあると失敗する。PID ごとに呼ぶ。
func DropStat(t testing.TB, db sqlcgen.DBTX, mediaAssetID int64, opts ...func(*DropStatParams)) {
	t.Helper()
	params := DropStatParams{Pid: 0x100, Packets: 10000}
	for _, opt := range opts {
		opt(&params)
	}
	var execErr error
	batch := sqlcgen.New(db).InsertDropStat(t.Context(), []sqlcgen.InsertDropStatParams{{
		MediaAssetID: mediaAssetID,
		Pid:          params.Pid,
		Packets:      params.Packets,
		Drops:        params.Drops,
		Errors:       params.Errors,
		Scrambled:    params.Scrambled,
		PidType:      params.PidType,
	}})
	batch.Exec(func(_ int, err error) { execErr = err })
	if execErr != nil {
		t.Fatalf("inserting drop stat for media asset %d pid %#x: %v", mediaAssetID, params.Pid, execErr)
	}
	if err := batch.Close(); err != nil {
		t.Fatalf("closing drop stat batch for media asset %d: %v", mediaAssetID, err)
	}
}
