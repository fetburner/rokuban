package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
)

func runThumbnailReconcilePass(t *testing.T, pool *pgxpool.Pool, w *ThumbnailReconcileWorker) {
	t.Helper()
	job := &river.Job[ThumbnailReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ThumbnailReconcileArgs{},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("ThumbnailReconcileWorker.Work: %v", err)
	}
}

func countThumbnailJobs(t *testing.T, pool *pgxpool.Pool, recordingID int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job
		 WHERE kind = 'thumbnail'
		   AND (args->>'recording_id')::bigint = $1`, recordingID,
	).Scan(&count); err != nil {
		t.Fatalf("counting thumbnail jobs: %v", err)
	}
	return count
}

// ヒント投入が無く、edge record の削除で record_sweep からも再投入できない
// 状態でも、定期パスが thumbnail ジョブを作り、通常の worker が thumbnail を
// コミットできることを端から端まで固定する。
func TestThumbnailReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "lost-hint/original.m2ts", []byte("fake-ts"))

	if got := countThumbnailJobs(t, pool, recordingID); got != 0 {
		t.Fatalf("thumbnail jobs before the periodic pass = %d, want 0", got)
	}

	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})
	if got := countThumbnailJobs(t, pool, recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}

	thumbnailWorker := &ThumbnailWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		runCmd:     fakeThumbnailTools(t, 60),
	}
	job := &river.Job[ThumbnailJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   ThumbnailJobArgs{RecordingID: recordingID},
	}
	if err := thumbnailWorker.Work(context.Background(), job); err != nil {
		t.Fatalf("ThumbnailWorker.Work: %v", err)
	}
	if _, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID); err != nil {
		t.Fatalf("thumbnail was not born after periodic recovery: %v", err)
	}
}

// 同じ desired−observed の差分を何度見ても、pending thumbnail job は River の
// 一意制約で 1 本に合流する。
func TestThumbnailReconcile_DoesNotDoubleEnqueue(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, t.TempDir(), recordingID, "duplicate/original.m2ts", []byte("fake-ts"))
	w := &ThumbnailReconcileWorker{Pool: pool}

	runThumbnailReconcilePass(t, pool, w)
	runThumbnailReconcilePass(t, pool, w)

	if got := countThumbnailJobs(t, pool, recordingID); got != 1 {
		t.Errorf("thumbnail jobs after two periodic passes = %d, want 1", got)
	}
}

// ファイルが無いことを delete_reconcile が確認した原本は定期パスから除外する。
// マーカーが消えた後は同じ録画を回収対象へ戻す。
func TestThumbnailReconcile_SkipsKnownMissingOriginalUntilRestored(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "missing/original.m2ts", []byte("fake-ts"))
	if err := os.Remove(filepath.Join(mediaDir, "missing", "original.m2ts")); err != nil {
		t.Fatalf("removing original fixture: %v", err)
	}
	if err := q.UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})
	if got := countThumbnailJobs(t, pool, recordingID); got != 0 {
		t.Fatalf("thumbnail jobs for known-missing original = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileCandidates); got != 0 {
		t.Fatalf("thumbnail reconcile candidates for known-missing original = %v, want 0", got)
	}

	if err := q.DeleteMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("clearing restored original marker: %v", err)
	}
	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})
	if got := countThumbnailJobs(t, pool, recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after clearing restored marker = %d, want 1", got)
	}
}

// 定期パスの恒久失敗ガードは、明示的な復旧投入まで狭めてはいけない。
func TestEnqueueMissingThumbnails_IncludesKnownMissingOriginal(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, t.TempDir(), recordingID, "manual-recovery/original.m2ts", []byte("fake-ts"))
	if err := q.UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	n, err := EnqueueMissingThumbnails(ctx, pool, client)
	if err != nil {
		t.Fatalf("EnqueueMissingThumbnails: %v", err)
	}
	if n != 1 {
		t.Fatalf("EnqueueMissingThumbnails returned %d, want 1", n)
	}
	if got := countThumbnailJobs(t, pool, recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after explicit recovery = %d, want 1", got)
	}
}

// ごみ箱の録画は original が残っていても定期パスから除外する。
func TestThumbnailReconcile_ExcludesTrash(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "trash/original.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seeding original: %v", err)
	}
	if _, err := q.SoftDeleteRecording(ctx, recordingID); err != nil {
		t.Fatalf("soft deleting recording: %v", err)
	}

	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})
	if got := countThumbnailJobs(t, pool, recordingID); got != 0 {
		t.Errorf("thumbnail jobs for trashed recording = %d, want 0", got)
	}
}

// RowLimit は録画単位の上限であり、先頭候補が解消しなくても窓を回して
// 後続候補へ到達する。
func TestThumbnailReconcile_WindowRotatesPastStuckCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := insertTestRecordingForSite(t, pool, "default", 801)
	secondID := insertTestRecordingForSite(t, pool, "default", 802)
	seedOriginalAsset(t, pool, mediaDir, firstID, "window/first.m2ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "window/second.m2ts", []byte("second"))

	w := &ThumbnailReconcileWorker{Pool: pool, RowLimit: 1}
	runThumbnailReconcilePass(t, pool, w)
	if got := countThumbnailJobs(t, pool, firstID); got != 1 {
		t.Fatalf("first recording jobs after first pass = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, pool, secondID); got != 0 {
		t.Fatalf("second recording jobs after first pass = %d, want 0", got)
	}

	runThumbnailReconcilePass(t, pool, w)
	if got := countThumbnailJobs(t, pool, secondID); got != 1 {
		t.Fatalf("second recording jobs after rotated pass = %d, want 1", got)
	}
}

func TestThumbnailReconcileWorker_WorkWithoutClientErrors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	w := &ThumbnailReconcileWorker{Pool: pool}
	job := &river.Job[ThumbnailReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ThumbnailReconcileArgs{},
	}
	if err := w.Work(context.Background(), job); err == nil {
		t.Fatal("expected error when no river client is attached to ctx, got nil")
	}
}

func TestThumbnailReconcileArgs_KindAndInsertOpts(t *testing.T) {
	if kind := (ThumbnailReconcileArgs{}).Kind(); kind != "thumbnail_reconcile" {
		t.Errorf("Kind() = %q, want thumbnail_reconcile", kind)
	}
	opts := ThumbnailReconcileArgs{}.InsertOpts()
	if opts.Queue != "thumbnail" {
		t.Errorf("Queue = %q, want thumbnail", opts.Queue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("UniqueOpts.ByArgs should be true")
	}
	if slices.Contains(opts.UniqueOpts.ByState, rivertype.JobStateCompleted) {
		t.Error("UniqueOpts.ByState must not include completed (the periodic job would become one-shot)")
	}
}

func TestBuildRiverConfig_RegistersThumbnailReconcilePeriodicJob(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:               true,
		ThumbnailReconcile:         true,
		ThumbnailReconcileInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 1 {
		t.Fatalf("PeriodicJobs = %d, want 1", len(riverCfg.PeriodicJobs))
	}

	disabled, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:       false,
		ThumbnailReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig (periodic disabled): %v", err)
	}
	if len(disabled.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs with periodic_jobs=false = %d, want 0", len(disabled.PeriodicJobs))
	}
}
