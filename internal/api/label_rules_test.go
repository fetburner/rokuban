package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// postLabelRule は分類ルールを作り、応答を返す。
func postLabelRule(t *testing.T, srvURL string, body map[string]any) (*http.Response, LabelRule) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling label rule: %v", err)
	}
	resp, err := http.Post(srvURL+"/api/label-rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /api/label-rules: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	var out LabelRule
	raw2, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading label rule response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Logf("POST /api/label-rules status = %d, body = %s", resp.StatusCode, raw2)
		return resp, out
	}
	if err := json.Unmarshal(raw2, &out); err != nil {
		t.Fatalf("decoding created label rule: %v", err)
	}
	return resp, out
}

// seedPlayableRecording は棚の母集団に入る録画（active な原本を持つ）を作る。
func seedPlayableRecording(t *testing.T, pool *pgxpool.Pool, title string, eventID int32, start time.Time) int64 {
	t.Helper()
	id := seedRecordingFull(t, pool, seedRecordingOpts{
		title: title, start: start, status: "finished", eventID: eventID,
	})
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes, state)
VALUES ($1, 'original', $2, 1, 'active')`, id, fmt.Sprintf("test/orig-%d", id)); err != nil {
		t.Fatalf("seeding original media asset for %d: %v", id, err)
	}
	return id
}

func getShelves(t *testing.T, srvURL string, query url.Values) []RecordingShelf {
	t.Helper()
	var got []RecordingShelf
	resp := getJSON(t, srvURL+"/api/recording-shelves?"+query.Encode(), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/recording-shelves?%s status = %d, want 200", query.Encode(), resp.StatusCode)
	}
	return got
}

// 棚は実効シリーズごとにまとまり、表示名には代表の生タイトルを使う。
// value は正規化されたキーなので表示名にならない。
func TestListRecordingShelves_GroupsByEffectiveSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base.Add(-time.Hour))
	newest := seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base)
	seedPlayableRecording(t, pool, "無関係な番組", 3, base.Add(-2*time.Hour))

	shelves := getShelves(t, srv.URL, url.Values{"key": {"series"}})
	if len(shelves) != 2 {
		t.Fatalf("shelves = %+v, want 2", shelves)
	}
	// 件数の降順（同数なら値の昇順）。作品X が 2 件で先頭。
	if shelves[0].Value == nil || *shelves[0].Value != "作品X" || shelves[0].Count != 2 {
		t.Fatalf("first shelf = %+v, want 作品X with count 2", shelves[0])
	}
	if shelves[0].Title != "アニメ　作品X　第2話" {
		t.Errorf("shelf title = %q, want the representative's raw title", shelves[0].Title)
	}
	if shelves[0].RepresentativeId != newest {
		t.Errorf("representative = %d, want %d (newest program_start_at)", shelves[0].RepresentativeId, newest)
	}
}

// 棚の母集団から、ごみ箱・superseded・再生できない録画が外れる。
func TestListRecordingShelves_ExcludesUnplayable(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	kept := seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base)

	trashed := seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Minute))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET deleted_at = now() WHERE id = $1", trashed); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	superseded := seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(2*time.Minute))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET superseded_at = now() WHERE id = $1", superseded); err != nil {
		t.Fatalf("superseding: %v", err)
	}
	// 再生できる資産が無い録画（ingest されていない）。
	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "アニメ　作品X　第4話", start: base.Add(3 * time.Minute), status: "finished", eventID: 4,
	})

	shelves := getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 {
		t.Fatalf("shelves = %+v, want only 作品X", shelves)
	}
	if shelves[0].Count != 1 || shelves[0].RepresentativeId != kept {
		t.Errorf("shelf = %+v, want count 1 and the surviving recording %d", shelves[0], kept)
	}
}

// key が未知の値なら 400（黙って 0 件にしない。docs/api/rest.md の規約）。
func TestListRecordingShelves_RejectsUnknownKey(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	resp := getJSON(t, srv.URL+"/api/recording-shelves?key=genre", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("key=genre status = %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the 400 body: %v", err)
	}
	var errBody ErrorResponse
	if err := json.Unmarshal(body, &errBody); err != nil || errBody.Error == "" {
		t.Errorf("400 の本文が読めない（docs/api/rest.md「400 の本文を捨てない」）: %s", body)
	}
}

// 分類ルールを作ると、同じトランザクションで全件再評価のジョブが入る。
//
// **これが無いと、作成の直後に既にコミット済みの録画が古い棚のまま残る**
// （トリガーは自分の行しか見ない）。docs/data/series.md §8「評価結果の持ち方」。
func TestCreateLabelRule_EnqueuesReconcileInSameTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServerWithRiver(t, pool)

	resp, created := postLabelRule(t, srv.URL, map[string]any{
		"value": "作品X", "keyword": "作品X", "priority": 10,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	if created.Id == 0 || created.Value != "作品X" || created.Key != "series" {
		t.Fatalf("created = %+v", created)
	}
	if created.Priority == nil || *created.Priority != 10 {
		t.Errorf("priority = %v, want 10", created.Priority)
	}

	var kinds []string
	if err := pool.QueryRow(context.Background(),
		"SELECT array_agg(kind) FROM river_job WHERE kind = 'label_rule_reconcile'").Scan(&kinds); err != nil {
		t.Fatalf("reading river_job: %v", err)
	}
	if len(kinds) != 1 {
		t.Fatalf("label_rule_reconcile jobs = %v, want exactly 1", kinds)
	}
}

// 削除も全件再評価を投入する。当たりの表は勝者しか持たないので、CASCADE だけ
// では次点のルールに移らない（docs/data/series.md §8）。
func TestDeleteLabelRule_EnqueuesReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServerWithRiver(t, pool)
	ctx := context.Background()

	_, created := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X"})
	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("clearing river_job: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/label-rules/"+itoa(created.Id), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE label rule: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'label_rule_reconcile'").Scan(&count); err != nil {
		t.Fatalf("counting river_job: %v", err)
	}
	if count != 1 {
		t.Errorf("label_rule_reconcile jobs after delete = %d, want 1", count)
	}

	// 消えた id への DELETE は 404。
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/label-rules/"+itoa(created.Id), nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("second DELETE label rule: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp2.StatusCode)
	}
}

// 2 回続けて編集すると、再評価のジョブも 2 本入る。
//
// **一意化すると 2 本目が捨てられる。** 実行中に来た編集を捨てると、1 本目が古い
// ルール集合で評価し終えた時点で打ち止めになり、次の定期再評価（15 分）まで
// 反映されない。River は running を外した ByState を挿入時にエラーにするので、
// 「実行中だけ除く」では代用できない（docs/data/series.md §8）。
func TestCreateLabelRule_TwoEditsEnqueueTwoJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServerWithRiver(t, pool)
	ctx := context.Background()

	postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X"})
	postLabelRule(t, srv.URL, map[string]any{"value": "作品Y", "keyword": "作品Y"})

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'label_rule_reconcile'").Scan(&count); err != nil {
		t.Fatalf("counting river_job: %v", err)
	}
	if count != 2 {
		t.Errorf("label_rule_reconcile jobs = %d, want 2 (the second edit must not be dropped)", count)
	}
}

// PATCH は上書きで、keyword の変更も再評価を投入する。存在しない id は 404。
func TestUpdateLabelRule_EnqueuesReconcileAndReports404(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServerWithRiver(t, pool)
	ctx := context.Background()

	_, created := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X", "priority": 0})
	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("clearing river_job: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{"value": "作品Y", "keyword": "作品Y", "priority": 5})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/label-rules/"+itoa(created.Id), bytes.NewReader(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH label rule: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var updated LabelRule
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decoding updated label rule: %v", err)
	}
	if updated.Value != "作品Y" || updated.Keyword != "作品Y" {
		t.Errorf("updated = %+v, want the new value and keyword", updated)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'label_rule_reconcile'").Scan(&count); err != nil {
		t.Fatalf("counting river_job: %v", err)
	}
	if count != 1 {
		t.Errorf("label_rule_reconcile jobs after patch = %d, want 1", count)
	}

	raw, _ = json.Marshal(map[string]any{"value": "作品Y", "keyword": "作品Y"})
	req, _ = http.NewRequest(http.MethodPatch, srv.URL+"/api/label-rules/999999", bytes.NewReader(raw))
	missing, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH missing label rule: %v", err)
	}
	defer func() { _ = missing.Body.Close() }()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("patch missing status = %d, want 404", missing.StatusCode)
	}
}

// 何も主張しないルールは 400 で拒否する（不変条件 10）。値が正規化で空になる
// 場合と、キーワードが空の場合。
func TestCreateLabelRule_RejectsMeaninglessRules(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServerWithRiver(t, pool)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"記号のみの値", map[string]any{"value": "【】", "keyword": "kw"}},
		{"空の値", map[string]any{"value": "", "keyword": "kw"}},
		{"空のキーワード", map[string]any{"value": "作品X", "keyword": ""}},
		{"未知の key", map[string]any{"key": "genre", "value": "作品X", "keyword": "kw"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := postLabelRule(t, srv.URL, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// 一覧・単体の Recording に実効シリーズが載る。分類ルールを当てると値が変わる。
func TestListRecordings_ExposesEffectiveSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "NHK高校講座　日本史　第1回", start: base, status: "finished", eventID: 1,
	})
	seedRecordingFull(t, pool, seedRecordingOpts{
		title: "【特集】", start: base.Add(time.Minute), status: "finished", eventID: 2,
	})

	// 分類ルールが無ければ自動キー。記号のみのタイトルは NULL。
	titles := getRecordingsSeries(t, srv.URL)
	if len(titles) != 2 {
		t.Fatalf("recordings = %d, want 2", len(titles))
	}
	byTitle := map[string]*string{}
	for _, r := range titles {
		byTitle[r.Title] = r.Series
	}
	if got := byTitle["NHK高校講座　日本史　第1回"]; got == nil || *got != "NHK高校講座" {
		t.Errorf("series = %v, want the automatic key NHK高校講座", got)
	}
	if got := byTitle["【特集】"]; got != nil {
		t.Errorf("series = %q, want null for a title with no automatic key", *got)
	}

	// ルールが当たると実効シリーズがその値になる（ビュー経由。単体 GET も同形）。
	postLabelRule(t, srv.URL, map[string]any{"value": "日本史", "keyword": "日本史"})
	// 全件再評価はワーカーが実行するので、ここでは同じ文を直接適用する。
	if _, err := pool.Exec(context.Background(), applyLabelRuleReevaluationSQL); err != nil {
		t.Fatalf("applying re-evaluation: %v", err)
	}

	for _, r := range getRecordingsSeries(t, srv.URL) {
		if r.Title != "NHK高校講座　日本史　第1回" {
			continue
		}
		if r.Series == nil || *r.Series != "日本史" {
			t.Errorf("series after adding a rule = %v, want 日本史", r.Series)
		}
	}
}

// applyLabelRuleReevaluationSQL はワーカーが呼ぶのと同じ文（内部 API を持たない
// テストからジョブの実行を待たずに適用するため。文自体はワーカーと同じ
// クエリを使う）。
const applyLabelRuleReevaluationSQL = `
WITH winners AS MATERIALIZED (
    SELECT r.id AS recording_id, public.label_rule_winner(r.title) AS label_rule_id
    FROM recordings r
), upserted AS (
    INSERT INTO label_rule_hits (recording_id, label_rule_id)
    SELECT w.recording_id, w.label_rule_id
    FROM winners w
    WHERE w.label_rule_id IS NOT NULL
    ON CONFLICT (recording_id) DO UPDATE
        SET label_rule_id = EXCLUDED.label_rule_id
        WHERE label_rule_hits.label_rule_id IS DISTINCT FROM EXCLUDED.label_rule_id
    RETURNING 1
), removed AS (
    DELETE FROM label_rule_hits h
    WHERE NOT EXISTS (
        SELECT 1 FROM winners w
        WHERE w.recording_id = h.recording_id AND w.label_rule_id IS NOT NULL
    )
    RETURNING 1
)
SELECT (SELECT count(*) FROM upserted) + (SELECT count(*) FROM removed)`

func getRecordingsSeries(t *testing.T, srvURL string) []Recording {
	t.Helper()
	var got []Recording
	resp := getJSON(t, srvURL+"/api/recordings", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/recordings status = %d, want 200", resp.StatusCode)
	}
	return got
}

// newAPIServerWithRiver は River クライアント付きの API を立てる（ヒント投入の
// 検査用）。insert-only のクライアントで足りる。
func newAPIServerWithRiver(t *testing.T, pool *pgxpool.Pool) *httptest.Server {
	t.Helper()
	client, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating river client: %v", err)
	}
	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool, RiverClient: client}))
	t.Cleanup(srv.Close)
	return srv
}
