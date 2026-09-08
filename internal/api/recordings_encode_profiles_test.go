package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// postEncodeProfiles は POST /api/recordings/{id}/encode-profiles を叩く。
func postEncodeProfiles(t *testing.T, url string, profiles []string) *http.Response {
	t.Helper()
	body := map[string]any{"profiles": profiles}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func encodeProfilesURL(base string, id int64) string {
	return fmt.Sprintf("%s/api/recordings/%d/encode-profiles", base, id)
}

// patchRecordingEncodePolicy は PATCH /api/recordings/{id}/encode-policy を叩く。
func patchRecordingEncodePolicy(t *testing.T, url string, keepOriginal string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"keepOriginal": keepOriginal})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func encodePolicyURL(base string, id int64) string {
	return fmt.Sprintf("%s/api/recordings/%d/encode-policy", base, id)
}

func getRecordingKeepOriginal(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var keepOriginal string
	if err := pool.QueryRow(context.Background(),
		`SELECT keep_original FROM recording_encode_policy WHERE recording_id = $1`, id,
	).Scan(&keepOriginal); err != nil {
		t.Fatalf("loading keep_original for recording %d: %v", id, err)
	}
	return keepOriginal
}

func setRecordingEncodeProfiles(t *testing.T, pool *pgxpool.Pool, id int64, profiles []string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_policy SET encode_profiles = $2 WHERE recording_id = $1`, id, profiles,
	); err != nil {
		t.Fatalf("setting encode_profiles for recording %d: %v", id, err)
	}
}

// seedReservationForTest は reservations に最小限の行を直接 INSERT する。
// 「予約がある録画」のシナリオ（TestAddRecordingEncodeProfiles_WithReservation_Success）
// を作るためだけに使う --- recordings.reservation_id は issue #158 で列自体を
// 落としたので、この呼び出しはもう FK を満たすためではない（recordings と
// reservations の間に直接の結合キーは無い）。Phase 1（#27/#28/#30）以降
// reservations は ruler の 1 パスの出力（site, program_id, rule_id, base,
// dedup 根拠 2 列, timestamps）だけを持つ導出テーブルで、番組の事実は
// program_snapshots 側の責務。reservations.program_id は program_snapshots
// (site, program_id) への FK（reservations_program_fkey）を持つため、先に
// program_snapshots 行を用意する。
func seedReservationForTest(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (
			site, program_id, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ($1, $2, now(), 1800000, 32736, 1024, 'GR', '27', $3, 'テスト局')
		ON CONFLICT (site, program_id) DO NOTHING`,
		db.DefaultSite, programID, int32(programID%100000),
	); err != nil {
		t.Fatalf("seeding program_snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reservations (site, program_id)
		VALUES ($1, $2)`,
		db.DefaultSite, programID,
	); err != nil {
		t.Fatalf("seeding reservation: %v", err)
	}
}

// getRecordingEncodeProfiles は recording_encode_policy 衛星表（issue #159）を
// 読む。行が無い（未凍結。原本 media_asset がまだコミットされていない）場合は
// 空のプロファイル一覧を返す --- このテストファイルの呼び出し元はいずれも
// seedIngested 後（原本コミット後。resolveAndSnapshotEncodePolicy が既定値でも
// 必ず凍結する。internal/worker/ingest.go 参照）に呼ぶので、実運用では行が
// 見つかるはずだが、防御的に nil スキャンにはしない。
func getRecordingEncodeProfiles(t *testing.T, pool *pgxpool.Pool, id int64) []string {
	t.Helper()
	var profiles []string
	err := pool.QueryRow(context.Background(),
		`SELECT encode_profiles FROM recording_encode_policy WHERE recording_id = $1`, id,
	).Scan(&profiles)
	if err == nil {
		return profiles
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return []string{}
	}
	t.Fatalf("loading encode_profiles for recording %d: %v", id, err)
	return nil
}

func countEncodeEnqueueHintJobs(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM river_job WHERE kind = 'encode_enqueue_hint'`,
	).Scan(&n); err != nil {
		t.Fatalf("counting encode_enqueue_hint jobs: %v", err)
	}
	return n
}

func clearEncodeEnqueueHintJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM river_job WHERE kind = 'encode_enqueue_hint'`); err != nil {
		t.Fatalf("clearing encode_enqueue_hint jobs: %v", err)
	}
}

func writePolicyTestFile(t *testing.T, mediaDir, relPath string) string {
	t.Helper()
	path := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating media directory for %s: %v", relPath, err)
	}
	if err := os.WriteFile(path, []byte("test media"), 0o644); err != nil {
		t.Fatalf("writing media file %s: %v", relPath, err)
	}
	return path
}

const wantKeepOriginal409Message = "cannot set keepOriginal=until_encoded without desired encode profiles; add encode profiles first"

// always と until_encoded の両方向で keep_original だけが変わり、desired の
// encode_profiles は変わらないことを確認する。同じ値への PATCH は 204 で、
// 保持ポリシー変更が encode_enqueue_hint を投入しないことも確認する（issue #697）。
func TestSetRecordingEncodePolicy_SuccessPreservesProfilesAndIsIdempotent(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264", "h265"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "保持ポリシー", time.Now().Truncate(time.Second), "finished", 601)
	seedIngested(t, pool, id, 1000, nil)
	setRecordingEncodeProfiles(t, pool, id, []string{"h264", "h265"})

	if n := countEncodeEnqueueHintJobs(t, pool); n != 0 {
		t.Fatalf("initial encode_enqueue_hint job count = %d, want 0", n)
	}

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "until_encoded")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("until_encoded status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "until_encoded" {
		t.Errorf("keep_original = %q, want until_encoded", got)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("encode_profiles after until_encoded = %v, want [h264 h265]", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 0 {
		t.Errorf("encode_enqueue_hint job count after until_encoded = %d, want 0", n)
	}

	// 同じ値への変更は冪等に成功する。
	resp = patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "until_encoded")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent until_encoded status = %d, want 204", resp.StatusCode)
	}

	// 逆方向でも encode_profiles は縮まらない。
	resp = patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "always")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("always status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original = %q, want always", got)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("encode_profiles after always = %v, want [h264 h265]", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 0 {
		t.Errorf("encode_enqueue_hint job count after policy changes = %d, want 0", n)
	}

	resp = patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "always")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("idempotent always status = %d, want 204", resp.StatusCode)
	}
}

// until_encoded は desired なプロファイルが空なら 409 を返し、ポリシーを
// 変更しない。recording_encode_policy 行が無い場合も同じ扱いにする（issue #697）。
func TestSetRecordingEncodePolicy_UntilEncodedWithoutProfiles_Returns409(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	emptyID := seedRecording(t, pool, "プロファイルなし", time.Now().Truncate(time.Second), "finished", 602)
	seedIngested(t, pool, emptyID, 1000, nil)
	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, emptyID), "until_encoded")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("empty profiles status = %d, want 409", resp.StatusCode)
	}
	if body := decodeErrorResponse(t, resp); body.Error != wantKeepOriginal409Message {
		t.Errorf("error = %q, want %q", body.Error, wantKeepOriginal409Message)
	}
	if got := getRecordingKeepOriginal(t, pool, emptyID); got != "always" {
		t.Errorf("keep_original after 409 = %q, want unchanged always", got)
	}

	noPolicyID := seedRecording(t, pool, "未凍結", time.Now().Add(time.Second).Truncate(time.Second), "finished", 603)
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: noPolicyID,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.m2ts", noPolicyID),
		SizeBytes:   1000,
	}); err != nil {
		t.Fatalf("seeding active original without policy: %v", err)
	}
	resp = patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, noPolicyID), "until_encoded")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("missing policy status = %d, want 409", resp.StatusCode)
	}
	if body := decodeErrorResponse(t, resp); body.Error != wantKeepOriginal409Message {
		t.Errorf("missing policy error = %q, want %q", body.Error, wantKeepOriginal409Message)
	}
	var policyRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, noPolicyID,
	).Scan(&policyRows); err != nil {
		t.Fatalf("counting policy rows after 409: %v", err)
	}
	if policyRows != 0 {
		t.Errorf("policy rows after 409 = %d, want 0", policyRows)
	}
}

// 未 ingest（原本 media_asset も recording_encode_policy 行も無い）録画への
// PATCH は 204（no-op）で、recording_encode_policy 行を作らないこと（issue #697
// レビューのブロッカー: SetRecordingKeepOriginal が旧 ON CONFLICT の INSERT
// だった頃はここで行を作ってしまい、後続の ingest が呼ぶ
// FreezeRecordingEncodePolicy（ON CONFLICT 無しの素の INSERT）が PK 衝突して
// 原本 media_asset の INSERT と同一 tx ごとロールバックし、原本が永久に
// コミットされなかった）。行を作らないことに加え、ingest 相当の
// FreezeRecordingEncodePolicy が実際に成功することまで確認する。
func TestSetRecordingEncodePolicy_BeforeIngest_NoOpAndDoesNotBlockFreeze(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "未 ingest への保持ポリシー変更", time.Now().Truncate(time.Second), "recording", 610)

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "always")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (no-op for un-ingested recording)", resp.StatusCode)
	}

	var policyRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM recording_encode_policy WHERE recording_id = $1`, id,
	).Scan(&policyRows); err != nil {
		t.Fatalf("counting policy rows after 204: %v", err)
	}
	if policyRows != 0 {
		t.Fatalf("policy rows after PATCH before ingest = %d, want 0 (endpoint must never freeze a new row)", policyRows)
	}

	// ingest の resolveAndSnapshotEncodePolicy が原本 media_asset の INSERT と
	// 同一トランザクションで呼ぶ操作を模す。行が残っていれば PK 衝突する。
	if err := sqlcgen.New(pool).FreezeRecordingEncodePolicy(context.Background(), sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    id,
		KeepOriginal:   "always",
		EncodeProfiles: []string{},
	}); err != nil {
		t.Fatalf("FreezeRecordingEncodePolicy after PATCH before ingest: %v (must succeed; the endpoint must not have pre-created a policy row)", err)
	}
}

// purge 済み（purged_at が立った tombstone）の録画への PATCH は 404 で、
// recording_encode_policy を書かないこと（issue #697 レビュー: GetRecordingByID
// は述語なしで ingest worker と共有するため緩められないが、GET
// /api/recordings/{id}（queryRecordingByID、purged_at IS NULL）との非対称を
// このハンドラで埋める）。
func TestSetRecordingEncodePolicy_Purged_ReturnsNotFoundAndDoesNotWrite(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "purge 済み", time.Now().Truncate(time.Second), "finished", 611)
	seedIngested(t, pool, id, 1000, nil)
	if _, err := pool.Exec(context.Background(),
		`UPDATE recordings SET deleted_at = now(), purged_at = now() WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("marking recording purged: %v", err)
	}

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "until_encoded")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original after 404 = %q, want unchanged always", got)
	}
}

func TestSetRecordingEncodePolicy_NotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, 999999), "always")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSetRecordingEncodePolicy_InvalidValueReturns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	id := seedRecording(t, pool, "不正なポリシー", time.Now().Truncate(time.Second), "finished", 604)
	seedIngested(t, pool, id, 1000, nil)
	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "sometimes")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original after 400 = %q, want always", got)
	}
}

// keepOriginal=until_encoded へ切り替えた後は、派生物とサムネイルが揃っていれば
// 次の削除 reconcile パスが原本を削除する（issue #697）。API 自身はファイルにも
// River にも触れず、既存のレベルトリガーだけが削除を行う。
func TestSetRecordingEncodePolicy_UntilEncoded_DeletesOnReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()
	mediaDir := t.TempDir()

	id := seedRecording(t, pool, "reconcile で原本削除", time.Now().Truncate(time.Second), "finished", 605)
	originalID := seedIngested(t, pool, id, 1000, nil)
	setRecordingEncodeProfiles(t, pool, id, []string{"h264"})
	originalPath := writePolicyTestFile(t, mediaDir, fmt.Sprintf("test/%d.m2ts", id))

	profile := "h264"
	q := sqlcgen.New(pool)
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindEncoded,
		Profile:     &profile,
		RelPath:     fmt.Sprintf("encoded/%d.mp4", id),
		SizeBytes:   200,
	}); err != nil {
		t.Fatalf("seeding encoded asset: %v", err)
	}
	if _, err := q.CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindThumbnail,
		RelPath:     fmt.Sprintf("thumbnail/%d.jpg", id),
		SizeBytes:   50,
	}); err != nil {
		t.Fatalf("seeding thumbnail asset: %v", err)
	}
	writePolicyTestFile(t, mediaDir, fmt.Sprintf("encoded/%d.mp4", id))
	writePolicyTestFile(t, mediaDir, fmt.Sprintf("thumbnail/%d.jpg", id))

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "until_encoded")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	w := &worker.DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE id = $1`, originalID).Scan(&state); err != nil {
		t.Fatalf("loading original state: %v", err)
	}
	if state != "deleted" {
		t.Errorf("original state = %q, want deleted", state)
	}
	if _, err := os.Stat(originalPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("original file stat error = %v, want not exist", err)
	}
}

// 原本が削除処理中でも always への変更を拒まない。次の reconcile パスが
// until_encoded の判定から外れた deleting 行を、ファイルが残っている限り active に
// 戻す（issue #105 / #697）。
func TestSetRecordingEncodePolicy_AlwaysWhileDeleting_RevertsOnReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()
	mediaDir := t.TempDir()

	id := seedRecording(t, pool, "削除中に保持", time.Now().Truncate(time.Second), "finished", 606)
	originalID := seedIngested(t, pool, id, 1000, nil)
	setRecordingEncodeProfiles(t, pool, id, []string{"h264"})
	if _, err := pool.Exec(context.Background(),
		`UPDATE recording_encode_policy SET keep_original = 'until_encoded' WHERE recording_id = $1`, id,
	); err != nil {
		t.Fatalf("setting initial until_encoded policy: %v", err)
	}
	originalPath := writePolicyTestFile(t, mediaDir, fmt.Sprintf("test/%d.m2ts", id))
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleting' WHERE id = $1`, originalID,
	); err != nil {
		t.Fatalf("marking original deleting: %v", err)
	}

	resp := patchRecordingEncodePolicy(t, encodePolicyURL(srv.URL, id), "always")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingKeepOriginal(t, pool, id); got != "always" {
		t.Errorf("keep_original = %q, want always", got)
	}

	w := &worker.DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	if err := w.Work(context.Background(), nil); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_assets WHERE id = $1`, originalID).Scan(&state); err != nil {
		t.Fatalf("loading original state: %v", err)
	}
	if state != "active" {
		t.Errorf("original state = %q, want active", state)
	}
	if _, err := os.Stat(originalPath); err != nil {
		t.Errorf("original file stat error = %v, want file to remain", err)
	}
}

// 予約が無い録画（mirakc に直接起こされた手動録画などを模す）でも事後追加が
// 成功し、recording_encode_policy.encode_profiles に追加専用（union + dedup）で反映され、
// encode_enqueue_hint ヒントジョブが同一トランザクションで投入されること
// （issue #133 の受け入れ 1 個目）。
func TestAddRecordingEncodeProfiles_NoReservation_Success(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264", "h265"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "予約なし", time.Now().Truncate(time.Second), "finished", 201)
	seedIngested(t, pool, id, 1000, nil)

	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Fatalf("initial encode_profiles = %v, want empty", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 0 {
		t.Fatalf("initial encode_enqueue_hint job count = %d, want 0", n)
	}

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264]", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 1 {
		t.Fatalf("encode_enqueue_hint job count = %d, want 1", n)
	}
	clearEncodeEnqueueHintJobs(t, pool)

	// 追加専用であること: 2 回目は h265 だけを指定する（h264 は含めない）。
	// 全置換だったら結果が [h265] になってしまうところを、union なら
	// [h264 h265] のまま h264 が残ることで区別できる。
	resp = postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h265"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("encode_profiles after second add = %v, want [h264 h265]", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 1 {
		t.Fatalf("encode_enqueue_hint job count after second add = %d, want 1", n)
	}
}

// 予約がある録画でも、ingest 後に追加したプロファイルが反映されること
// （issue #133 の受け入れ 2 個目）。事後追加の実装は reservations の有無を
// 一切見ないが、受け入れ基準の文言どおり明示的に固定する。
func TestAddRecordingEncodeProfiles_WithReservation_Success(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	base := time.Now().Truncate(time.Second)
	seedReservationForTest(t, pool, 5001)

	// NetworkID/ServiceID/EventID は seedReservationForTest が用意した
	// program_snapshots 行（32736, 1024, event_id=5001）と一致させる。
	// recordings.reservation_id は issue #158 で列自体を落としたので、
	// 予約と録画を結ぶのは放送イベントキーだけである --- キーを一致させないと
	// 「予約がある録画」を再現できず、NoReservation 版と実質同一のケースになる。
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "rule",
		Site:              db.DefaultSite,
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           5001,
		ServiceName:       "テスト局",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "予約あり",
		ProgramStartAt:    base,
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("seeding recording with reservation: %v", err)
	}
	seedIngested(t, pool, id, 500, nil)

	// 「予約がある録画」を実際に再現できているかを、recordings と reservations を
	// つなぐ唯一の経路である放送イベントキー (site, network_id, service_id,
	// event_id) 経由で確認する。recordings.reservation_id は issue #158 で
	// 落ちたので、この到達性を確認しないと NoReservation 版と実質同一のケースを
	// 「予約あり」だと誤認したまま通ってしまう。作った recording 自身の列から
	// キーを読み直す（呼び出し側で決め打ちの値と比較すると、CreateRecording への
	// 引数を書き間違えても検出できない）。
	var site string
	var networkID, serviceID, eventID int32
	if err := pool.QueryRow(context.Background(),
		`SELECT site, network_id, service_id, event_id FROM recordings WHERE id = $1`, id,
	).Scan(&site, &networkID, &serviceID, &eventID); err != nil {
		t.Fatalf("loading broadcast event key for recording %d: %v", id, err)
	}
	if _, err := sqlcgen.New(pool).GetReservationEncodePolicyByEvent(context.Background(), sqlcgen.GetReservationEncodePolicyByEventParams{
		Site:      site,
		NetworkID: networkID,
		ServiceID: serviceID,
		EventID:   eventID,
	}); err != nil {
		t.Fatalf("recording is not reachable from the seeded reservation via broadcast event key: %v", err)
	}

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264]", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 1 {
		t.Fatalf("encode_enqueue_hint job count = %d, want 1", n)
	}
}

// recording_encode_policy に行が無い（未凍結）まま原本 media_asset だけが
// active な録画（internal/inplace.Register → internal/catalog/rescue_scan.go
// の rescueStorage、catalog を 1 世代も持たない状態からの災害復旧が作る形。
// resolveAndSnapshotEncodePolicy を経由しないので凍結されない）への事後追加が
// 204 で成功すること（issue #159 レビューで見つかった回帰）。
//
// 直す前の実装（AppendRecordingEncodeProfiles が :execrows の UPDATE で
// rows == 0 をエラーにする）はこの形で 500 を返していた --- 原本ありの録画に
// 事後追加できない、という issue #133 が解こうとした問題そのものが再発していた。
// AppendRecordingEncodeProfiles を ON CONFLICT の INSERT にしたことで、
// 「原本が active = 凍結済みとみなす」を適用し既定値 keep_original='always' で
// 行を新規に作りながら追記する。
func TestAddRecordingEncodeProfiles_NoPolicyRowButOriginalActive_Returns204(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "災害復旧で復元", time.Now().Truncate(time.Second), "finished", 301)
	// seedIngested は使わない --- あれは recording_encode_policy 行も一緒に
	// 作ってしまう（実運用の ingest を模したフィクスチャなので）。ここでは
	// inplace.Register が実際に作る形（原本 media_asset のみ、policy 行なし）を
	// 直接再現する。
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: id,
		Kind:        db.AssetKindOriginal,
		RelPath:     fmt.Sprintf("test/%d.m2ts", id),
		SizeBytes:   1000,
	}); err != nil {
		t.Fatalf("seeding media_asset without recording_encode_policy: %v", err)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Fatalf("initial encode_profiles = %v, want empty (no policy row yet)", got)
	}

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264]", got)
	}
	var keepOriginal string
	if err := pool.QueryRow(context.Background(),
		`SELECT keep_original FROM recording_encode_policy WHERE recording_id = $1`, id,
	).Scan(&keepOriginal); err != nil {
		t.Fatalf("loading keep_original for recording %d: %v", id, err)
	}
	if keepOriginal != "always" {
		t.Errorf("keep_original = %q, want always (safe default for freshly-created policy row)", keepOriginal)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 1 {
		t.Fatalf("encode_enqueue_hint job count = %d, want 1", n)
	}
}

// wantEncodeProfiles409Message は #271 で確定させた 409 メッセージのリテラル。
// 旧文言 "no encodable original media asset (deleted or being deleted); cannot
// add encode profiles" は「未 ingest」（original 行自体が無いケース）を
// 「削除済みか削除中」だと誤誘導していたため、削除・deleting・未 ingest の
// いずれもありうることが伝わる文言に直した（実装の定数と比較すると意味が無い
// ため、期待値はリテラルで書く）。
const wantEncodeProfiles409Message = "original media asset not active (deleted, deleting, or not yet ingested); cannot add encode profiles"

// decodeErrorResponse はレスポンスボディを ErrorResponse として読む。
func decodeErrorResponse(t *testing.T, resp *http.Response) ErrorResponse {
	t.Helper()
	var body ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	return body
}

// 原本が未 ingest（GetActiveOriginalMediaAsset が ErrNoRows）の録画への事後追加は
// 409 を返し、encode_profiles を変更せず、ジョブも投入しないこと（issue #133
// の受け入れ 3 個目 --- EnqueueMissingEncodes 単体はこのケースで黙って no-op に
// なるため、api 層で明示的に検査していることの固定）。
//
// メッセージ本文も固定する（issue #271）。「未 ingest」は削除済みでも
// deleting でもないので、旧文言 "deleted or being deleted" はこのケースに
// 対して不正確だった。
func TestAddRecordingEncodeProfiles_NoOriginal_Returns409(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "未 ingest", time.Now().Truncate(time.Second), "recording", 202)

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if body := decodeErrorResponse(t, resp); body.Error != wantEncodeProfiles409Message {
		t.Errorf("error message = %q, want %q", body.Error, wantEncodeProfiles409Message)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles after 409 = %v, want unchanged (empty)", got)
	}
	if n := countEncodeEnqueueHintJobs(t, pool); n != 0 {
		t.Fatalf("encode_enqueue_hint job count after 409 = %d, want 0", n)
	}
}

// 原本削除済み（media_assets の original が state='deleted'）の録画も同様に 409。
// until_encoded でエンコード完了後に retention reconcile が原本を消した状態を模す。
func TestAddRecordingEncodeProfiles_OriginalDeleted_Returns409(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "原本削除済み", time.Now().Truncate(time.Second), "finished", 203)
	seedIngested(t, pool, id, 500, nil)
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleted', deleted_at = now()
		 WHERE recording_id = $1 AND kind = 'original'`, id,
	); err != nil {
		t.Fatalf("marking original deleted: %v", err)
	}

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if body := decodeErrorResponse(t, resp); body.Error != wantEncodeProfiles409Message {
		t.Errorf("error message = %q, want %q", body.Error, wantEncodeProfiles409Message)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles after 409 = %v, want unchanged (empty)", got)
	}
}

// 原本が deleting（unlink 待ち）の録画も 409。
//
// 一覧の射影（ListRecordings の LEFT JOIN）は `a.state <> 'deleted'` なので
// deleting の原本でも sizeBytes が付き、UI は「原本あり」と見てボタンを出す。
// 一方サーバーの判定は GetActiveOriginalMediaAsset（`state = 'active'`）なので
// ここに落ちる。**この非対称を意図された振る舞いとして固定する** ---
// deleting の原本に対してエンコードを走らせてはいけない（unlink 中のファイルを
// 読む）ので、409 にして UI にエラーを見せるのが正しい。判定を
// `state <> 'deleted'` に緩めるとこのテストが落ちる。
func TestAddRecordingEncodeProfiles_OriginalDeleting_Returns409(t *testing.T) {
	pool := testutil.SetupDB(t)
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "原本 unlink 待ち", time.Now().Truncate(time.Second), "finished", 204)
	seedIngested(t, pool, id, 500, nil)
	if _, err := pool.Exec(context.Background(),
		`UPDATE media_assets SET state = 'deleting'
		 WHERE recording_id = $1 AND kind = 'original'`, id,
	); err != nil {
		t.Fatalf("marking original deleting: %v", err)
	}

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409（deleting の原本でエンコードを走らせてはいけない）", resp.StatusCode)
	}
	// メッセージ本文も固定する（issue #271）。deleting は「削除済み」ではないので
	// 旧文言 "deleted or being deleted" のうち "being deleted" 側で辛うじて
	// カバーされていたが、新文言でも deleting が含意されていることを確認する。
	if body := decodeErrorResponse(t, resp); body.Error != wantEncodeProfiles409Message {
		t.Errorf("error message = %q, want %q", body.Error, wantEncodeProfiles409Message)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles after 409 = %v, want unchanged (empty)", got)
	}
}

// 存在しない録画 ID には 404。
func TestAddRecordingEncodeProfiles_NotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool, EncodeProfileNames: []string{"h264"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, 999999), []string{"h264"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// 空配列は 400（追加する意味が無い依頼を弾く）。
func TestAddRecordingEncodeProfiles_Empty_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool, EncodeProfileNames: []string{"h264"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "空", time.Now().Truncate(time.Second), "finished", 204)
	seedIngested(t, pool, id, 500, nil)

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles after 400 = %v, want unchanged (empty)", got)
	}
}

// config.encode.profiles に無い名前は 400（既存の validateEncodeProfiles を
// ルール/overrides と共有していることの固定。issue #64 と同じ検査）。
func TestAddRecordingEncodeProfiles_UnknownProfile_Returns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool, EncodeProfileNames: []string{"h264"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	id := seedRecording(t, pool, "未知プロファイル", time.Now().Truncate(time.Second), "finished", 205)
	seedIngested(t, pool, id, 500, nil)

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"no-such-profile"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := getRecordingEncodeProfiles(t, pool, id); len(got) != 0 {
		t.Errorf("encode_profiles after 400 = %v, want unchanged (empty)", got)
	}
}

// api → worker のヒント経路を実際の River クライアントで最後まで流し、
// 不足分の encode ジョブが投入されるところまで見る（issue #133 の受け入れ
// 「encode_profiles に反映されて encode ジョブが投入されること」の
// エンドツーエンドの裏付け）。
func TestAddRecordingEncodeProfiles_EndToEnd_EnqueuesEncodeJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	workers := worker.NewWorkers(&worker.Deps{Pool: pool})
	riverClient, err := worker.NewClient(pool, workers, worker.ClientConfig{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	router := NewRouter(RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	subscribeCh, subscribeCancel := riverClient.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := riverClient.Start(startCtx); err != nil {
		t.Fatalf("starting river client: %v", err)
	}
	defer func() {
		cancel()
		<-riverClient.Stopped()
	}()

	id := seedRecording(t, pool, "予約なし・エンドツーエンド", time.Now().Truncate(time.Second), "finished", 206)
	seedIngested(t, pool, id, 1234, nil)

	resp := postEncodeProfiles(t, encodeProfilesURL(srv.URL, id), []string{"h264"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	deadline := time.After(20 * time.Second)
waitHint:
	for {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind == "encode_enqueue_hint" {
				break waitHint
			}
		case <-deadline:
			t.Fatal("timed out waiting for encode_enqueue_hint completion")
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'encode'
		 AND (args->>'recording_id')::bigint = $1 AND args->>'profile' = 'h264'`,
		id,
	).Scan(&count); err != nil {
		t.Fatalf("counting encode jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("encode job count = %d, want 1", count)
	}

	if got := getRecordingEncodeProfiles(t, pool, id); !slices.Equal(got, []string{"h264"}) {
		t.Errorf("encode_profiles = %v, want [h264]", got)
	}
}
