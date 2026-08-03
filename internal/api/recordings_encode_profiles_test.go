package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

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

// seedReservationForTest は reservations に最小限の行を直接 INSERT する。
// recordings.reservation_id が参照する FK を満たすためだけに使う --- Phase 1
// （#27/#28/#30）以降 reservations は ruler の 1 パスの出力（id, site, program_id,
// rule_id, base, dedup 根拠 2 列, timestamps）だけを持つ導出テーブルで、番組の
// 事実は program_snapshots 側の責務。reservations.program_id は
// program_snapshots (site, program_id) への FK（reservations_program_fkey）を
// 持つため、先に program_snapshots 行を用意する。
func seedReservationForTest(t *testing.T, pool *pgxpool.Pool, programID int64) int64 {
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
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO reservations (site, program_id)
		VALUES ($1, $2)
		RETURNING id`,
		db.DefaultSite, programID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seeding reservation: %v", err)
	}
	return id
}

func getRecordingEncodeProfiles(t *testing.T, pool *pgxpool.Pool, id int64) []string {
	t.Helper()
	var profiles []string
	if err := pool.QueryRow(context.Background(),
		`SELECT encode_profiles FROM recordings WHERE id = $1`, id,
	).Scan(&profiles); err != nil {
		t.Fatalf("loading encode_profiles for recording %d: %v", id, err)
	}
	return profiles
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

// 予約が無い録画（mirakc に直接起こされた手動録画などを模す）でも事後追加が
// 成功し、recordings.encode_profiles に追加専用（union + dedup）で反映され、
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
	reservationID := seedReservationForTest(t, pool, 5001)

	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		ReservationID:     &reservationID,
		Source:            "rule",
		Site:              db.DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           5001,
		ServiceName:       "ＯＨＫ",
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

// 原本が未 ingest（GetActiveOriginalMediaAsset が ErrNoRows）の録画への事後追加は
// 409 を返し、encode_profiles を変更せず、ジョブも投入しないこと（issue #133
// の受け入れ 3 個目 --- EnqueueMissingEncodes 単体はこのケースで黙って no-op に
// なるため、api 層で明示的に検査していることの固定）。
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
