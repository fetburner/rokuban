package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// 判定 (c)。ffmpeg の実行中にユーザーが h264 を外すと、外す前に積まれたジョブは
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
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "20240101/t5.m2ts", []string{"h264"}, []byte("original payload t5"))

	reached, release := pauseBeforeEncodeLock(t, func(string) bool { return true })
	w := newH264Worker(pool, mediaDir, scratchDir, ffmpegPath)
	done := make(chan error, 1)
	go func() { done <- w.Work(ctx, jobWithID(5, recordingID)) }()

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

	profile := "h264"
	if _, err := sqlcgen.New(pool).GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID, Profile: &profile,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("encoded row lookup err = %v, want no row (the removed profile must not be published)", err)
	}
	finalPath := filepath.Join(mediaDir, "20240101", "t5_h264.mp4")
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canonical stat err = %v, want not exist", err)
	}
	assertNoEncodeTemp(t, filepath.Dir(finalPath))
}
