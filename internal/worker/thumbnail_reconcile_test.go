package worker

import (
	"context"
	"fmt"
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

func countSeekTilesJobs(t *testing.T, pool *pgxpool.Pool, recordingID int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job
		 WHERE kind = 'seek_tiles'
		   AND (args->>'recording_id')::bigint = $1`, recordingID,
	).Scan(&count); err != nil {
		t.Fatalf("counting seek_tiles jobs: %v", err)
	}
	return count
}

// 定期パスは thumbnail だけでなく seek_tiles のギャップも埋める。
func TestThumbnailReconcile_EnqueuesMissingSeekTiles(t *testing.T) {
	pool := setupTestPool(t)
	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, t.TempDir(), recordingID, "seek-tiles/original.m2ts", []byte("fake-ts"))

	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})

	if got := countSeekTilesJobs(t, pool, recordingID); got != 1 {
		t.Errorf("seek_tiles jobs after the periodic pass = %d, want 1", got)
	}
	// 同じパスが thumbnail も積む（どちらも desired − observed の差分）。
	if got := countThumbnailJobs(t, pool, recordingID); got != 1 {
		t.Errorf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}
}

// 実体無しと確認済みの原本は seek_tiles の候補からも除外する（poster と同じ扱い）。
func TestThumbnailReconcile_SeekTilesSkipsKnownMissingOriginal(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "seek-missing/original.m2ts", []byte("fake-ts"))
	if err := os.Remove(filepath.Join(mediaDir, "seek-missing", "original.m2ts")); err != nil {
		t.Fatalf("removing original fixture: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	runThumbnailReconcilePass(t, pool, &ThumbnailReconcileWorker{Pool: pool})
	if got := countSeekTilesJobs(t, pool, recordingID); got != 0 {
		t.Errorf("seek_tiles jobs for known-missing original = %d, want 0", got)
	}
}

// **再開位置は派生物の種類ごとに別に持つ。** 共通のカーソルにすると、thumbnail の
// 候補が常に上限に張り付いているとき（恒久失敗が先頭に居座る等）、そのカーソルが
// 進み続けて seek_tiles 側の候補を窓の外へ飛ばす。
//
// ここでは thumbnail の候補を録画 1・2 に、seek_tiles の候補を録画 3・4・5 に
// 分けてある。RowLimit=2 で 2 パス回すと、独立したカーソルなら 3・4・5 の
// 3 件すべてにジョブが積まれる。共通のカーソルだと 2 パス目が
// 「thumbnail が尽きたので 0 に戻る」→ seek_tiles の窓が 0 に戻り、5 が永久に
// 届かない（2 件のまま）ので落ちる。
func TestThumbnailReconcile_SeekTilesCursorIsIndependent(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	// 録画 1..5（event_id を 1..5 にずらす。同一キーのアクティブ行は一意制約に当たる）。
	ids := make([]int64, 0, 5)
	for i := int32(1); i <= 5; i++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, i))
	}
	for i, id := range ids {
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cursor/%d.m2ts", i+1), []byte("fake-ts"))
	}
	// thumbnail があるのは録画 3・4・5 → thumbnail の候補は 1・2。
	for _, id := range ids[2:] {
		seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil, fmt.Sprintf("cursor/t%d.jpg", id), []byte("jpg"))
	}
	// seek_tiles があるのは録画 1・2 → seek_tiles の候補は 3・4・5。
	for _, id := range ids[:2] {
		seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindSeekTiles, nil, fmt.Sprintf("cursor/s%d.jpg", id), []byte("jpg"))
	}

	w := &ThumbnailReconcileWorker{Pool: pool, RowLimit: 2}
	runThumbnailReconcilePass(t, pool, w)
	runThumbnailReconcilePass(t, pool, w)

	for _, id := range ids[:2] {
		if got := countThumbnailJobs(t, pool, id); got != 1 {
			t.Errorf("thumbnail jobs for recording %d = %d, want 1", id, got)
		}
	}
	for _, id := range ids[2:] {
		if got := countSeekTilesJobs(t, pool, id); got != 1 {
			t.Errorf("seek_tiles jobs for recording %d = %d, want 1 "+
				"(the seek_tiles cursor must not be dragged along by the thumbnail window)", id, got)
		}
	}
}
