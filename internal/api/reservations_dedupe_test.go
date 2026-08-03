package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// M2-6 重複排除の API 表現（issue #24）。
//
// skip は reservations の列ではなく db.EffectiveOptions の結果
// （base + overrides + program_intents.action）で、根拠 2 列は ruler が毎パス
// 作り直す導出列をそのまま出す。

// reservationDedupeResp は本ファイルで確認したいフィールドだけを持つ。
// dedup 2 列と skip は他のテストのデコード用型に含まれていないので別に定義する。
type reservationDedupeResp struct {
	Id                    int64    `json:"id"`
	Skip                  bool     `json:"skip"`
	DedupMatchRecordingId *int64   `json:"dedupMatchRecordingId"`
	DedupSimilarity       *float32 `json:"dedupSimilarity"`
}

func getReservationDedupeJSON(t *testing.T, srv *httptest.Server, id int64) reservationDedupeResp {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/reservations/" + itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/reservations/%d status = %d, want 200", id, resp.StatusCode)
	}
	var got reservationDedupeResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return got
}

func listReservationsDedupeJSON(t *testing.T, srv *httptest.Server) []reservationDedupeResp {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/reservations")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/reservations status = %d, want 200", resp.StatusCode)
	}
	var got []reservationDedupeResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return got
}

// insertDedupeSkippedReservation は「重複としてスキップされた」予約行を直接作る
// （ruler を経由しない）。base.skip と根拠 2 列は ruler の出力を模したもの。
//
// #27 で番組の事実のスナップショットが program_snapshots に抽出され、
// reservations への FK が張られたため、予約行より先に program_snapshots を作る。
func insertDedupeSkippedReservation(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	programID int64, ruleID, recordingID int64, similarity float32,
) int64 {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('default', $1, 'テスト番組', $2, 1800000, 11500, 1150, 'GR', '27', $3, 'テスト局')`,
		programID, start, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO reservations (
  site, program_id, rule_id, base,
  dedup_match_recording_id, dedup_similarity
) VALUES (
  'default', $1, $2, '{"skip":true,"priority":10}'::jsonb, $3, $4
) RETURNING id`, programID, ruleID, recordingID, similarity).Scan(&id)
	if err != nil {
		t.Fatalf("inserting dedupe-skipped reservation fixture: %v", err)
	}
	return id
}

// insertRecordingFixture は根拠として参照する録画履歴を 1 行作る。
func insertRecordingFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, ruleID int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO recordings (
  rule_id, source, site, network_id, service_id, event_id,
  service_name, channel_type, channel, title,
  program_start_at, program_duration_ms, status
) VALUES ($1, 'rule', 'default', 11500, 1150, 4321, 'テスト局', 'GR', '27',
          'テスト番組', now() - interval '7 days', 1800000, 'finished')
RETURNING id`, ruleID).Scan(&id)
	if err != nil {
		t.Fatalf("inserting recording fixture: %v", err)
	}
	return id
}

// TestGetReservation_DedupeSkipExposed は重複としてスキップされた予約が
// skip = true と根拠 2 列を返すことを確認する。一覧でも同じ値が出る
// （表示経路が reservationFromRow の 1 箇所に集約されていることの確認）。
func TestGetReservation_DedupeSkipExposed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041234
	ruleID := insertRuleFixture(t, pool, ctx)
	recordingID := insertRecordingFixture(t, pool, ctx, ruleID)
	resID := insertDedupeSkippedReservation(t, pool, ctx, programID, ruleID, recordingID, 0.875)

	got := getReservationDedupeJSON(t, srv, resID)
	if !got.Skip {
		t.Error("skip = false, want true (base.skip が立っている予約)")
	}
	if got.DedupMatchRecordingId == nil {
		t.Error("dedupMatchRecordingId is absent, want the matched recording id")
	} else if *got.DedupMatchRecordingId != recordingID {
		t.Errorf("dedupMatchRecordingId = %d, want %d", *got.DedupMatchRecordingId, recordingID)
	}
	if got.DedupSimilarity == nil {
		t.Error("dedupSimilarity is absent, want 0.875")
	} else if *got.DedupSimilarity != 0.875 {
		t.Errorf("dedupSimilarity = %v, want 0.875", *got.DedupSimilarity)
	}

	list := listReservationsDedupeJSON(t, srv)
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if !list[0].Skip || list[0].DedupMatchRecordingId == nil {
		t.Errorf("list entry lost skip/evidence: %+v", list[0])
	}
}

// TestGetReservation_RecordIntentClearsDedupeSkip は「この番組は重複扱いにしない」
// （EPGStation#473）の表示側: base.skip が立っていても program_intents に
// action='record' があれば skip = false になる。根拠 2 列は残る --- 「重複と
// 判定されたが録る」を UI で説明できる必要がある。
func TestGetReservation_RecordIntentClearsDedupeSkip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115051234
	ruleID := insertRuleFixture(t, pool, ctx)
	recordingID := insertRecordingFixture(t, pool, ctx, ruleID)
	resID := insertDedupeSkippedReservation(t, pool, ctx, programID, ruleID, recordingID, 0.9)

	// 意図が無い時点では skip = true（対照）。
	if got := getReservationDedupeJSON(t, srv, resID); !got.Skip {
		t.Fatal("precondition: skip should be true before the record intent")
	}

	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentRecord,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}

	got := getReservationDedupeJSON(t, srv, resID)
	if got.Skip {
		t.Error("skip = true; the user's record intent must beat the dedupe skip (EPGStation#473)")
	}
	if got.DedupMatchRecordingId == nil {
		t.Error("dedupMatchRecordingId was dropped; the evidence must stay so the UI can explain it")
	}
}

// TestGetReservation_NoDedupeEvidenceOmitsFields は根拠が無い予約で 2 列が
// 省略され skip = false になることを確認する（上の 2 テストの反対方向）。
func TestGetReservation_NoDedupeEvidenceOmitsFields(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115061234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)

	got := getReservationDedupeJSON(t, srv, resID)
	if got.Skip {
		t.Error("skip = true for a reservation without base.skip")
	}
	if got.DedupMatchRecordingId != nil {
		t.Errorf("dedupMatchRecordingId = %d, want absent", *got.DedupMatchRecordingId)
	}
	if got.DedupSimilarity != nil {
		t.Errorf("dedupSimilarity = %v, want absent", *got.DedupSimilarity)
	}
}

// TestGetReservation_SkipIntentSetsSkip は base.skip が無くても
// program_intents.action='skip' なら skip = true になることを確認する
// （skip が「列」ではなく effective の結果であることの確認）。
func TestGetReservation_SkipIntentSetsSkip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115071234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)

	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: "default", ProgramID: programID, Action: db.IntentSkip,
	}); err != nil {
		t.Fatalf("seeding intent: %v", err)
	}

	if got := getReservationDedupeJSON(t, srv, resID); !got.Skip {
		t.Error("skip = false with program_intents.action = 'skip'")
	}
}

// TestGetReservation_BrokenBaseJSONFails は壊れた base jsonb を握りつぶさない
// ことを確認する（docs/schema.md §3「jsonb の Unmarshal 失敗を握りつぶさない」）。
// skip = false を返して黙って進むと「mirakc に同期されないのに理由が UI から
// 読めない」という一番説明しにくい状態になるため、500 で鳴らす。
func TestGetReservation_BrokenBaseJSONFails(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115081234
	ruleID := insertRuleFixture(t, pool, ctx)
	resID := insertReservationDirect(t, pool, ctx, programID, &ruleID, 11500, 1150)
	// jsonb として妥当だが ReservationOptions にデコードできない値を入れる。
	if _, err := pool.Exec(ctx,
		`UPDATE reservations SET base = '{"skip":"yes"}'::jsonb WHERE id = $1`, resID); err != nil {
		t.Fatalf("corrupting base: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/reservations/" + itoa(resID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (broken base jsonb must not be swallowed)", resp.StatusCode)
	}
}
