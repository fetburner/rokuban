package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// 判定 (c)。派生物 Put の後にユーザーが h264 を外すと、外す前に積まれたジョブは
// 公開しない（成功で終わる）。判定が無いと外した版がここで復活する。削除 reconcile の
// 後に来た公開なら、要求行の無い tombstone を active に戻して二度と消えない。
func TestEncodeWorker_ProfileRemovedFromDesiredIsNotPublished(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	ffmpegPath := installCountingFFmpeg(t)
	mediaDir, scratchDir := t.TempDir(), t.TempDir()
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "20240101/t5.m2ts", []string{"h264"}, []byte("original payload t5"))

	reached, release := pauseBeforeEncodeObjectPublication(t, func(string) bool { return true })
	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	done := make(chan error, 1)
	go func() { done <- runWorkerViaJobQFake(ctx, w, jobWithID(recordingID)) }()

	waitReached(t, reached, done)
	if err := sqlcgen.New(pool).RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{
		RecordingID: recordingID, Profile: "h264",
	}); err != nil {
		t.Fatalf("RemoveRecordingEncodeProfile: %v", err)
	}
	release()
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Work: %v, want success (skipped publish)", err)
	}

	var activeCopies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'active'`, recordingID).Scan(&activeCopies); err != nil {
		t.Fatal(err)
	}
	if activeCopies != 0 {
		t.Errorf("active encoded copies = %d, want none after desired profile removal", activeCopies)
	}
	var pendingCopies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
		WHERE a.recording_id = $1 AND a.kind = 'encoded' AND a.profile = 'h264' AND c.state = 'pending'`, recordingID).Scan(&pendingCopies); err != nil {
		t.Fatal(err)
	}
	if pendingCopies != 1 {
		t.Errorf("pending encoded copies = %d, want one unexposed copy", pendingCopies)
	}
	finalPath := filepath.Join(mediaDir, "20240101", "t5_h264.mp4")
	if _, err := os.Stat(finalPath); err != nil {
		t.Errorf("pending object stat err = %v, want the unexposed Put result", err)
	}
}
