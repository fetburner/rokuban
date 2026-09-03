package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
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
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindEncoded, &profile,
		"past/retention_h264.mp4", []byte("encoded"))
	subtitlePath := filepath.Join(mediaDir, "past", "retention_h264.vtt")
	if err := os.WriteFile(subtitlePath, []byte("WEBVTT\n\n"), 0o644); err != nil {
		t.Fatalf("writing subtitle sidecar: %v", err)
	}

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
	if fileExists(subtitlePath) {
		t.Error("subtitle sidecar still exists on disk, want removed with encoded asset")
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

// 即時削除の要求（recording_purge_requests の行）は猶予期間を無視して削除する。
// deleted_at はまだ猶予（30 日）の中の「たった今」にしておき、要求の行**単独**が
// 猶予をバイパスすることを確認する --- 述語の即時腕を時刻比較に戻すと
// （deleted_at が猶予内なので）削除されず落ちる。
func TestDeleteReconcileWorker_PurgeRequestedImmediate_Deletes(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "purge/now.m2ts", []byte("data"))

	recent := time.Now()
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", recent, recordingID); err != nil {
		t.Fatalf("soft-deleting recording: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO recording_purge_requests (recording_id) VALUES ($1)", recordingID); err != nil {
		t.Fatalf("requesting immediate purge: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, TrashRetention: 30 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Errorf("asset state = %q, want deleted (an immediate purge request should bypass retention even though deleted_at is recent)", got)
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

	recent := time.Now()
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = $1 WHERE id = $2", recent, recordingID); err != nil {
		t.Fatalf("soft-deleting recording: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO recording_purge_requests (recording_id) VALUES ($1)", recordingID); err != nil {
		t.Fatalf("requesting immediate purge: %v", err)
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

// dropUntilEncodedRequiresProfilesCheck は recording_encode_policy の CHECK 制約
// recording_encode_policy_check（issue #159 で recordings 側の CHECK から
// 移設）を一時的に外す。encode_profiles が
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
		// 先に直しておく（recording_encode_policy_check の前身である recordings
		// 側の同種 CHECK を足したときの Up が既存行にしているのと同じ
		// 「安全側に倒す」処理）。
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
// 含むもの 3、CLAUDE.md 不変条件 10「表現不可能にする」。recordings 側の CHECK
// から issue #159 でこの衛星表へ移設）。until_encoded に切り替えるときだけプロファイルを要求する
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

// until_encoded 候補が複数（別々の録画）ある場合、条件 2（desired な派生物の
// 完備）を満たさない録画だけを除外し、満たす録画はすべて正しく削除すること。
//
// 旧条件 3（キューの pending 状態を直接見るガード）は issue #516 で削除した ---
// この安全性は until_encoded_deletable_originals（条件 2）が代わりに担保する。
// recordingA は desired なプロファイル h264 が未コミットのまま encode ジョブが
// 実行中（ジョブの有無そのものは判定に効かない --- 添えるのは「たまたま」で
// はなく実際に active な試行が走っている状況を再現するため）。条件 2 は SQL
// 側（view）で判定するので A は untilEncodedRows に一度も現れない --- した
// がって条件 2 側の完備候補を recordingB 1 件にすると、until_encoded ループが
// 複数件のうち先頭 1 件だけを処理して残りを無視する変異を注入しても
// untilEncodedRows の要素数が 1 のままで検出できない。recordingC も条件 2 を
// 満たす 2 件目の完備候補として加え、ループが両方を処理することまで固定する。
func TestDeleteReconcileWorker_UntilEncoded_PartialCandidates_OnlyDeletesComplete(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	profile := "h264"

	// recordingA: h264 が未コミット（encode ジョブが実行中）+ サムネイルのみ
	// コミット済み。条件 2 を満たさないので原本は残る。
	recordingA := insertTestRecording(t, pool)
	assetA := seedOriginalAsset(t, pool, mediaDir, recordingA, "orig/partial-a.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingA, db.AssetKindThumbnail, nil, "thumb/partial-a.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingA, []string{profile})

	insertOnly, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only client: %v", err)
	}
	if _, err := insertOnly.Insert(context.Background(), EncodeJobArgs{RecordingID: recordingA, Profile: profile}, nil); err != nil {
		t.Fatalf("inserting active encode job for recording A: %v", err)
	}

	// recordingB / recordingC: どちらも h264 + サムネイルがコミット済みで
	// 条件 2 を満たす（削除されるべき）候補が 2 件同時に存在する状態を作る。
	// event_id を insertTestRecording の既定（1）とずらす --- recordings_unique_active_event
	// は (site, network_id, service_id, event_id) のアクティブ行に対する一意
	// 制約なので、同じ event_id で 2 つ目以降のアクティブな録画を
	// insertTestRecording で作ろうとするとそのまま衝突する。
	recordingB := insertTestRecordingWithEventID(t, pool, 2)
	assetB := seedOriginalAsset(t, pool, mediaDir, recordingB, "orig/partial-b.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindEncoded, &profile, "enc/partial-b.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindThumbnail, nil, "thumb/partial-b.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingB, []string{profile})

	recordingC := insertTestRecordingWithEventID(t, pool, 3)
	assetC := seedOriginalAsset(t, pool, mediaDir, recordingC, "orig/partial-c.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingC, db.AssetKindEncoded, &profile, "enc/partial-c.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingC, db.AssetKindThumbnail, nil, "thumb/partial-c.jpg", []byte("jpg"))
	markRecordingUntilEncoded(t, pool, recordingC, []string{profile})

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetA); got != "active" {
		t.Errorf("recording A original state = %q, want active (desired profile h264 not yet committed)", got)
	}
	if got := assetState(t, pool, assetB); got != "deleted" {
		t.Errorf("recording B original state = %q, want deleted (all desired derivatives committed)", got)
	}
	if got := assetState(t, pool, assetC); got != "deleted" {
		t.Errorf("recording C original state = %q, want deleted (all desired derivatives committed)", got)
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
	// delete_reconcile は site を持たないブレーカーなので site 列は空文字列
	// （breaker.IsSiteless のコメント参照）。
	q := sqlcgen.New(pool)
	if err := breaker.Trip(context.Background(), q, "", breaker.DeleteReconcile, 0, breaker.Sample{Total: 1}); err != nil {
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
// を名前付き述語（until_encoded_deletable_originals view /
// trash_deletable_recordings 関数）への参照に統一していることの静的チェック
// （issue #160）。生の predicate テキスト
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
		"FROM recording_purge_requests",
		"r.deleted_at IS NOT NULL",
	} {
		if strings.Contains(text, needle) {
			t.Errorf("delete_reconcile.sql contains %q; the predicate should live only in the named view/function (until_encoded_deletable_originals / trash_deletable_recordings), not be re-inlined into a consumer query", needle)
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

// TestDeleteReconcileWorker_MixedSitePrefixTree_NoFalseOrphans は、issue #186
// (M4-14) の受け入れ「前置前に ingest 済みの既存行がそのまま削除 reconcile の
// 対象であり続ける」を、コードを読んだ論証だけでなく実測で固定する。
//
// M4-14 は新規 ingest 分だけ rel_path に "sites/{site}/" を前置し、既存行は
// 移行しない（マイグレーションを書かない）ため、本番のメディアディレクトリは
// 前置あり ("sites/tokyo/...") と前置なし ("legacy/...") が混在するツリーに
// なる。walkMediaFiles（internal/worker/delete_reconcile.go）は catalog.Subdir
// の完全一致だけを SkipDir する汎用的な Walk で、site 由来の階層を特別扱いして
// いないので理屈上は無関係のはずだが、将来 catalog.Subdir の判定が
// strings.HasPrefix 等に緩められた場合に "sites/" を誤って引っ掛ける退行を防ぐ
// ガードとしてここに固定する。
//
// **両方向を確認する**（CLAUDE.md「分岐を直したら両方向で確認する」）:
// 登録済み資産だけを置くテストは「false orphan を作らない」しか検出できず、
// walkMediaFiles が "sites/" 配下を丸ごと SkipDir する退行（= 登録済み資産が
// 単に走査から消える）を見逃す --- 登録済み資産しか置かない場合、走査から
// 消えても「orphan_files が空」という結論は変わらないため。そのため
// "sites/tokyo/" 配下に**未登録**の古い mtime のファイルも 1 つ置き、それが
// orphan_files に**現れること**（= true orphan を見落とさない）も確認する。
func TestDeleteReconcileWorker_MixedSitePrefixTree_NoFalseOrphans(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	legacyRecordingID := insertTestRecording(t, pool)
	legacyAssetID := seedOriginalAsset(t, pool, mediaDir, legacyRecordingID,
		"legacy/original.m2ts", []byte("legacy data"))

	prefixedRecordingID := insertTestRecordingForSite(t, pool, "tokyo", 9001)
	prefixedAssetID := seedOriginalAsset(t, pool, mediaDir, prefixedRecordingID,
		"sites/tokyo/new/original.m2ts", []byte("prefixed data"))

	// media_assets に登録されていない、sites/tokyo/ 配下の孤児ファイル。
	// walkMediaFiles が "sites/" を（例えば info.Name() == "sites" の SkipDir
	// のような変異で）丸ごと除外していないかを検出するための正例。
	const trueOrphanRel = "sites/tokyo/orphan.dat"
	trueOrphanPath := filepath.Join(mediaDir, filepath.FromSlash(trueOrphanRel))
	if err := os.MkdirAll(filepath.Dir(trueOrphanPath), 0o755); err != nil {
		t.Fatalf("mkdir for true orphan: %v", err)
	}
	if err := os.WriteFile(trueOrphanPath, []byte("nobody references this"), 0o644); err != nil {
		t.Fatalf("writing true orphan file: %v", err)
	}

	// mtime を古くする。orphan 候補判定は mtime が新しい間はスキップするため
	// （TestDeleteReconcileWorker_Orphan_RecentMTime_NotRegistered 参照）、
	// mtime が新しいままだと rel_path の突き合わせが実際に走らずこのテストが
	// 何も保証しないテストになる。3 ファイルとも古くして突き合わせ自体を
	// 通過させることで、「walkMediaFiles が返す relPath と media_assets.rel_path
	// の突き合わせが両方の形（前置あり/なし）で成立する」ことを固定する。
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, rel := range []string{
		"legacy/original.m2ts",
		filepath.Join("sites", "tokyo", "new", "original.m2ts"),
		filepath.FromSlash(trueOrphanRel),
	} {
		if err := os.Chtimes(filepath.Join(mediaDir, rel), old, old); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, OrphanMTimeGrace: 7 * 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, legacyAssetID); got != "active" {
		t.Errorf("legacy (unprefixed) asset state = %q, want active (must survive orphan sweep)", got)
	}
	if got := assetState(t, pool, prefixedAssetID); got != "active" {
		t.Errorf("sites/-prefixed asset state = %q, want active (must survive orphan sweep)", got)
	}
	if !fileExists(filepath.Join(mediaDir, "legacy", "original.m2ts")) {
		t.Error("legacy (unprefixed) file was removed, want kept")
	}
	if !fileExists(filepath.Join(mediaDir, "sites", "tokyo", "new", "original.m2ts")) {
		t.Error("sites/-prefixed file was removed, want kept")
	}

	var registeredOrphanCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM orphan_files WHERE rel_path IN ('legacy/original.m2ts', 'sites/tokyo/new/original.m2ts')",
	).Scan(&registeredOrphanCount); err != nil {
		t.Fatalf("querying orphan_files (registered assets): %v", err)
	}
	if registeredOrphanCount != 0 {
		t.Errorf("orphan_files count for registered assets = %d, want 0 (both legacy and site-prefixed active assets must not be flagged as orphans)", registeredOrphanCount)
	}

	var trueOrphanCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", trueOrphanRel,
	).Scan(&trueOrphanCount); err != nil {
		t.Fatalf("querying orphan_files (true orphan): %v", err)
	}
	if trueOrphanCount != 1 {
		t.Errorf("orphan_files count for %q = %d, want 1 (an unregistered file under sites/ must still be detected as an orphan candidate --- otherwise walkMediaFiles is silently excluding sites/, which is exactly the M4-11 failure mode the reserved directories exist to prevent)", trueOrphanRel, trueOrphanCount)
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

	// delete_reconcile は site を持たないブレーカーなので site 列は空文字列
	// （breaker.IsSiteless のコメント参照）。
	q := sqlcgen.New(pool)
	cb, err := q.GetCircuitBreaker(context.Background(), sqlcgen.GetCircuitBreakerParams{
		Site: "", Name: breaker.DeleteReconcile,
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
	// delete_reconcile は site を持たないブレーカーなので site 列は空文字列
	// （breaker.IsSiteless のコメント参照）。
	q := sqlcgen.New(pool)
	if err := breaker.Trip(context.Background(), q, "", breaker.DeleteReconcile, 0, breaker.Sample{Total: 1}); err != nil {
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

// active な media_asset の実体が無いことを検出する経路が無かった問題（issue #343）。
//
// ファイルが存在する間は候補として記録しない。もう一つ別の active 資産に実
// ファイルを置いておくことで、下記の「ゼロ件観測で全損シグネチャ扱いになり
// スキップする」安全弁を踏まないようにする（そちらは別テストで確認する）。
func TestDeleteReconcileWorker_MissingAsset_FilePresent_NotRegistered(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	recordingID := insertTestRecording(t, pool)
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "present/original.m2ts", []byte("data"))

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active (file is present, must not be touched)", got)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 0 {
		t.Errorf("missing_media_assets count = %d, want 0 (file exists on disk)", count)
	}
}

// ファイルが無い active な資産は missing_media_assets に候補として記録
// されるが、エイジング窓を過ぎるまでは Warn ログ・メトリクスに出ない
// （単発の走査揺れを確認済みの異常と区別する。孤児回収の OrphanAge と同じ
// 非対称）。media_assets 自体には一切触れない（自動削除しない）。
func TestDeleteReconcileWorker_MissingAsset_WithinAge_RecordedButNotReported(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	// 全損シグネチャ（ゼロ件観測）を踏まないよう、実体のある資産を 1 つ
	// 別に置く。
	otherRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, otherRecordingID, "present/original.m2ts", []byte("data"))

	recordingID := insertTestRecordingWithEventID(t, pool, 3)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/original.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}

	// プロセス内のゲージに残るので、このテストの外へ漏らさない
	// （他のテストと順序非依存にする）。
	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active (missing-file detection never deletes)", got)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 1 {
		t.Errorf("missing_media_assets count = %d, want 1 (candidate must be recorded even before aging out)", count)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 0 {
		t.Errorf("MediaAssetsMissing{kind=original} = %v, want 0 (not aged out yet, must not be reported)", got)
	}
}

// エイジング窓を過ぎた実体無し候補は Warn ログ相当のメトリクスに反映される。
// first_seen をあらかじめ過去に投入し、14 日待たずに検証する
// （TestDeleteReconcileWorker_Orphan_AgedOut_Deletes と同じ手法）。
func TestDeleteReconcileWorker_MissingAsset_AgedOut_ReportedWithoutDeleting(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	otherRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, otherRecordingID, "present/original.m2ts", []byte("data"))

	recordingID := insertTestRecordingWithEventID(t, pool, 4)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/aged.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	// エイジング窓を過ぎた記録として直接投入する。
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("seeding aged missing-asset record: %v", err)
	}

	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	before := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active (missing-file detection never deletes)", got)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 1 {
		t.Errorf("MediaAssetsMissing{kind=original} = %v, want 1 (aged out, must be reported)", got)
	}
	if got := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure); got != before {
		t.Errorf("MissingAssetScanSuspectedStorageFailure changed from %v to %v, want unchanged (a real file was observed this pass)", before, got)
	}
}

// cleanup.missing_asset_age の設定値が実際に反映されていることを検証する。
// 既存のエイジング系テストはいずれも MissingAssetAge に既定値
// (defaultMissingAssetAge = 24h) と同じ値を渡しているため、設定を無視して
// 既定値にフォールバックする実装でも見分けがつかない（CLAUDE.md「実装の
// 定数と比較するテストは何も主張していない」）。ここでは既定値と区別できる
// 短い値 (1h) を渡し、first_seen を 2h 前にすることで「既定 24h では未エイジ
// ングだが設定した 1h ではエイジング済み」という向きで検証する。
//
// このテストが検出すべき変異: Work が w.MissingAssetAge を無視して
// defaultMissingAssetAge を使う。その場合 2h 前の候補は 24h 未満なので
// 報告されず、最後のアサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_ConfiguredAge_ShorterThanDefault_Reported(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	otherRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, otherRecordingID, "present/original.m2ts", []byte("data"))

	recordingID := insertTestRecordingWithEventID(t, pool, 6)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/configured-age.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	// 既定 24h では未エイジングだが、設定する 1h ではエイジング済みになる
	// 経過時間。
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seeding aged missing-asset record: %v", err)
	}

	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 1 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active (missing-file detection never deletes)", got)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 1 {
		t.Errorf("MediaAssetsMissing{kind=original} = %v, want 1 (configured 1h age must be honored, not the 24h default)", got)
	}
}

// missing_media_assets の候補は、対象ファイルが後から見つかると掃除される
// （孤児回収の「登録された行を掃除する」と対になる逆方向）。この解消は
// メトリクスにも反映される必要がある --- reportAgedMissingAssets の
// metrics.MediaAssetsMissing.Reset() を削っても、1 パスだけの検証では
// 「一度も Set されていないので偶然 0」を見分けられない。1 パス目でエイジ
// ング済み候補を報告させてゲージを 1 に上げ、2 パス目でファイルを復元して
// ゲージが 0 に戻ることまで確認する（CLAUDE.md「壊す場所を、実際に壊れる
// 経路の上に置く」）。
//
// このテストが検出すべき変異: reportAgedMissingAssets から Reset() を削る。
// その場合 2 パス目でも 1 パス目の値 (1) が残り、最後のアサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_FileReappears_ClearsCandidate(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	otherRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, otherRecordingID, "present/original.m2ts", []byte("data"))

	recordingID := insertTestRecordingWithEventID(t, pool, 5)
	relPath := "reappears/original.m2ts"
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     relPath,
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	// エイジング窓を過ぎた記録として投入し、1 パス目で報告対象にする。
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("seeding missing-asset candidate: %v", err)
	}

	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() (pass 1) error: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 1 {
		t.Fatalf("MediaAssetsMissing{kind=original} after pass 1 = %v, want 1 (aged candidate must be reported)", got)
	}

	// ファイルが復元された(バックアップからの手動復元等)。
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() (pass 2) error: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 0 {
		t.Errorf("missing_media_assets count = %d, want 0 (file reappeared, candidate must be cleared)", count)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 0 {
		t.Errorf("MediaAssetsMissing{kind=original} after pass 2 = %v, want 0 (resolved candidate must clear the gauge, not just leave it stale)", got)
	}
}

// マウントが落ちている・空マウントの疑い（この走査で 1 件もファイルを
// 観測できなかった）のときは、全 active 行を「実体無し」と報告して騒がない
// --- 形で検知して丸ごとスキップし、既存の確認済み状態も上書きしない
// （issue #343 の受け入れ基準）。
func TestDeleteReconcileWorker_MissingAsset_EmptyMediaDir_SkipsSuspectedMountFailure(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir() // 空。MediaDir 配下に一切ファイルが無い状態を模す。

	recordingID := insertTestRecording(t, pool)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "would-be/original.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}

	before := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("asset state = %q, want active", got)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 0 {
		t.Errorf("missing_media_assets count = %d, want 0 (zero-file scan must not record any candidate)", count)
	}
	if got := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure); got != before+1 {
		t.Errorf("MissingAssetScanSuspectedStorageFailure = %v, want %v (must increment when the scan sees zero files while active assets exist)", got, before+1)
	}
}

// 上と対になる負例: MediaDir が空でも active な media_assets 行そのものが
// 無ければ、何もすることが無いので全損シグネチャ扱いにする必要はない
// （疑わしいパスとして数えない）。
func TestDeleteReconcileWorker_MissingAsset_EmptyMediaDirNoActiveAssets_DoesNotTripSuspicion(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	before := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure); got != before {
		t.Errorf("MissingAssetScanSuspectedStorageFailure changed from %v to %v, want unchanged (no active media_assets rows means nothing to be suspicious about)", before, got)
	}
}

// 既に確認済み(エイジング済み)だった候補は、その後のパスがゼロ件観測
// (マウント失敗の疑い)になっても消されない --- 疑わしいパスの結果で
// 前回までの確認済み状態を上書きしない。
func TestDeleteReconcileWorker_MissingAsset_SuspectedMountFailure_DoesNotClearExistingCandidates(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	recordingID := insertTestRecording(t, pool)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/original.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, now())",
		assetID); err != nil {
		t.Fatalf("seeding missing-asset candidate: %v", err)
	}

	// mediaDir は空のまま(この資産の実ファイルも他の資産も一切無い) ---
	// このパスはゼロ件観測になり全損シグネチャでスキップされるはず。
	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 1 {
		t.Errorf("missing_media_assets count = %d, want 1 (a suspected mount failure must not clear existing confirmed-missing candidates)", count)
	}
}

// 疑わしいパス（全損シグネチャ）では reportAgedMissingAssets の呼び出し自体を
// 止める（missing_media_assets 側の記録を見送るのと同じ理由で、報告も
// 見送る --- 記録だけ止めて報告を続けると「疑わしい間はゲージが凍結する」が
// metrics.go / docs/operations/monitoring.md の記述どおりにならない）。
//
// 「凍結」を検出可能にするため、疑わしいパスの直前にゲージへ本来の再計算
// 結果とは異なる番兵値を直接 Set しておく。reportAgedMissingAssets が
// 呼ばれなければ番兵値がそのまま残るが、無条件に呼ばれると Reset() で
// クリアされ実際の集計値（1）に置き換わる --- 対象の active 資産は
// このパスでも変わらず存在するので、「呼ばれても偶然同じ値になる」を
// 番兵値によって排除している。
//
// このテストが検出すべき変異: Work が reconcileMissingAssets の戻り値
// （疑わしいパスだったか）を無視して reportAgedMissingAssets を無条件に呼ぶ
// （PR #390 の元の実装）。その場合、疑わしいパスでも Reset() が呼ばれて
// 番兵値が実際の集計値に置き換わり、このテストの最終アサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_SuspectedMountFailure_FreezesGauge(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir() // 空。active な資産が存在する疑わしいパスを作る。

	recordingID := insertTestRecording(t, pool)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/frozen.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("seeding aged missing-asset record: %v", err)
	}

	const sentinel = 42
	// 番兵はプロセス内のゲージに残るので、このテストの外へ漏らさない
	// （今は後続テストが必ず Reset を通るが、-run で部分実行すると
	// 順序依存になる）。
	t.Cleanup(metrics.MediaAssetsMissing.Reset)
	metrics.MediaAssetsMissing.WithLabelValues("original").Set(sentinel)
	suspectedBefore := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.MissingAssetScanSuspectedStorageFailure); got != suspectedBefore+1 {
		t.Fatalf("MissingAssetScanSuspectedStorageFailure = %v, want %v (this pass must be suspected — empty mediaDir with an active asset)", got, suspectedBefore+1)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != sentinel {
		t.Errorf("MediaAssetsMissing{kind=original} after suspected pass = %v, want %v (reportAgedMissingAssets must not be called at all — the sentinel must survive untouched)", got, float64(sentinel))
	}
}

// reportAgedMissingAssets の個別 Warn ログは missingAssetLogBudget 件で
// 打ち切り、超過分は 1 行の要約にまとめる --- 対象が解消するまで
// defaultDeleteReconcileInterval ごとに全件再送され続けるログが無制限に
// 膨らむのを防ぐため。
//
// このテストが検出すべき変異: ログ出力を打ち切らずに全件出す（元の実装）。
// その場合、個別ログの件数が missingAssetLogBudget を超え、要約行も
// 出ないため、このテストのアサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_ReportLogBudget_CapsWarnLines(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	presentRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, presentRecordingID, "present/original.m2ts", []byte("data"))

	q := sqlcgen.New(pool)
	const overBudget = missingAssetLogBudget + 5
	for i := 0; i < overBudget; i++ {
		recordingID := insertTestRecordingWithEventID(t, pool, int32(1000+i))
		assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
			RecordingID: recordingID,
			Kind:        db.AssetKindOriginal,
			RelPath:     fmt.Sprintf("gone/budget-%d.m2ts", i),
			SizeBytes:   4,
		})
		if err != nil {
			t.Fatalf("seeding asset without a file (%d): %v", i, err)
		}
		if _, err := pool.Exec(context.Background(),
			"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
			assetID, time.Now().Add(-48*time.Hour)); err != nil {
			t.Fatalf("seeding aged missing-asset record (%d): %v", i, err)
		}
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	logged := logBuf.String()
	if got := strings.Count(logged, "active media asset has no file on disk"); got != missingAssetLogBudget {
		t.Errorf("individual missing-asset log lines = %d, want %d (missingAssetLogBudget)", got, missingAssetLogBudget)
	}
	wantSummary := fmt.Sprintf("and_more=%d", overBudget-missingAssetLogBudget)
	if !strings.Contains(logged, "suppressing further missing-asset log lines") || !strings.Contains(logged, wantSummary) {
		t.Errorf("log output does not contain the expected summary line (%s); log:\n%s", wantSummary, logged)
	}
	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != float64(overBudget) {
		t.Errorf("MediaAssetsMissing{kind=original} = %v, want %v (the metric, not the log, is the source of the count)", got, float64(overBudget))
	}
}

// ListAgedMissingMediaAssets の `AND a.state = 'active'` は不変条件 9
// 「適用の瞬間の再評価」そのもの --- missing_media_assets の行は一度
// 記録されると、対象の media_asset が後から active でなくなっても
// reconcileMissingAssets の掃除ループが必ず先に走るとは限らない
// （active な行が 1 件も無いパスは掃除自体を早期 return でスキップする。
// len(active) == 0 の分岐）。この預言を JOIN 側で再確認しないと、既に
// 削除済みの資産が「実体無し」として報告され続ける。
//
// このテストが検出すべき変異: ListAgedMissingMediaAssets の
// `AND a.state = 'active'` を外す。その場合、対象が deleted に遷移した後も
// 報告され続け、最後のアサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_AssetNoLongerActive_NotReported(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir() // 空。対象の active 行が 0 件になるのでこれでよい
	// （全損シグネチャの early return は len(active) == 0 の方が先に効く）。

	recordingID := insertTestRecording(t, pool)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/stale.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("seeding aged missing-asset record: %v", err)
	}
	// 対象が独立に(このワーカーの外で)active でなくなった状態を模す ---
	// reconcileMissingAssets の掃除ループが必ずこれより先に走るとは限らない
	// ことの検証なので、掃除を経ずに直接 state を進める。
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", assetID); err != nil {
		t.Fatalf("marking asset deleted out of band: %v", err)
	}

	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := promtestutil.ToFloat64(metrics.MediaAssetsMissing.WithLabelValues("original")); got != 0 {
		t.Errorf("MediaAssetsMissing{kind=original} = %v, want 0 (the asset is no longer active, must not be reported as a missing-file candidate)", got)
	}
}

// 上のテストの裏側: active でなくなった資産の候補行は「active が 1 件でもある
// パス」で実際に回収されること。reconcileMissingAssets の掃除ループは
// len(active) == 0 の early return より後ろにあるので、active が 0 件のパスでは
// 古い候補行が残る（報告側が state='active' で弾くので無害）。その「後で
// 回収される」という約束が実在することを直接測る --- 掃除ループを止めると
// 候補行が永久に残り、missing_media_assets が単調増加する。
//
// このテストが検出すべき変異: reconcileMissingAssets の掃除ループ
// （ListAllMissingMediaAssetIDs 以降）を消す / DeleteMissingMediaAsset の
// 呼び出しを飛ばす。どちらも最後のアサーションが落ちる。
func TestDeleteReconcileWorker_MissingAsset_StaleCandidate_CollectedWhenActiveExists(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	// 掃除ループに入るための条件を作る: active な資産が 1 件以上あり、かつ
	// ディスク上でファイルが 1 件以上観測される（全損シグネチャを踏まない）。
	otherRecordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, otherRecordingID, "present/original.m2ts", []byte("data"))

	recordingID := insertTestRecordingWithEventID(t, pool, 7)
	q := sqlcgen.New(pool)
	assetID, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "gone/stale.m2ts",
		SizeBytes:   4,
	})
	if err != nil {
		t.Fatalf("seeding asset without a file: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO missing_media_assets (media_asset_id, first_seen) VALUES ($1, $2)",
		assetID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("seeding aged missing-asset record: %v", err)
	}
	// このワーカーの外で active でなくなった（＝ListActiveMediaAssets に
	// 現れないので今パスの candidates に入らない）。行が消えていないので
	// FK の ON DELETE CASCADE では掃除されず、掃除ループだけが回収できる。
	if _, err := pool.Exec(context.Background(),
		"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1", assetID); err != nil {
		t.Fatalf("marking asset deleted out of band: %v", err)
	}

	t.Cleanup(metrics.MediaAssetsMissing.Reset)

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir, MissingAssetAge: 24 * time.Hour}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM missing_media_assets WHERE media_asset_id = $1", assetID).Scan(&count); err != nil {
		t.Fatalf("querying missing_media_assets: %v", err)
	}
	if count != 0 {
		t.Errorf("missing_media_assets count = %d, want 0 (the asset is no longer active, so the candidate row must be collected on a pass that has at least one active asset)", count)
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
//
// Queue の期待値はリテラル "cleanup" で書く（cleanupQueue 定数と比較すると、
// 実装も同じ定数を参照しているだけなので、定数の値が何であっても常に一致して
// しまい何も主張しない。issue #185 のレビュー指摘）。
func TestDeleteReconcileArgs_KindAndQueue(t *testing.T) {
	args := DeleteReconcileArgs{}
	if args.Kind() != "delete_reconcile" {
		t.Errorf("Kind = %q", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "cleanup" {
		t.Errorf("Queue = %q, want %q", opts.Queue, "cleanup")
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("ByArgs should be true")
	}
	if len(opts.UniqueOpts.ByState) == 0 {
		t.Error("ByState should be set to pendingJobStates (completed を含めると定期ジョブがワンショット化する)")
	}
	// ByQueue が立っていること（issue #185 レビュー: キュー名の変更が一意キーに
	// 影響しないと、旧キュー（river.QueueDefault）の残骸が新キュー（cleanup）への
	// insert を UniqueSkippedAsDuplicate として黙って塞ぐ。pendingJobStates
	// 直後の doc コメント参照）。
	if !opts.UniqueOpts.ByQueue {
		t.Error("ByQueue should be true (キュー名変更が一意キーに影響しないと旧キューの残骸が新キューへの insert を塞ぐ)")
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
