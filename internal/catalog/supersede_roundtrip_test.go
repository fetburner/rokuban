package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// superseded_at が catalog の往復で保存されること（issue #129 症状 2）。
//
// superseded_at を運ばないと、同一 active-event の「枠を明け渡した failed 行」と
// 「成功した行」が rescue 側でどちらも superseded_at IS NULL に戻り、
// recordings_unique_active_event（deleted_at IS NULL AND superseded_at IS NULL）に
// 衝突して復旧そのものが落ちる。
func TestExportRescue_PreservesSupersededAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	start := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)

	newRec := func(eventID int32, status string) int64 {
		id, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
			Source: "manual", Site: "default",
			NetworkID: 32736, ServiceID: 1024, EventID: eventID,
			ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
			Title: "再録画された番組", IsFree: true,
			ProgramStartAt:    start,
			ProgramDurationMs: 180000,
			Status:            status,
		})
		if err != nil {
			t.Fatalf("CreateRecording(%s): %v", status, err)
		}
		return id
	}

	failedID := newRec(200, "failed")
	n, err := q.SupersedeFailedRecording(ctx, sqlcgen.SupersedeFailedRecordingParams{
		Site: "default", NetworkID: 32736, ServiceID: 1024, EventID: 200,
		ProgramStartAt: start,
	})
	if err != nil || n != 1 {
		t.Fatalf("SupersedeFailedRecording: rows=%d err=%v", n, err)
	}
	okID := newRec(200, "finished") // 枠が空いたので入る

	doc, err := Export(ctx, pool)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.Recordings) != 2 {
		t.Fatalf("exported recordings = %d, want 2", len(doc.Recordings))
	}

	mediaDir := t.TempDir()
	genDir, err := Write(mediaDir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(genDir, DocumentFilename)
	if _, err := pool.Exec(ctx, `TRUNCATE media_assets, recordings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if _, err := RescueFile(ctx, pool, path); err != nil {
		t.Fatalf("RescueFile: %v （superseded_at が往復で失われると一意索引に衝突して復旧が落ちる）", err)
	}

	var supersededCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recordings WHERE superseded_at IS NOT NULL`,
	).Scan(&supersededCount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if supersededCount != 1 {
		t.Errorf("superseded な行 = %d, want 1（failed 行 id=%d が superseded のまま戻るべき。成功行 id=%d は live）",
			supersededCount, failedID, okID)
	}
}
