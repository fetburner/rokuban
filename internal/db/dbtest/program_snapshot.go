package dbtest

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramSnapshotParams は ProgramSnapshot の上書き対象で、UpsertProgramSnapshot の入力と同じ項目（site と program_id は引数で受ける）。
type ProgramSnapshotParams struct {
	Site        string
	Title       string
	StartAt     time.Time
	DurationMs  int64
	NetworkID   int32
	ServiceID   int32
	ChannelType string
	Channel     string
	EventID     int32
	ServiceName string
}

// ProgramSnapshot は番組 programID の事実のスナップショット（program_snapshots の行）を作る。既定値は site = "default"、title = "テスト番組"、開始時刻 = 呼び出し時点の現在時刻 + 24 時間、尺 = 30 分、network_id = 32736、service_id = 1024、channel_type = "GR"、channel = "27"、event_id = programID % 100000、service_name = "テスト局"。opts で既定値を上書きする。
// UpsertProgramSnapshot で作るので、(site, program_id) の行が既にあれば新しい行は作らず、その行の全列を既定値（または上書き値）で置き換える。置き換えが起きなかったら失敗する。
func ProgramSnapshot(t testing.TB, db sqlcgen.DBTX, programID int64, opts ...func(*ProgramSnapshotParams)) {
	t.Helper()
	params := ProgramSnapshotParams{
		Site:        "default",
		Title:       "テスト番組",
		StartAt:     time.Now().Add(24 * time.Hour),
		DurationMs:  1800000,
		NetworkID:   32736,
		ServiceID:   1024,
		ChannelType: "GR",
		Channel:     "27",
		EventID:     int32(programID % 100000),
		ServiceName: "テスト局",
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).UpsertProgramSnapshot(t.Context(), sqlcgen.UpsertProgramSnapshotParams{
		Site:        params.Site,
		ProgramID:   programID,
		Title:       params.Title,
		StartAt:     params.StartAt,
		DurationMs:  params.DurationMs,
		NetworkID:   params.NetworkID,
		ServiceID:   params.ServiceID,
		ChannelType: params.ChannelType,
		Channel:     params.Channel,
		EventID:     params.EventID,
		ServiceName: params.ServiceName,
	}); err != nil {
		t.Fatalf("upserting program snapshot %d at site %q: %v", programID, params.Site, err)
	}
}
