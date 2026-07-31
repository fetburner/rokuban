package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

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

// 削除の進行中（deleting）にごみ箱から復元された録画では recording.deleted を
// 発火しない。pending 経路は「既に決めた削除」を続行するので最後のアセットが
// 消えるが、録画は生きているので「録画が消えた」ではない
// （TrashPurgeWithoutOriginal_Fires... の逆方向。復元は DB だけを触る
// = internal/api RestoreRecording なので、この競合は実際に起こりうる）。
func TestDeleteReconcileWorker_RestoredWhileDeleting_DoesNotFireRecordingDeletedWebhook(t *testing.T) {
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
	if got := assetState(t, pool, assetID); got != "deleted" {
		t.Fatalf("asset state after second pass = %q, want deleted (pending は決定済みの削除を続行する)", got)
	}
	if events := rec.received(); len(events) != 0 {
		t.Errorf("webhook fired for a restored recording: %+v", events)
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
	// 予算 0 = 1 件目から捨てる。予算があれば発火する条件（trashed + アセット無し）は
	// 揃えてあるので、ここで 0 件なら予算が効いている。
	w.notifyPurgedRecordings(context.Background(), sqlcgen.New(pool), []int64{recordingID}, 0)
	if events := rec.received(); len(events) != 0 {
		t.Errorf("notified despite an exhausted budget: %+v", events)
	}

	w.notifyPurgedRecordings(context.Background(), sqlcgen.New(pool), []int64{recordingID}, time.Minute)
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

// encode_profiles が空（本来 until_encoded では起こらないはずの組だが、issue #103
// の凍結処理のバグ等で起こりうる）のとき、原本を消してはならない（issue #103 の
// 「罠」の安全弁。internal/db/queries/delete_reconcile.sql の
// ListUntilEncodedOriginalsToDelete が cardinality(r.encode_profiles) > 0 を
// 要求するようになった前は、NOT EXISTS(unnest(...)) が空集合に対して自明に真になり
// 「全プロファイル完備」を誤って認めていた）。
func TestDeleteReconcileWorker_UntilEncoded_EmptyProfiles_NotDeleted(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)

	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "orig/empty-profiles.m2ts", []byte("data"))
	// desired な派生物はサムネイルだけ。encode_profiles は意図的に空にする。
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, db.AssetKindThumbnail, nil, "thumb/empty-profiles.jpg", []byte("jpg"))

	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{}, recordingID); err != nil {
		t.Fatalf("setting keep_original: %v", err)
	}

	w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	if got := assetState(t, pool, assetID); got != "active" {
		t.Errorf("original state = %q, want active (encode_profiles is empty; one-copy invariant would be violated)", got)
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
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingA); err != nil {
		t.Fatalf("setting keep_original for recording A: %v", err)
	}

	// recordingB: 派生物は揃っており、pending なジョブは無い。event_id を
	// insertTestRecording の既定（1）とずらす —— recordings_unique_active_event は
	// (site, network_id, service_id, event_id) のアクティブ行に対する一意制約なので、
	// 同じ event_id で 2 つ目のアクティブな録画を insertTestRecording で作ろうとすると
	// そのまま衝突する。
	recordingB := insertTestRecordingWithEventID(t, pool, 2)
	assetB := seedOriginalAsset(t, pool, mediaDir, recordingB, "orig/multi-b.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindEncoded, &profile, "enc/multi-b.mp4", []byte("mp4"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingB, db.AssetKindThumbnail, nil, "thumb/multi-b.jpg", []byte("jpg"))
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET keep_original = 'until_encoded', encode_profiles = $1 WHERE id = $2",
		[]string{"h264"}, recordingB); err != nil {
		t.Fatalf("setting keep_original for recording B: %v", err)
	}

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
