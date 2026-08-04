package catalog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestExportRescue_PreservesNeverScheduledRecording は issue #98 の never-scheduled
// 行（recordings.status='failed' + quality_events に recording.never-scheduled）
// が catalog の export/rescue を往復しても復元できることを確認する。
//
// #143 のレビューで申し送りされた教訓（recordings の「行の見え方を変える」変更は
// catalog の往復を必ず確認する。superseded_at が CatalogUpsertRecording の
// 列リストから漏れていて rescue が一意索引違反で落ちていた）を踏まえ、この PR
// では recordings に新しい列を追加していない（既存の status/quality_events の
// 組み合わせで表現している）ので、往復の失敗要因になりうるのは quality_events
// の内容が正しく保存されるかどうかだけである。
func TestExportRescue_PreservesNeverScheduledRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	programStartAt := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)
	programEndedAt := programStartAt.Add(30 * time.Minute)
	reason, err := json.Marshal(map[string]any{"programEndedAt": programEndedAt})
	if err != nil {
		t.Fatalf("marshalling reason: %v", err)
	}
	qe, err := json.Marshal([]map[string]any{{
		"at":     programEndedAt,
		"event":  "recording.never-scheduled",
		"reason": json.RawMessage(reason),
	}})
	if err != nil {
		t.Fatalf("marshalling quality_events: %v", err)
	}

	// recordings.reservation_id は issue #158 で列自体を落としたので、この試行行を
	// 予約と結びつける材料は存在しない --- 「予約が既に GC された後の
	// never-scheduled 行」であることも自然に模せる。
	rows, err := q.CreateNeverScheduledRecording(ctx, sqlcgen.CreateNeverScheduledRecordingParams{
		Source:            "rule",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           300,
		ServiceName:       "NHK総合",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "never-scheduled になった番組",
		ProgramStartAt:    programStartAt,
		ProgramDurationMs: 1800000,
		QualityEvents:     qe,
	})
	if err != nil {
		t.Fatalf("CreateNeverScheduledRecording: %v", err)
	}
	if rows != 1 {
		t.Fatalf("CreateNeverScheduledRecording rows = %d, want 1", rows)
	}

	doc, err := Export(ctx, pool, "")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.Recordings) != 1 {
		t.Fatalf("exported recordings = %d, want 1", len(doc.Recordings))
	}
	exported := doc.Recordings[0]
	if exported.Status != "failed" {
		t.Errorf("exported status = %q, want %q", exported.Status, "failed")
	}

	mediaDir := t.TempDir()
	path, err := Write(mediaDir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE media_assets, recordings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	result, err := RescueFile(ctx, pool, path)
	if err != nil {
		t.Fatalf("RescueFile: %v (never-scheduled 行が復元できないと disaster recovery が壊れる)", err)
	}
	if result.Recordings != 1 {
		t.Errorf("rescued recordings = %d, want 1", result.Recordings)
	}

	var status string
	var qualityEvents []byte
	if err := pool.QueryRow(ctx, `
SELECT status, quality_events FROM recordings
WHERE site = 'default' AND network_id = 32736 AND service_id = 1024 AND event_id = 300`,
	).Scan(&status, &qualityEvents); err != nil {
		t.Fatalf("querying rescued recording: %v", err)
	}
	if status != "failed" {
		t.Errorf("rescued status = %q, want %q", status, "failed")
	}

	var neverScheduledFound bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM jsonb_array_elements($1::jsonb) qe
    WHERE qe->>'event' = 'recording.never-scheduled'
)`, qualityEvents).Scan(&neverScheduledFound); err != nil {
		t.Fatalf("checking rescued quality_events: %v", err)
	}
	if !neverScheduledFound {
		t.Errorf("rescued quality_events does not contain the never-scheduled marker: %s", qualityEvents)
	}

	// 往復後も never_scheduled_events VIEW（00030）で検出できることを確認する
	// --- ここが壊れると、rescue 後に同じ予約が desired へ復活して POST を
	// 再送してしまう（issue #134 の再発）。
	var matchesExclusionPredicate bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM never_scheduled_events
    WHERE site = 'default' AND network_id = 32736 AND service_id = 1024 AND event_id = 300
)`).Scan(&matchesExclusionPredicate); err != nil {
		t.Fatalf("checking exclusion predicate: %v", err)
	}
	if !matchesExclusionPredicate {
		t.Error("rescued never-scheduled recording does not match the sync-exclusion predicate")
	}
}
