package dbtest_test

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestProgramSnapshot_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)

	// 既定値の開始時刻は builder が呼ばれた時点の現在時刻 + 24 時間なので、前後を挟んで範囲で確かめる。
	before := time.Now().Add(24 * time.Hour)
	dbtest.ProgramSnapshot(t, pool, 1234)
	after := time.Now().Add(24 * time.Hour)
	got, err := q.GetProgramSnapshot(t.Context(), sqlcgen.GetProgramSnapshotParams{Site: "default", ProgramID: 1234})
	testutil.RequireNoError(t, err, "reading default program snapshot")
	if got.Title != "テスト番組" || got.DurationMs != 1800000 || got.NetworkID != 32736 || got.ServiceID != 1024 ||
		got.ChannelType != "GR" || got.Channel != "27" || got.EventID != 1234 || got.ServiceName != "テスト局" {
		t.Fatalf("default program snapshot = %+v, want title テスト番組, duration 1800000, network 32736, service 1024, GR, channel 27, event 1234, service テスト局", got)
	}
	if got.StartAt.Before(before.Add(-time.Second)) || got.StartAt.After(after.Add(time.Second)) {
		t.Fatalf("default program snapshot start_at = %v, want within [%v, %v]", got.StartAt, before, after)
	}

	wantStart := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	dbtest.ProgramSnapshot(t, pool, 5678, func(p *dbtest.ProgramSnapshotParams) {
		p.Site = "osaka"
		p.Title = "上書き番組"
		p.StartAt = wantStart
		p.DurationMs = 3600000
		p.NetworkID = 11500
		p.ServiceID = 1150
		p.ChannelType = "BS"
		p.Channel = "BS15_0"
		p.EventID = 200
		p.ServiceName = "上書き局"
	})
	got, err = q.GetProgramSnapshot(t.Context(), sqlcgen.GetProgramSnapshotParams{Site: "osaka", ProgramID: 5678})
	testutil.RequireNoError(t, err, "reading overridden program snapshot")
	if got.Title != "上書き番組" || !got.StartAt.Equal(wantStart) || got.DurationMs != 3600000 ||
		got.NetworkID != 11500 || got.ServiceID != 1150 || got.ChannelType != "BS" || got.Channel != "BS15_0" ||
		got.EventID != 200 || got.ServiceName != "上書き局" {
		t.Fatalf("overridden program snapshot = %+v, want title 上書き番組, start 2026-07-30T01:00:00Z, duration 3600000, network 11500, service 1150, BS, BS15_0, event 200, service 上書き局", got)
	}
}
