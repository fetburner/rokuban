package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// purged_at が catalog の往復で保存されること（issue #135）。
//
// purged_at を運ばないと、rescue で復元された tombstone は purged_at IS NULL
// に戻ってしまい、完全削除が完了して二度とファイルが戻らない録画がごみ箱
// 一覧（ListTrashRecordings は purged_at IS NULL を要求する）に再び現れる。
func TestExportRescue_PreservesPurgedAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	newRec := func(eventID int32) int64 {
		id, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
			Source: "manual", Site: "default",
			NetworkID: 32736, ServiceID: 1024, EventID: eventID,
			ServiceName: "NHK総合", ChannelType: "GR", Channel: "27",
			Title: "完全削除済みの録画", IsFree: true,
			ProgramStartAt:    time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC),
			ProgramDurationMs: 180000,
			Status:            "finished",
		})
		if err != nil {
			t.Fatalf("CreateRecording: %v", err)
		}
		return id
	}

	// purge 済み（完全削除が完了した tombstone）と、まだごみ箱にあるだけの
	// 録画の両方を用意し、往復後もこの区別が保たれることを確認する。
	purgedID := newRec(300)
	if _, err := pool.Exec(ctx,
		"UPDATE recordings SET deleted_at = now(), purged_at = now() WHERE id = $1", purgedID); err != nil {
		t.Fatalf("marking purged: %v", err)
	}
	trashedOnlyID := newRec(301)
	if _, err := pool.Exec(ctx,
		"UPDATE recordings SET deleted_at = now() WHERE id = $1", trashedOnlyID); err != nil {
		t.Fatalf("marking trashed: %v", err)
	}

	doc, err := Export(ctx, pool, "")
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
		t.Fatalf("RescueFile: %v", err)
	}

	var purgedAt, trashedOnlyPurgedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT purged_at FROM recordings WHERE id = $1`, purgedID,
	).Scan(&purgedAt); err != nil {
		t.Fatalf("query purged recording: %v", err)
	}
	if purgedAt == nil {
		t.Errorf("purged_at lost across export/rescue for recording %d "+
			"(rescue would resurrect a purged tombstone into the trash view)", purgedID)
	}

	if err := pool.QueryRow(ctx,
		`SELECT purged_at FROM recordings WHERE id = $1`, trashedOnlyID,
	).Scan(&trashedOnlyPurgedAt); err != nil {
		t.Fatalf("query trashed-only recording: %v", err)
	}
	if trashedOnlyPurgedAt != nil {
		t.Errorf("purged_at incorrectly set for recording %d that was only trashed, not purged", trashedOnlyID)
	}
}
