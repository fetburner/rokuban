package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRunShadowDiff_EndToEnd は DB（reservations / program_intents）と
// EPGStation（httptest サーバー）の両方から実際にデータを集めて Compare するところまでを
// 通しで確認する。CLI 本体（cobra RunE）はごく薄い配線なので、ここでは
// runShadowDiff / printShadowDiffReport という切り出した関数を直接叩く。
func TestRunShadowDiff_EndToEnd(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	start := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)

	// Rokuban 側: programId=1 は両方に存在させる（Both）
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 1, Title: "番組1",
		ProgramStartAt: start, ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("creating reservation 1: %v", err)
	}
	// programId=2 は Rokuban だけ（RokubanOnly）
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 2, Title: "番組2",
		ProgramStartAt: start.Add(time.Hour), ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("creating reservation 2: %v", err)
	}
	// programId=3 は Rokuban 側で skip 意図（EPGStation 側にはあるが Expected）
	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site: db.DefaultSite, ProgramID: 3,
		ProgramStartAt: start.Add(2 * time.Hour), ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("skipping program 3: %v", err)
	}

	// EPGStation 側のフィクスチャ
	reserves := []epgstation.Reserve{
		{ID: 100, ProgramID: int64Ptr(1), Name: "番組1", StartAt: start.UnixMilli(), EndAt: start.Add(30 * time.Minute).UnixMilli()},
		{ID: 101, ProgramID: int64Ptr(3), Name: "番組3", StartAt: start.Add(2 * time.Hour).UnixMilli(), EndAt: start.Add(2*time.Hour + 30*time.Minute).UnixMilli()},
		{ID: 102, ProgramID: int64Ptr(4), Name: "番組4", StartAt: start.Add(3 * time.Hour).UnixMilli(), EndAt: start.Add(3*time.Hour + 30*time.Minute).UnixMilli()},
		{ID: 103, ProgramID: nil, IsTimeSpecified: true, Name: "時刻指定予約", StartAt: start.Add(4 * time.Hour).UnixMilli(), EndAt: start.Add(4*time.Hour + 30*time.Minute).UnixMilli()},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"reserves": %s, "total": %d}`, mustMarshalReserves(t, reserves), len(reserves))
	}))
	defer srv.Close()

	epgClient := epgstation.NewClient(srv.URL, srv.Client())

	report, err := runShadowDiff(ctx, q, epgClient, db.DefaultSite)
	if err != nil {
		t.Fatalf("runShadowDiff: %v", err)
	}

	if len(report.Both) != 1 || report.Both[0].ProgramID != 1 {
		t.Errorf("Both = %+v, want [programId=1]", report.Both)
	}
	if len(report.RokubanOnly) != 1 || report.RokubanOnly[0].ProgramID != 2 {
		t.Errorf("RokubanOnly = %+v, want [programId=2]", report.RokubanOnly)
	}
	if len(report.EPGStationOnly) != 1 || report.EPGStationOnly[0].ProgramID != 4 {
		t.Errorf("EPGStationOnly = %+v, want [programId=4]", report.EPGStationOnly)
	}
	if len(report.Expected) != 2 {
		t.Fatalf("len(Expected) = %d, want 2 (skip + time-specified): %+v", len(report.Expected), report.Expected)
	}
	if !report.HasUnexplained() {
		t.Error("HasUnexplained() = false, want true (RokubanOnly/EPGStationOnly present)")
	}

	var out bytes.Buffer
	if err := printShadowDiffReport(&out, report); err != nil {
		t.Fatalf("printShadowDiffReport: %v", err)
	}
	rendered := out.String()

	for _, want := range []string{
		"RokubanOnly:     1",
		"EPGStationOnly:  1",
		"Expected:        2",
		"番組2",               // RokubanOnly の題名
		"番組4",               // EPGStationOnly の題名
		"時刻指定予約は programId", // allowlist の理由
		"JST",               // 時刻表示
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("output does not contain %q:\n%s", want, rendered)
		}
	}

	// JST 表示になっていること。RokubanOnly の programId=2 は start+1h UTC
	// = 22:00 UTC = 翌日 07:00 JST。
	if !strings.Contains(rendered, "2026-08-02 07:00:00") {
		t.Errorf("output should show start time converted to JST:\n%s", rendered)
	}
}

// TestRunShadowDiff_NoUnexplainedDiff は完全一致のケースで HasUnexplained が false になることを確認する。
func TestRunShadowDiff_NoUnexplainedDiff(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	start := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: db.DefaultSite, ProgramID: 1, Title: "番組1",
		ProgramStartAt: start, ProgramDurationMs: 1800000,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	reserves := []epgstation.Reserve{
		{ID: 100, ProgramID: int64Ptr(1), Name: "番組1", StartAt: start.UnixMilli(), EndAt: start.Add(30 * time.Minute).UnixMilli()},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"reserves": %s, "total": %d}`, mustMarshalReserves(t, reserves), len(reserves))
	}))
	defer srv.Close()

	report, err := runShadowDiff(ctx, q, epgstation.NewClient(srv.URL, srv.Client()), db.DefaultSite)
	if err != nil {
		t.Fatalf("runShadowDiff: %v", err)
	}
	if report.HasUnexplained() {
		t.Errorf("HasUnexplained() = true, want false: %+v", report)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func mustMarshalReserves(t *testing.T, reserves []epgstation.Reserve) []byte {
	t.Helper()
	data, err := json.Marshal(reserves)
	if err != nil {
		t.Fatalf("marshalling reserves fixture: %v", err)
	}
	return data
}
