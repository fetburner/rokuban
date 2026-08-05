package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// webhookRecorder は webhook 先の httptest サーバが受け取ったイベントを記録する。
// ハンドラは別 goroutine で走るので internal/webhook のテストと同じく同期して持つ。
// encode_test.go からも使う。
type webhookRecorder struct {
	mu     sync.Mutex
	events []webhook.Event
}

// newWebhookRecorder は記録用サーバを立て、そこへ送る Client を返す。
func newWebhookRecorder(t *testing.T) (*webhookRecorder, *webhook.Client) {
	t.Helper()
	rec := &webhookRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading webhook body: %v", err)
		}
		var ev webhook.Event
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("unmarshalling webhook body: %v", err)
		}
		rec.mu.Lock()
		rec.events = append(rec.events, ev)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, webhook.New(config.WebhookConfig{URL: srv.URL})
}

// received は受信したイベントのコピーを返す。
func (r *webhookRecorder) received() []webhook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webhook.Event(nil), r.events...)
}

// reset は記録を捨てる（2 パス目だけを見たいとき用）。
func (r *webhookRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// markRecordingUntilEncoded は recording_encode_policy 衛星表（issue #159）に
// keep_original='until_encoded' の行を作る/上書きするテスト用フィクスチャ。
// ingest の resolveAndSnapshotEncodePolicy が原本コミットと同一 tx で凍結する
// 内容を模す（reservations.sql は #52 並走中につき、この目的のためだけの
// 書き込みクエリを新設しない。CLAUDE.md 同種の規律）。このファイルの呼び出し元は
// すべて until_encoded 腕の削除判定を確認するテストなので keep_original を
// パラメータにしない（unparam）。本番の凍結は ON CONFLICT を持たない素の
// INSERT だが（1 録画につき 1 回しか呼ばれない前提）、ここはテストの都合上
// 何度でも呼べるように upsert にしてある。
func markRecordingUntilEncoded(t *testing.T, pool *pgxpool.Pool, recordingID int64, profiles []string) {
	t.Helper()
	if profiles == nil {
		profiles = []string{}
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'until_encoded', $2)
		 ON CONFLICT (recording_id) DO UPDATE SET
		   keep_original   = EXCLUDED.keep_original,
		   encode_profiles = EXCLUDED.encode_profiles,
		   updated_at      = now()`,
		recordingID, profiles); err != nil {
		t.Fatalf("setting recording_encode_policy: %v", err)
	}
}

// markRecordingTrashed は録画をごみ箱に入れ、猶予（既定 30 日）を過ぎた状態にする。
func markRecordingTrashed(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	past := time.Now().Add(-40 * 24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, recordingID); err != nil {
		t.Fatalf("marking recording deleted: %v", err)
	}
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

// purgedAt はある録画の recordings.purged_at を返す（issue #135）。
func purgedAt(t *testing.T, pool *pgxpool.Pool, recordingID int64) *time.Time {
	t.Helper()
	var v *time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT purged_at FROM recordings WHERE id = $1", recordingID).Scan(&v); err != nil {
		t.Fatalf("querying purged_at: %v", err)
	}
	return v
}

// inTrash は ListTrashRecordings（api が「ごみ箱は空です」判定に使う一覧そのもの）
// に指定した録画 id が含まれるかを返す。ここで直接クエリを引くことで、
// api パッケージを経由せずに「ごみ箱ビューから見えるか」を検証する
// （issue #135 の受け入れ「完全削除が完了した録画がごみ箱一覧に出ない」）。
func inTrash(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	rows, err := sqlcgen.New(pool).ListTrashRecordings(context.Background(), db.DefaultSite)
	if err != nil {
		t.Fatalf("ListTrashRecordings: %v", err)
	}
	for _, r := range rows {
		if r.ID == recordingID {
			return true
		}
	}
	return false
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
	// issue #135: 完全削除が完了したので purged_at が立ち、ごみ箱一覧から消える。
	if got := purgedAt(t, pool, recordingID); got == nil {
		t.Error("purged_at not set after the last asset was deleted")
	}
	if inTrash(t, pool, recordingID) {
		t.Error("recording still present in ListTrashRecordings, want absent (purge complete)")
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
	// 反転（issue #135 の受け入れ「purge 前はごみ箱に出続ける」）: まだ完全削除が
	// 終わっていないので purged_at は立たず、ごみ箱一覧にも出続ける。
	if got := purgedAt(t, pool, recordingID); got != nil {
		t.Errorf("purged_at = %v, want nil (asset not yet deleted)", got)
	}
	if !inTrash(t, pool, recordingID) {
		t.Error("recording missing from ListTrashRecordings, want present (purge not complete)")
	}
}

// purge_after は猶予期間を無視して即時削除する。
func TestDeleteReconcileWorker_PurgeAfterImmediate_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "purge/now.m2ts", []byte("data"))

	// SQL 側は DB の now() と比較する。Docker VM とホストの時計が数十 ms
	// ずれても確実に「今すぐ」の側へ入るよう、安全に過去の時刻を使う。
	purgeAt := time.Now().Add(-time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1, purge_after = $1 WHERE id = $2", purgeAt, recordingID); err != nil {
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

// issue #135 の実測で一番分かりにくかったケース: media_assets を 1 行も
// 持たない録画（status='failed' で ingest まで到達しなかった行など）に
// 「今すぐ完全削除」を要求すると、消す対象が無いので観測できる変化が一切
// 起きず、GC が永久に始まらないように見えていた。MarkPurgedRecordings は
// recordings を起点に引く（NOT EXISTS は 0 行に対しても恒真）ので、この
// ケースでも 1 パス目で purged_at が立ち、recording.deleted も 1 回だけ発火し、
// ごみ箱一覧から消えるべき。
func TestDeleteReconcileWorker_ZeroAssetRecording_PurgeMarksAndFiresWebhookOnce(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	// media_assets は 1 行も作らない。

	// SQL 側は DB の now() と比較する。Docker VM とホストの時計が数十 ms
	// ずれても確実に「今すぐ」の側へ入るよう、安全に過去の時刻を使う。
	purgeAt := time.Now().Add(-time.Hour)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1, purge_after = $1 WHERE id = $2", purgeAt, recordingID); err != nil {
		t.Fatalf("marking recording for immediate purge: %v", err)
	}

	// 前提確認: purge 前はごみ箱に見えているはず（アセットが無くても
	// 除外条件を「残っているアセットがある録画だけ」にしていないことの事前チェック）。
	if !inTrash(t, pool, recordingID) {
		t.Fatal("recording should be visible in trash before purge completes")
	}

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, Webhook: client}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := purgedAt(t, pool, recordingID); got == nil {
		t.Fatal("purged_at not set for a recording with zero media_assets")
	}
	if inTrash(t, pool, recordingID) {
		t.Error("zero-asset recording still present in ListTrashRecordings after purge")
	}
	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1 (zero-asset recording): %+v", len(events), events)
	}
	if events[0].RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", events[0].RecordingID, recordingID)
	}
	if events[0].Type != webhook.EventRecordingDeleted {
		t.Errorf("type = %q, want %q", events[0].Type, webhook.EventRecordingDeleted)
	}

	// 2 パス目では既に purged_at が立っているので二度と候補に上がらず、
	// webhook も再発火しない（冪等）。
	rec.reset()
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() second pass error: %v", err)
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired again on the second pass: %+v", events)
	}
}

// deleting のまま止まっている間（unlink 未完了）は、たとえごみ箱の猶予を
// 過ぎていても purged_at を立てない。判定は「state <> 'deleted' の
// media_assets が 0 行」なので、deleting は「消えた」に数えない
// （issue #135 の罠。TestDeleteReconcileWorker_TrashPastRetention_Deletes の
// 反転）。
func TestDeleteReconcileWorker_DeletingAssetStuck_StaysInTrash(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	// unlink を失敗させる: rel_path を空でないディレクトリに置き換える。
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "purged/stuck.m2ts", []byte("data"))
	assetPath := filepath.Join(mediaDir, "purged", "stuck.m2ts")
	if err := os.Remove(assetPath); err != nil {
		t.Fatalf("removing seeded file: %v", err)
	}
	if err := os.MkdirAll(assetPath, 0o755); err != nil {
		t.Fatalf("creating blocking dir: %v", err)
	}
	blockerFile := filepath.Join(assetPath, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	markRecordingTrashed(t, pool, recordingID)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleting" {
		t.Fatalf("asset state = %q, want deleting (unlink blocked)", got)
	}
	if got := purgedAt(t, pool, recordingID); got != nil {
		t.Errorf("purged_at = %v, want nil (an asset is still stuck in deleting)", got)
	}
	if !inTrash(t, pool, recordingID) {
		t.Error("recording missing from ListTrashRecordings while an asset is still deleting")
	}
}

// ごみ箱の録画を完全削除したら recording.deleted が 1 回だけ発火すること
// （アセット 3 件を消しても 1 通。発火は録画単位）。
func TestDeleteReconcileWorker_TrashPurge_FiresRecordingDeletedWebhookOnce(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/original.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "webhook/encoded.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "webhook/thumb.jpg", []byte("jpg"))
	markRecordingTrashed(t, pool, recordingID)

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: client,
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state = %q, want deleted", got)
	}
	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1: %+v", len(events), events)
	}
	got := events[0]
	if got.Type != webhook.EventRecordingDeleted {
		t.Errorf("type = %q, want %q", got.Type, webhook.EventRecordingDeleted)
	}
	if got.RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", got.RecordingID, recordingID)
	}
	if got.Status != "deleted" {
		t.Errorf("status = %q, want deleted", got.Status)
	}
	if got.Title != "テスト番組" {
		t.Errorf("title = %q, want %q", got.Title, "テスト番組")
	}
	if got.Profile != "" {
		t.Errorf("profile = %q, want empty (recording.deleted は録画単位)", got.Profile)
	}
}

// 原本を先に消してある録画（until_encoded 済み）をごみ箱経由で完全削除しても
// recording.deleted が発火すること。発火条件をアセットの kind に取ると、この
// 経路では原本が既に無いので一度も発火しない。
func TestDeleteReconcileWorker_TrashPurgeWithoutOriginal_FiresRecordingDeletedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	// 原本は until_encoded で既に物理削除済み（行は deleted で残る）。
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/gone.m2ts", []byte("data"))
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", originalID); err != nil {
		t.Fatalf("marking original deleted: %v", err)
	}
	if err := os.Remove(filepath.Join(mediaDir, "webhook", "gone.m2ts")); err != nil {
		t.Fatalf("removing original file: %v", err)
	}

	profile := "h264"
	encodedID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "webhook/encoded.mp4", []byte("mp4"))
	markRecordingTrashed(t, pool, recordingID)

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: client,
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, encodedID); got != "deleted" {
		t.Fatalf("encoded state = %q, want deleted", got)
	}
	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events = %d, want 1 (原本が既に無い録画の完全削除): %+v", len(events), events)
	}
	if events[0].Type != webhook.EventRecordingDeleted {
		t.Errorf("type = %q, want %q", events[0].Type, webhook.EventRecordingDeleted)
	}
}

// until_encoded の原本削除では recording.deleted を発火しない（録画は生きていて
// encoded で再生できる。TrashPurge_FiresRecordingDeletedWebhookOnce の逆方向）。
func TestDeleteReconcileWorker_UntilEncodedOriginalPurge_DoesNotFireRecordingDeletedWebhook(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/until-encoded.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "webhook/ue-encoded.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "webhook/ue-thumb.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, Webhook: client}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("original state = %q, want deleted", got)
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired for a live recording: %+v", events)
	}
}

// 削除の進行中（deleting）にごみ箱から復元された録画は、次パスでファイルが
// 消えない（issue #105）。復元は recordings.deleted_at だけを消す
// （internal/api RestoreRecording）ので media_assets.state は deleting の
// まま残る。ここで pending 経路が判定を再評価せず「既に決めた削除」として
// 無条件に続行すると、「復元しました」と表示された直後にファイルが消える
// （issue #105 の失敗シナリオそのもの）。ListUnqualifiedDeletingAssets /
// resolveUnqualifiedDeletingAsset がこの行を active に戻すので、pending
// 経路には現れずファイルは残る。recording.deleted も発火しない（録画は
// 生きている）。
func TestDeleteReconcileWorker_RestoredWhileDeleting_RevertsInsteadOfDeleting(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	// 原本 1 件だけ。unlink を 1 パス目だけ失敗させる（中身のあるディレクトリ）。
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/restore/original.m2ts", []byte("data"))
	assetPath := filepath.Join(mediaDir, "webhook", "restore", "original.m2ts")
	if err := os.Remove(assetPath); err != nil {
		t.Fatalf("removing seeded file: %v", err)
	}
	if err := os.MkdirAll(assetPath, 0o755); err != nil {
		t.Fatalf("creating blocking dir: %v", err)
	}
	blockerFile := filepath.Join(assetPath, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	markRecordingTrashed(t, pool, recordingID)

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: client,
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if got := assetState(t, pool, assetID); got != "deleting" {
		t.Fatalf("asset state = %q, want deleting", got)
	}

	// ここでユーザーがごみ箱から復元する。
	if _, err := sqlcgen.New(pool).RestoreRecording(context.Background(), recordingID); err != nil {
		t.Fatalf("restoring recording: %v", err)
	}
	if err := os.Remove(blockerFile); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}
	rec.reset()

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() second pass error: %v", err)
	}
	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("asset state after second pass = %q, want active (restored recordings must not be deleted, issue #105)", got)
	}
	if !fileExists(assetPath) {
		t.Error("file was removed after restore, want kept (issue #105)")
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired for a restored recording: %+v", events)
	}
}

// unlink 自体は成功したが MarkMediaAssetDeleted がコミットされる前にプロセスが
// 落ち、その間に復元された場合は、active には戻さず deleted を確定する
// （issue #105 のコードレビューで指摘された狭い窓）。ここで無条件に active へ
// 戻すと、案 B（復元時に deleting を同期的に active へ戻す）を却下した理由
// そのもの ——「active なのにファイルが無い行」を作ってしまう。
func TestDeleteReconcileWorker_UnlinkedButUncommittedThenRestored_FinalizesAsDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/crash/original.m2ts", []byte("data"))
	assetPath := filepath.Join(mediaDir, "webhook", "crash", "original.m2ts")
	markRecordingTrashed(t, pool, recordingID)

	// 前パスで unlink までは成功したがプロセスが落ち、MarkMediaAssetDeleted が
	// コミットされなかった状態を直接作る（deleting のままファイルだけが無い）。
	if err := os.Remove(assetPath); err != nil {
		t.Fatalf("simulating a completed unlink: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleting' WHERE id = $1", assetID); err != nil {
		t.Fatalf("marking asset deleting: %v", err)
	}

	// ここでユーザーがごみ箱から復元する。
	if _, err := sqlcgen.New(pool).RestoreRecording(context.Background(), recordingID); err != nil {
		t.Fatalf("restoring recording: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state = %q, want deleted (file already gone; reverting to active would create an active row with no file)", got)
	}
}

// アセットが 1 件でも消しきれていないうちは発火せず、次パスで最後の 1 件が
// 消えた時点で発火すること（pending 経路からの発火も兼ねる）。
func TestDeleteReconcileWorker_TrashPurge_FiresOnlyAfterLastAsset(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/last/original.m2ts", []byte("data"))

	// unlink を失敗させる: rel_path をファイルではなく、中身のあるディレクトリにする
	// （os.Remove は空でないディレクトリを消せない）。
	profile := "h264"
	blockedID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "webhook/last/blocked.mp4", []byte("mp4"))
	blockedPath := filepath.Join(mediaDir, "webhook", "last", "blocked.mp4")
	if err := os.Remove(blockedPath); err != nil {
		t.Fatalf("removing seeded file: %v", err)
	}
	if err := os.MkdirAll(blockedPath, 0o755); err != nil {
		t.Fatalf("creating blocking dir: %v", err)
	}
	blockerFile := filepath.Join(blockedPath, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	markRecordingTrashed(t, pool, recordingID)

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: client,
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, blockedID); got != "deleting" {
		t.Fatalf("blocked asset state = %q, want deleting", got)
	}
	if events := rec.received(); len(events) != 0 {
		t.Fatalf("fired while an asset was still not deleted: %+v", events)
	}

	// 障害を除けば次パスの pending 経路が拾い直し、そこで初めて発火する。
	if err := os.Remove(blockerFile); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}
	rec.reset()
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() second pass error: %v", err)
	}

	if got := assetState(t, pool, blockedID); got != "deleted" {
		t.Fatalf("blocked asset state after second pass = %q, want deleted", got)
	}
	events := rec.received()
	if len(events) != 1 {
		t.Fatalf("webhook events on second pass = %d, want 1: %+v", len(events), events)
	}
	if events[0].RecordingID != recordingID {
		t.Errorf("recordingId = %d, want %d", events[0].RecordingID, recordingID)
	}
}

// webhook 先がハングしても削除そのものは完了すること（issue #73 の受け入れ基準
// 「webhook 先が落ちていても本処理は完了する」の削除 reconcile 版）。
func TestDeleteReconcileWorker_HangingWebhook_StillDeletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/hang.m2ts", []byte("data"))
	markRecordingTrashed(t, pool, recordingID)

	// クライアント側の timeout を必ず超える程度に応答を遅らせる。上限を切らないと
	// クライアントが諦めても接続が閉じるまでハンドラが残り、Close が待たされる。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	w := &DeleteReconcileWorker{
		Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour,
		Webhook: webhook.New(config.WebhookConfig{URL: srv.URL, Timeout: 50 * time.Millisecond}),
	}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v (webhook の失敗で削除パスを落としてはいけない)", err)
	}
	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("asset state = %q, want deleted", got)
	}
}

// 通知の時間予算を使い切ったら残りは捨てる（削除パス全体を webhook で
// タイムアウトさせないための安全弁）。
func TestDeleteReconcileWorker_NotifyBudgetExhausted_DropsRemaining(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "webhook/budget.m2ts", []byte("data"))
	markRecordingTrashed(t, pool, recordingID)
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1", recordingID); err != nil {
		t.Fatalf("marking assets deleted: %v", err)
	}

	rec, client := newWebhookRecorder(t)
	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, Webhook: client}
	// notifyPurgedRecordings はもう「発火してよいか」を計算し直さない
	// （issue #135 —— それを MarkPurgedRecordings に一本化したのがこの issue の
	// 主旨）。ここでは渡した集合がそのまま発火対象になることを前提に、予算
	// だけを検証する。
	purged := []sqlcgen.MarkPurgedRecordingsRow{{ID: recordingID, Site: "default", Title: "テスト番組"}}
	// 予算 0 = 1 件目から捨てる。
	w.notifyPurgedRecordings(context.Background(), purged, 0)
	if events := rec.received(); len(events) != 0 {
		t.Errorf("notified despite an exhausted budget: %+v", events)
	}

	w.notifyPurgedRecordings(context.Background(), purged, time.Minute)
	if events := rec.received(); len(events) != 1 {
		t.Fatalf("webhook events with a budget = %d, want 1: %+v", len(events), events)
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

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("original state = %q, want deleted", got)
	}
}

// dropUntilEncodedRequiresProfilesCheck は recording_encode_policy（issue #159。
// 00020 から移設した CHECK 制約）の CHECK 制約を一時的に外す。encode_profiles が
// 空の until_encoded 行は通常この CHECK に阻まれて作れないが、issue #104 の
// 削除クエリ側ガード（until_encoded_deletable_originals view の
// cardinality(encode_profiles) > 0）は「CHECK に頼らない」独立した防御として
// 足したものなので、CHECK が無い前提でもそれ単体で原本を守れることを確認する
// 必要がある。テスト用パッケージ DB は TRUNCATE のみでスキーマは使い回すため、
// t.Cleanup で必ず元に戻し他のテストに影響しないようにする。
func dropUntilEncodedRequiresProfilesCheck(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		"ALTER TABLE recording_encode_policy DROP CONSTRAINT recording_encode_policy_check"); err != nil {
		t.Fatalf("dropping check constraint: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// テスト本体が作った違反行（keep_original='until_encoded' かつ
		// encode_profiles が空）が残ったままだと ADD CONSTRAINT 自体が失敗する。
		// 次のテストは TRUNCATE で行ごと消えるが、この関数はその前に走るので
		// 先に直しておく（00020 マイグレーションの Up が既存行にしているのと
		// 同じ「安全側に倒す」処理）。
		if _, err := pool.Exec(cleanupCtx,
			"UPDATE recording_encode_policy SET keep_original = 'always' "+
				"WHERE keep_original = 'until_encoded' AND cardinality(encode_profiles) = 0"); err != nil {
			t.Fatalf("fixing up rows before restoring check constraint: %v", err)
		}
		if _, err := pool.Exec(cleanupCtx,
			"ALTER TABLE recording_encode_policy ADD CONSTRAINT recording_encode_policy_check "+
				"CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0)"); err != nil {
			t.Fatalf("restoring check constraint: %v", err)
		}
	})
}

// バグの再現ケース（issue #104）: keep_original='until_encoded' かつ
// encode_profiles='{}' で、望ましい派生物がサムネイルしか無い（= encode_profiles
// が空なので unnest() が 0 行になり、欠けているプロファイルが「1 つもない」と
// 恒真判定される）録画は、原本が唯一のコピーである（docs/storage.md §6）にも
// かかわらず、修正前は削除されてしまっていた。
func TestDeleteReconcileWorker_UntilEncoded_EmptyProfiles_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/empty-profiles.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/empty-profiles.jpg", []byte("jpg"))
	// encoded アセットは 1 つも無い。encode_profiles も空のまま。

	dropUntilEncodedRequiresProfilesCheck(t, pool)
	if _, err := pool.Exec(ctx,
		"INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'until_encoded', '{}')",
		recordingID); err != nil {
		t.Fatalf("setting keep_original with empty encode_profiles: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active "+
			"(encode_profiles is empty; must not be treated as \"all desired profiles present\")", got)
	}
}

// recording_encode_policy の CHECK 自体が効いていることの確認（issue #104 の
// 含むもの 3、CLAUDE.md 不変条件 10「表現不可能にする」。00020 から issue #159 で
// この衛星表へ移設）。until_encoded に切り替えるときだけプロファイルを要求する
// 形になっているはず。両方向を確認する: 空プロファイルは拒否され、プロファイルを
// 添えれば通る。
func TestRecordingEncodePolicy_UntilEncodedRequiresProfilesCheck(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	recordingID := insertTestRecording(t, pool)

	if _, err := pool.Exec(ctx,
		"INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'until_encoded', '{}')",
		recordingID); err == nil {
		t.Fatal("expected a CHECK violation for until_encoded with empty encode_profiles, got no error")
	}

	if _, err := pool.Exec(ctx,
		"INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'until_encoded', $2)",
		recordingID, []string{"h264"}); err != nil {
		t.Fatalf("expected until_encoded with a non-empty profile list to be allowed, got error: %v", err)
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

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

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

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active (thumbnail missing)", got)
	}
}

// 原本を入力とする encode ジョブが実行待ちの間は、派生物が揃って見えても消さない
// （storage.md §6 の条件 3）。
func TestDeleteReconcileWorker_UntilEncoded_PendingEncodeJob_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/pending-job.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/pending-job.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/pending-job.jpg", []byte("jpg"))

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

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

// until_encoded の原本削除が unlink 失敗で deleting のまま中断しても、録画が
// 復元されたわけではない（trash とは無関係）ので、次パスで従来どおり削除が
// 再開される（issue #105。RestoredWhileDeleting_RevertsInsteadOfDeleting の
// 逆方向 —— 再評価条件を「ごみ箱」だけに書くと、この until_encoded 経路が
// 永久に deleting のまま止まるので、両方向で固定する）。
//
// 2 パス目の直前でサーキットブレーカーを発動させておくのが肝。そうしないと、
// 「pending 経路が再評価で拾い直す」変異を注入して壊しても、同じパスの
// 通常経路（ListUntilEncodedOriginalsToDelete、ブレーカー対象）がこの行を
// active 経由で再度拾って同じ最終状態（deleted・ファイル消滅）に収束してしまい、
// テストが回帰を検出できない（コードレビューで実証済み）。ブレーカーを
// 発動させておけば、正しい実装だけがブレーカー免除の pending 経路で削除を
// 完遂でき、変異を注入した実装は通常経路がブレーカーに止められて
// active のまま残るので FAIL する。
func TestDeleteReconcileWorker_UntilEncodedDeletingInterrupted_ResumesOnNextPass(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/interrupted.m2ts", []byte("data"))
	assetPath := filepath.Join(mediaDir, "orig", "interrupted.m2ts")
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/interrupted.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/interrupted.jpg", []byte("jpg"))

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

	// 1 パス目の unlink を失敗させる（中身のあるディレクトリで置き換える）。
	if err := os.Remove(assetPath); err != nil {
		t.Fatalf("removing seeded file: %v", err)
	}
	if err := os.MkdirAll(assetPath, 0o755); err != nil {
		t.Fatalf("creating blocking dir: %v", err)
	}
	blockerFile := filepath.Join(assetPath, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if got := assetState(t, pool, assetID); got != "deleting" {
		t.Fatalf("asset state = %q, want deleting (unlink blocked)", got)
	}

	// 障害を取り除く。復元は起きていない —— 録画は生きたまま until_encoded
	// のポリシーで削除が進行中なだけ。
	if err := os.Remove(blockerFile); err != nil {
		t.Fatalf("removing blocker file: %v", err)
	}

	// ブレーカーを発動させ、通常経路（ブレーカー対象）を完全に止める。
	// pending 経路（ブレーカー対象外）だけがこの行を削除できる状態にする。
	q := sqlcgen.New(pool)
	if err := breaker.Trip(context.Background(), q, db.DefaultSite, breaker.DeleteReconcile, 0, breaker.Sample{Total: 1}); err != nil {
		t.Fatalf("tripping breaker: %v", err)
	}

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() second pass error: %v", err)
	}
	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state after second pass = %q, want deleted (until_encoded deletion should resume via the breaker-exempt pending path, issue #105)", got)
	}
	if fileExists(assetPath) {
		t.Error("original file still exists after resumed deletion, want removed")
	}
}

// until_encoded 腕の否定形（deleting のまま止まっている行が、その後の変化で
// 派生物完備の条件を満たさなくなった）を active に戻す経路（issue #105 の
// 否定形。RestoredWhileDeleting_RevertsInsteadOfDeleting は trash 腕しか
// 検証していなかった）。issue #160 で否定形を手保守の NOT(...) から名前付き
// 述語（until_encoded_deletable_originals）への NOT EXISTS に統一したので、
// この経路が正しく動くことを until_encoded 側でも固定する。
func TestDeleteReconcileWorker_UntilEncodedUnqualifiedWhileDeleting_RevertsToActive(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/unqualify.m2ts", []byte("data"))
	assetPath := filepath.Join(mediaDir, "orig", "unqualify.m2ts")
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile, "enc/unqualify.mp4", []byte("mp4"))
	thumbID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/unqualify.jpg", []byte("jpg"))

	markRecordingUntilEncoded(t, pool, recordingID, []string{"h264"})

	// 前パスで active → deleting へ遷移させた（unlink はまだ）状態を直接作る。
	// この時点では派生物が揃っており、遷移は正しい判断だった。
	if _, err := pool.Exec(ctx,
		"UPDATE media_assets SET state = 'deleting' WHERE id = $1", assetID); err != nil {
		t.Fatalf("marking original deleting: %v", err)
	}

	// その後サムネイルが失われる（記録・ファイルとも）。もう
	// until_encoded_deletable_originals の述語を満たさない。
	if _, err := pool.Exec(ctx, "DELETE FROM media_assets WHERE id = $1", thumbID); err != nil {
		t.Fatalf("removing thumbnail asset row: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Fatalf("original state = %q, want active "+
			"(thumbnail no longer active; until_encoded predicate no longer qualifies this asset for deletion, issue #160)", got)
	}
	if !fileExists(assetPath) {
		t.Error("original file was removed even though the until_encoded predicate no longer qualifies it for deletion")
	}
}

// delete_reconcile.sql の 5 クエリが、削除可否の 2 腕（ごみ箱 / until_encoded）
// を名前付き述語（00029_delete_reconcile_predicates.sql の view / 関数）への
// 参照に統一していることの静的チェック（issue #160）。生の predicate テキスト
// （キー列や cardinality ガード）がクエリファイルに再度インライン化されると、
// このテストが機械的に検出する —— 「〜と同条件」というコメントで揃える
// 義務が復活していないかどうかも合わせて見る。
func TestDeleteReconcileQueries_ReferenceNamedPredicatesNotDuplicatedText(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "db", "queries", "delete_reconcile.sql"))
	if err != nil {
		t.Fatalf("reading delete_reconcile.sql: %v", err)
	}
	text := string(src)

	// 生の predicate（列参照込み）が再インライン化されていないこと。
	for _, needle := range []string{
		"r.keep_original",
		"cardinality(r.encode_profiles)",
		"r.purge_after",
		"r.deleted_at IS NOT NULL",
	} {
		if strings.Contains(text, needle) {
			t.Errorf("delete_reconcile.sql contains %q; the predicate should live only in the named view/function (00027 migration), not be re-inlined into a consumer query", needle)
		}
	}

	// 「同条件」を手で揃える必要があった旧コメントが復活していないこと。
	for _, needle := range []string{"と同条件", "同条件を再掲"} {
		if strings.Contains(text, needle) {
			t.Errorf("delete_reconcile.sql still has a %q comment; naming the predicate should have removed the need to keep duplicate WHERE clauses in sync", needle)
		}
	}

	// 5 つの消費クエリすべてが名前付き述語を参照していること。
	if got := strings.Count(text, "until_encoded_deletable_originals"); got < 5 {
		t.Errorf("delete_reconcile.sql references until_encoded_deletable_originals %d times, want at least 5 (all 5 consumer queries named in issue #160)", got)
	}
	if got := strings.Count(text, "trash_deletable_recordings"); got < 5 {
		t.Errorf("delete_reconcile.sql references trash_deletable_recordings %d times, want at least 5 (all 5 consumer queries named in issue #160)", got)
	}
}

// insertTestRecordingWithEventID は insertTestRecording と同じ内容の録画を、
// event_id だけ差し替えて作る。recordings_unique_active_event は
// (site, network_id, service_id, event_id) のアクティブ行（deleted_at IS NULL）に
// 対する一意制約なので、同一テスト内で複数のアクティブな録画を用意するには
// event_id を分ける必要がある。
func insertTestRecordingWithEventID(t *testing.T, pool *pgxpool.Pool, eventID int32) int64 {
	t.Helper()
	q := sqlcgen.New(pool)
	id, err := q.CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           eventID,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    time.Now(),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("inserting test recording: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM drop_stats WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1)", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM media_assets WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM record_sync WHERE recording_id = $1", id)
		_, _ = pool.Exec(context.Background(), "DELETE FROM recordings WHERE id = $1", id)
	})
	return id
}

// until_encoded 候補が複数（別々の録画）ある場合、pending なジョブは
// それを持つ録画だけをブロックし、他の候補の削除を巻き込まないこと
// （issue #110: pendingDerivativeJobRecordingIDs が候補ごとの recording_id を
// 正しく対応付けているかの検証。1 件でも pending があれば全候補を一律に
// ブロックする「broadcast」変異を通してしまうと、1 本の詰まった encode
// ジョブで無関係な録画の until_encoded 削除まで無言で永久に止まる）。
func TestDeleteReconcileWorker_UntilEncoded_PendingJobOnOneOfMultipleCandidates_OnlyBlocksThatOne(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	profile := "h264"

	// recordingA: pending な encode ジョブを持つ。desired なプロファイルは
	// 揃っているが、別プロファイル（av1）の再エンコードが待機中。
	recordingA := insertTestRecording(t, pool)
	assetA := seedOriginalAsset(t, pool, mediaDir, recordingA, "orig/multi-a.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingA, db.AssetKindEncoded, &profile, "enc/multi-a.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingA, db.AssetKindThumbnail, nil, "thumb/multi-a.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingA, []string{"h264"})

	// recordingB: 派生物は揃っており、pending なジョブは無い。event_id を
	// insertTestRecording の既定（1）とずらす —— recordings_unique_active_event は
	// (site, network_id, service_id, event_id) のアクティブ行に対する一意制約なので、
	// 同じ event_id で 2 つ目のアクティブな録画を insertTestRecording で作ろうとすると
	// そのまま衝突する。
	recordingB := insertTestRecordingWithEventID(t, pool, 2)
	assetB := seedOriginalAsset(t, pool, mediaDir, recordingB, "orig/multi-b.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindEncoded, &profile, "enc/multi-b.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindThumbnail, nil, "thumb/multi-b.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingB, []string{"h264"})

	insertOnly, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only client: %v", err)
	}
	if _, err := insertOnly.Insert(context.Background(), EncodeJobArgs{RecordingID: recordingA, Profile: "av1"}, nil); err != nil {
		t.Fatalf("inserting pending encode job for recording A: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetA); got != "active" {
		t.Errorf("recording A original state = %q, want active (pending encode job for recording A)", got)
	}
	if got := assetState(t, pool, assetB); got != "deleted" {
		t.Errorf("recording B original state = %q, want deleted (no pending job for recording B, must not be blocked by A's)", got)
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
//
// ただし issue #105 以降、pending 経路は無条件に再開するのではなく trash /
// until_encoded の判定を再評価するようになった。この行が「まだ削除して
// よい」と言えるのは録画がごみ箱の猶予を過ぎているからなので、それを
// markRecordingTrashed で明示する（無いと resolveUnqualifiedDeletingAsset が
// active に戻してしまい、このテストの主張が成り立たない）。
func TestDeleteReconcileWorker_ResumesStuckDeletingRow(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	markRecordingTrashed(t, pool, recordingID)

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

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
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
