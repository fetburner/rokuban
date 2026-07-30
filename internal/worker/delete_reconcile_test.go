package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/webhook"
)

// seedOriginalAsset は原本の active media_assets 行 + ファイルを 1 件用意する。
func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     relPath,
		SizeBytes:   int64(len(content)),
	})
	if err != nil {
		t.Fatalf("seeding original media_asset: %v", err)
	}
	return id
}

// seedEncodedOrThumbnailAsset は encoded / thumbnail の active media_assets 行 + ファイルを用意する。
func seedEncodedOrThumbnailAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, kind string, profile *string, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        kind,
		Profile:     profile,
		RelPath:     relPath,
		SizeBytes:   int64(len(content)),
	})
	if err != nil {
		t.Fatalf("seeding %s media_asset: %v", kind, err)
	}
	return id
}

func assetState(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(),
		"SELECT state FROM media_assets WHERE id = $1", id).Scan(&state); err != nil {
		t.Fatalf("querying asset state: %v", err)
	}
	return state
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ごみ箱の猶予を過ぎた録画の原本は物理削除され、行は deleted に遷移する。
func TestDeleteReconcileWorker_TrashPastRetention_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "past/retention.m2ts", []byte("data"))
	fullPath := filepath.Join(mediaDir, "past", "retention.m2ts")

	past := time.Now().Add(-40 * 24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, recordingID); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("asset state = %q, want deleted", got)
	}
	if fileExists(fullPath) {
		t.Error("file still exists on disk, want removed")
	}
}

// 猶予期間内のごみ箱録画は削除されない（TrashPastRetention の逆方向）。
func TestDeleteReconcileWorker_TrashWithinRetention_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "within/retention.m2ts", []byte("data"))
	fullPath := filepath.Join(mediaDir, "within", "retention.m2ts")

	recent := time.Now().Add(-1 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", recent, recordingID); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active (not yet past retention)", got)
	}
	if !fileExists(fullPath) {
		t.Error("file was removed, want kept (within retention)")
	}
}

// purge_after は猶予期間を無視して即時削除する。
func TestDeleteReconcileWorker_PurgeAfterImmediate_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "purge/now.m2ts", []byte("data"))

	now := time.Now()
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1, purge_after = $1 WHERE id = $2", now, recordingID); err != nil {
		t.Fatalf("marking recording for immediate purge: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("asset state = %q, want deleted (purge_after should bypass retention)", got)
	}
}

// 原本の物理削除で recording.deleted が発火すること。
func TestDeleteReconcileWorker_FiresRecordingDeletedWebhook_OnOriginalDelete(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/original.m2ts", []byte("data"))
	past := time.Now().Add(-40 * 24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, recordingID); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}

	var gotEvent webhook.Event
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading webhook body: %v", err)
		}
		if err := json.Unmarshal(body, &gotEvent); err != nil {
			t.Errorf("unmarshalling webhook body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: webhook.New(config.WebhookConfig{URL: srv.URL}),
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state = %q, want deleted", got)
	}
	if hits != 1 {
		t.Fatalf("webhook hits = %d, want 1", hits)
	}
	if gotEvent.Type != webhook.EventRecordingDeleted {
		t.Errorf("type = %q, want %q", gotEvent.Type, webhook.EventRecordingDeleted)
	}
	if gotEvent.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", gotEvent.RecordingID, recordingID)
	}
	if gotEvent.Status != "deleted" {
		t.Errorf("status = %q, want deleted", gotEvent.Status)
	}
	if gotEvent.Title != "テスト番組" {
		t.Errorf("title = %q, want %q", gotEvent.Title, "テスト番組")
	}
}

// encoded/thumbnail だけの削除では recording.deleted を発火しない
// （FiresRecordingDeletedWebhook_OnOriginalDelete の逆方向。同じ録画の派生物削除を
// 「録画そのものが消えた」と誤解させないため）。
func TestDeleteReconcileWorker_EncodedOnlyDelete_DoesNotFireRecordingDeletedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	profile := "h264"
	assetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "webhook/encoded.mp4", []byte("data"))
	past := time.Now().Add(-40 * 24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, recordingID); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: webhook.New(config.WebhookConfig{URL: srv.URL}),
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state = %q, want deleted", got)
	}
	if hits != 0 {
		t.Errorf("webhook fired for non-original asset delete, hits = %d, want 0", hits)
	}
}

// until_encoded で desired な派生物（profile + thumbnail）が揃っていれば原本を消す。
func TestDeleteReconcileWorker_UntilEncoded_Complete_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/complete.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/complete.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/complete.jpg", []byte("jpg"))

	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("original state = %q, want deleted", got)
	}
}

// 望ましいプロファイルの一部が欠けている間は原本を消さない（Complete の逆方向）。
func TestDeleteReconcileWorker_UntilEncoded_MissingProfile_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/missing-profile.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/missing-profile.jpg", []byte("jpg"))
	// h264 プロファイルは望ましいが未生成のまま。

	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active (encoded profile missing)", got)
	}
}

// サムネイルが未生成の間は原本を消さない。
func TestDeleteReconcileWorker_UntilEncoded_MissingThumbnail_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/missing-thumb.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/missing-thumb.mp4", []byte("mp4"))

	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active (thumbnail missing)", got)
	}
}

// 原本を入力とする encode ジョブが実行待ちの間は、派生物が揃って見えても消さない
// （storage.md §7 の条件 3）。
func TestDeleteReconcileWorker_UntilEncoded_PendingEncodeJob_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/pending-job.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/pending-job.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/pending-job.jpg", []byte("jpg"))

	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

	insertOnly, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only client: %v", err)
	}
	if _, err := insertOnly.Insert(context.Background(), EncodeJobArgs{RecordingID: recordingID, Profile: "av1"}, nil); err != nil {
		t.Fatalf("inserting pending encode job: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active (pending encode job for this recording)", got)
	}
}

// mtime が新しいファイルは孤児候補として記録しない。
func TestDeleteReconcileWorker_Orphan_RecentMTime_NotRegistered(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	orphanPath := filepath.Join(mediaDir, "unregistered-recent.dat")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("writing orphan file: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, OrphanMTimeGrace: 7 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", "unregistered-recent.dat").Scan(&count); err != nil {
		t.Fatalf("querying orphan_files: %v", err)
	}
	if count != 0 {
		t.Errorf("orphan_files count = %d, want 0 (mtime too recent)", count)
	}
	if !fileExists(orphanPath) {
		t.Error("recent orphan file was removed, want kept")
	}
}

// mtime が古いファイルは孤児候補として記録され、エイジング済みなら削除される。
func TestDeleteReconcileWorker_Orphan_AgedOut_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	relPath := "unregistered-old.dat"
	orphanPath := filepath.Join(mediaDir, relPath)
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("writing orphan file: %v", err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// エイジング窓を過ぎた記録として直接投入する（14 日待たずに検証するため）。
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO orphan_files (rel_path, first_seen) VALUES ($1, $2)",
		relPath, time.Now().Add(-20*24*time.Hour)); err != nil {
		t.Fatalf("seeding aged orphan record: %v", err)
	}

	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir,
		OrphanMTimeGrace: 7 * 24 * time.Hour,
		OrphanAge:        14 * 24 * time.Hour,
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if fileExists(orphanPath) {
		t.Error("aged orphan file still exists, want removed")
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&count); err != nil {
		t.Fatalf("querying orphan_files: %v", err)
	}
	if count != 0 {
		t.Errorf("orphan_files count = %d, want 0 (record cleared after delete)", count)
	}
}

// 候補数がしきい値を超えるとサーキットブレーカーが発動し、その回では何も消さない。
func TestDeleteReconcileWorker_CircuitBreaker_TripsOnExcess(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	var assetIDs []int64
	for i := range 3 {
		recordingID := insertTestRecording(t, pool)
		assetID := seedOriginalAsset(t, pool, mediaDir, recordingID,
			filepath.Join("trip", string(rune('a'+i))+".m2ts"), []byte("data"))
		assetIDs = append(assetIDs, assetID)
		past := time.Now().Add(-40 * 24 * time.Hour)
		if _, err := pool.Exec(context.Background(),
			"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, recordingID); err != nil {
			t.Fatalf("marking recording deleted: %v", err)
		}
	}

	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir,
		TrashRetention:    30 * 24 * time.Hour,
		MaxDeletesPerPass: 2, // 候補は 3 件 > 2 なので発動するはず
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	for _, id := range assetIDs {
		if got := assetState(t, pool, id); got != "active" {
			t.Errorf("asset %d state = %q, want active (breaker should have withheld deletes)", id, got)
		}
	}

	q := sqlcgen.New(pool)
	cb, err := q.GetCircuitBreaker(context.Background(), sqlcgen.GetCircuitBreakerParams{
		Site: db.DefaultSite, Name: breaker.DeleteReconcile,
	})
	if err != nil {
		t.Fatalf("expected circuit breaker to be tripped, got error: %v", err)
	}
	if cb.Pending != 3 {
		t.Errorf("breaker pending = %d, want 3", cb.Pending)
	}
}

// 前パスで deleting のまま止まった行は、ブレーカーの発動有無に関わらず再開される
// （「既に決めた削除」の再実行であり新規の判断ではないため）。
func TestDeleteReconcileWorker_ResumesStuckDeletingRow(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	relPath := "stuck/deleting.m2ts"
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	var assetID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
		VALUES ($1, 'original', $2, 4, 'deleting')
		RETURNING id`, recordingID, relPath).Scan(&assetID); err != nil {
		t.Fatalf("seeding stuck deleting row: %v", err)
	}

	// ブレーカーを発動させておく（新規候補はゼロなので影響しないはず）。
	q := sqlcgen.New(pool)
	if err := breaker.Trip(context.Background(), q, db.DefaultSite, breaker.DeleteReconcile, 0, breaker.Sample{Total: 1}); err != nil {
		t.Fatalf("tripping breaker: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("stuck asset state = %q, want deleted (should resume regardless of breaker)", got)
	}
	if fileExists(full) {
		t.Error("stuck asset file still exists, want removed")
	}
}

// 削除 reconcile は record_sweep と同じ理由で River 既定より長い上限を持つ。
func TestDeleteReconcileWorker_HasGenerousTimeout(t *testing.T) {
	w := &DeleteReconcileWorker{}
	got := w.Timeout(nil)
	if got <= time.Minute {
		t.Errorf("Timeout() = %v, want > 1m (River default)", got)
	}
}

// Kind / InsertOpts の形。UniqueOpts が無いと定期ジョブが実質ワンショット化する
// 事故（worker.go の pendingJobStates コメント参照）を防ぐための確認。
func TestDeleteReconcileArgs_KindAndQueue(t *testing.T) {
	args := DeleteReconcileArgs{}
	if args.Kind() != "delete_reconcile" {
		t.Errorf("Kind = %q", args.Kind())
	}
	opts := args.InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Error("ByArgs should be true")
	}
	if len(opts.UniqueOpts.ByState) == 0 {
		t.Error("ByState should be set to pendingJobStates (completed を含めると定期ジョブがワンショット化する)")
	}
}

// DeleteReconcile が PeriodicJobs に載ること。
func TestBuildRiverConfig_DeleteReconcilePeriodic(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:    true,
		DeleteReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 1 {
		t.Fatalf("PeriodicJobs = %d, want 1 (delete_reconcile only)", len(riverCfg.PeriodicJobs))
	}
}

// DeleteReconcile を立てないと定期ジョブに載らないこと（フラグの逆方向）。
func TestBuildRiverConfig_DeleteReconcileDisabled(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 0 {
		t.Fatalf("PeriodicJobs = %d, want 0 (DeleteReconcile not set)", len(riverCfg.PeriodicJobs))
	}
}
