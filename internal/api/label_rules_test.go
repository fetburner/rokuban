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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
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
func seedPlayableRecording(t *testing.T, pool *pgxpool.Pool, title string, eventID int32, start time.Time, opts ...func(*dbtest.RecordingParams)) int64 {
	t.Helper()
	defaults := func(p *dbtest.RecordingParams) {
		p.Title = title
		p.ProgramStartAt = start
		p.EventID = eventID
		p.Status = "finished"
	}
	id := dbtest.Recording(t, pool, append([]func(*dbtest.RecordingParams){defaults}, opts...)...)
	dbtest.MediaAsset(t, pool, id)
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
// value は画面のシリーズ名になる実効キーで、title は副見出しの生タイトル。
// count は生きている録画全体、playableCount はそのうち再生できる録画だけを数える。
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
	if shelves[0].PlayableCount != 2 {
		t.Errorf("playable count = %d, want 2", shelves[0].PlayableCount)
	}
	if shelves[0].Title != "アニメ　作品X　第2話" {
		t.Errorf("shelf title = %q, want the representative's raw title", shelves[0].Title)
	}
	if shelves[0].RepresentativeId != newest {
		t.Errorf("representative = %d, want %d (newest program_start_at)", shelves[0].RepresentativeId, newest)
	}
	if want := fmt.Sprintf("/api/media/recordings/%d/thumbnail", newest); shelves[0].ThumbnailUrl != want {
		t.Errorf("shelf thumbnailUrl = %q, want %q (the representative's thumbnail)", shelves[0].ThumbnailUrl, want)
	}
	if !shelves[0].LatestStartAt.Equal(base) {
		t.Errorf("latest start = %s, want %s", shelves[0].LatestStartAt, base)
	}
}

// 棚の母集団は生きている録画全体。ごみ箱・superseded は外し、再生できない録画も
// 棚には残す。再生できる件数は別列で返す。
func TestListRecordingShelves_IncludesLivePopulation(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base)

	trashed := seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Minute))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET deleted_at = now() WHERE id = $1", trashed); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	superseded := seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(2*time.Minute))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET superseded_at = now() WHERE id = $1", superseded); err != nil {
		t.Fatalf("superseding: %v", err)
	}
	// 再生できる資産が無い録画も、録画中・取り込み待ち・失敗を含めて棚に残す。
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第4話", base.Add(3*time.Minute), "recording", 4
	})
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第5話", base.Add(4*time.Minute), "finished", 5
	})
	failed := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第6話", base.Add(5*time.Minute), "failed", 6
	})

	shelves := getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 {
		t.Fatalf("shelves = %+v, want one live 作品X shelf", shelves)
	}
	if shelves[0].Count != 4 || shelves[0].PlayableCount != 1 || shelves[0].RepresentativeId != failed {
		t.Errorf("shelf = %+v, want count 4, playable count 1, representative %d", shelves[0], failed)
	}
	if !shelves[0].LatestStartAt.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("latest start = %s, want %s", shelves[0].LatestStartAt, base.Add(5*time.Minute))
	}
}

// 棚は生きている録画を数える。ごみ箱・superseded は外れるが、録画中・取り込み待ち・
// 失敗の録画も棚の母集団に残し、再生可能件数を別に返す。encoded だけの録画も再生できる。
func TestListRecordingShelves_PopulationAndRepresentative(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	// 同じ棚の 3 件。1 件は encoded の派生物だけを持つ。
	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base.Add(-2*time.Hour))
	encodedOnly := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第2話", base, "finished", 2
	})
	profile := "h264"
	dbtest.MediaAsset(t, pool, encodedOnly, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "test/encoded"
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(-time.Hour))

	// 母集団から外れるもの: ごみ箱 / superseded。再生できる資産が無い行は残る。
	trashed := seedPlayableRecording(t, pool, "アニメ　作品X　第4話", 4, base.Add(time.Hour))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET deleted_at = now() WHERE id = $1", trashed); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	superseded := seedPlayableRecording(t, pool, "アニメ　作品X　第5話", 5, base.Add(2*time.Hour))
	if _, err := pool.Exec(ctx, "UPDATE recordings SET superseded_at = now() WHERE id = $1", superseded); err != nil {
		t.Fatalf("superseding: %v", err)
	}
	noAsset := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第6話", base.Add(3*time.Hour), "finished", 6
	})

	// 値の無い録画は NULL の棚として返す（UI は表示しないが、API では欠落と区別する）。
	nullKey := seedPlayableRecording(t, pool, "【特集】", 7, base.Add(4*time.Hour))

	shelves := getShelves(t, srv.URL, url.Values{})
	byValue := map[string]RecordingShelf{}
	for _, s := range shelves {
		byValue[ptrStr(s.Value)] = s
	}
	if len(shelves) != 2 {
		t.Fatalf("shelves = %d (%v), want 2 (作品X and the NULL shelf)", len(shelves), byValue)
	}
	got, ok := byValue["作品X"]
	if !ok {
		t.Fatalf("no 作品X shelf in %v", byValue)
	}
	if got.Count != 4 {
		t.Errorf("作品X count = %d, want 4 (deleted / superseded rows are excluded, assetless rows remain)", got.Count)
	}
	if got.PlayableCount != 3 {
		t.Errorf("作品X playable count = %d, want 3 (the encoded-only recording is playable)", got.PlayableCount)
	}
	if got.RepresentativeId != noAsset || got.Title != "アニメ　作品X　第6話" {
		t.Errorf("作品X representative = %d %q, want %d (newest program_start_at) and its raw title", got.RepresentativeId, got.Title, noAsset)
	}
	nullShelf, ok := byValue["<nil>"]
	if !ok {
		t.Fatalf("no NULL shelf in %v", byValue)
	}
	if nullShelf.Count != 1 || nullShelf.RepresentativeId != nullKey {
		t.Errorf("NULL shelf = %+v, want count 1 and the 特集 recording", nullShelf)
	}
}

// 未視聴は放送イベント単位で数え、別拠点・ごみ箱・supersede 済み・purge 済みの録画の視聴済み印も束ねる。
func TestListRecordingShelves_CountsUnwatchedBroadcastEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	// 同じ放送を 2 拠点で録り、片方に視聴済み印を付ける。
	watched := seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Site = "tokyo"
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Site = "osaka"
	})
	if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, watched); err != nil {
		t.Fatalf("marking broadcast watched: %v", err)
	}

	// もう 1 放送は 2 拠点で録るが印を付けない。2 行ではなく 1 件と数える。
	secondEpisode := seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "tokyo"
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "osaka"
	})
	// 視聴済み印が supersede 済み録画にだけ残る場合も、別拠点の生きた録画を除外する。
	supersededWatched := seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(2*time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "tokyo"
	})
	if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, supersededWatched); err != nil {
		t.Fatalf("marking superseded broadcast watched: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET superseded_at = now() WHERE id = $1`, supersededWatched); err != nil {
		t.Fatalf("superseding watched recording: %v", err)
	}
	thirdEpisode := seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(2*time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "osaka"
	})

	// 同時刻でも network_id / service_id が違えば別の放送イベント。
	networkEvent := seedPlayableRecording(t, pool, "アニメ　作品X　第4話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.NetworkID = 32679
	})
	serviceEvent := seedPlayableRecording(t, pool, "アニメ　作品X　第5話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.ServiceID = 5169
	})

	// 視聴済みイベント（base）と同時刻でも network_id / service_id が違えば未視聴。
	// 視聴済み印の JOIN 条件から network_id / service_id を外すと、ここが視聴済みに数えられる。
	networkAtWatched := seedPlayableRecording(t, pool, "アニメ　作品X　第8話", 8, base, func(p *dbtest.RecordingParams) {
		p.NetworkID = 32679
	})
	serviceAtWatched := seedPlayableRecording(t, pool, "アニメ　作品X　第9話", 9, base, func(p *dbtest.RecordingParams) {
		p.ServiceID = 5169
	})

	// 再生できない生きた録画（録画中）は未視聴に数えない。
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第6話", base.Add(3*time.Hour), "recording", 6
	})
	// 視聴済み印が purge 済み tombstone にだけ残る場合も、生きている別拠点の録画を除外する。
	purgedWatched := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "アニメ　作品X　第7話", base.Add(4*time.Hour), "finished", 7
		p.Site = "tokyo"
	})
	assetID := dbtest.MediaAsset(t, pool, purgedWatched, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "test/purged"
	})
	dbtest.SetMediaAssetState(t, pool, assetID, "deleted")
	if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, purgedWatched); err != nil {
		t.Fatalf("marking purged broadcast watched: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now(), purged_at = now() WHERE id = $1`, purgedWatched); err != nil {
		t.Fatalf("purging watched recording: %v", err)
	}
	purgedLive := seedPlayableRecording(t, pool, "アニメ　作品X　第7話", 7, base.Add(4*time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "osaka"
	})

	shelves := getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 || shelves[0].Value == nil || *shelves[0].Value != "作品X" {
		t.Fatalf("shelves = %+v, want one 作品X shelf", shelves)
	}
	if shelves[0].UnwatchedCount != 5 {
		t.Fatalf("unwatched count = %d, want 5 distinct unwatched events", shelves[0].UnwatchedCount)
	}

	// 印を持つ録画を後からごみ箱へ移しても、生きている同一放送は未視聴にならない。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, watched); err != nil {
		t.Fatalf("trashing watched recording: %v", err)
	}
	shelves = getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 || shelves[0].UnwatchedCount != 5 {
		t.Fatalf("shelves after trashing watched recording = %+v, want five unwatched events", shelves)
	}

	// 視聴を解除すると、生きている同一放送が再び未視聴に数えられる。
	if _, err := pool.Exec(ctx, `DELETE FROM recording_watched WHERE recording_id = $1`, watched); err != nil {
		t.Fatalf("clearing watched marker: %v", err)
	}
	shelves = getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 || shelves[0].UnwatchedCount != 6 {
		t.Fatalf("shelves after clearing watched marker = %+v, want six unwatched events", shelves)
	}

	// 全イベントに視聴済み印を付けても、棚は残り件数だけが 0 になる。
	for _, id := range []int64{watched, secondEpisode, thirdEpisode, networkEvent, serviceEvent, networkAtWatched, serviceAtWatched, purgedLive} {
		if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatalf("marking event recording %d watched: %v", id, err)
		}
	}
	shelves = getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 || shelves[0].UnwatchedCount != 0 {
		t.Fatalf("shelves after watching every event = %+v, want a shelf with zero unwatched events", shelves)
	}
}

// 同じ放送が 2 拠点の両方で視聴済みでも、棚の件数は録画行の数のままで増えない。
//
// 視聴済み印を束ねる CTE が DISTINCT でないと、印が 2 行ある放送の live 行が複製され、
// 件数が水増しされる（自動 PUT は行ごとに印を付けるので実際に起きる）。
func TestListRecordingShelves_WatchedOnEveryRowDoesNotInflateCounts(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	for _, site := range []string{"tokyo", "osaka"} {
		id := seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
			p.Site = site
		})
		if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatalf("marking %s row watched: %v", site, err)
		}
	}

	shelves := getShelves(t, srv.URL, url.Values{})
	if len(shelves) != 1 {
		t.Fatalf("shelves = %+v, want one shelf", shelves)
	}
	if got := shelves[0]; got.Count != 2 || got.PlayableCount != 2 || got.UnwatchedCount != 0 {
		t.Fatalf("count/playable/unwatched = %d/%d/%d, want 2/2/0", got.Count, got.PlayableCount, got.UnwatchedCount)
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

// 分類ルールを作った後、commit 済みの状態で全件再評価のジョブを依頼する。
//
// **これが無いと、作成の直後に既にコミット済みの録画が古い棚のまま残る**
// （トリガーは自分の行しか見ない）。docs/data/series.md §8「評価結果の持ち方」。
func TestCreateLabelRule_EnqueuesReconcileAfterCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	baseEnqueuer := newRecordingEnqueuer()
	enqueuer := &labelRuleCommitCheckingEnqueuer{recordingEnqueuer: baseEnqueuer, pool: pool}
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Ready:          pool.Ping,
		RecordingQuery: pgstoretest.Recordings(pool), LabelRules: pgstoretest.LabelRules(pool), Enqueuer: enqueuer,
	}))
	t.Cleanup(srv.Close)

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

	calls := enqueuer.CallsOfKind("label_rule_reconcile")
	if len(calls) != 1 {
		t.Fatalf("label_rule_reconcile enqueue count = %d, want 1", len(calls))
	}
}

type labelRuleCommitCheckingEnqueuer struct {
	*recordingEnqueuer
	pool *pgxpool.Pool
}

func (e *labelRuleCommitCheckingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	var visible int64
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM label_rules WHERE value = '作品X'`).Scan(&visible); err != nil {
		return jobq.Result{}, fmt.Errorf("checking committed label rule: %w", err)
	}
	if visible != 1 {
		return jobq.Result{}, fmt.Errorf("label rule count visible during enqueue = %d, want 1 committed row", visible)
	}
	return e.recordingEnqueuer.Enqueue(ctx, args, opts...)
}

func TestCreateLabelRule_StoreFailureDoesNotEnqueue(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, enqueuer := newAPIServerWithRecordingEnqueuer(t, pool)
	const constraint = "label_rules_test_reject_keyword"
	if _, err := pool.Exec(context.Background(), `ALTER TABLE label_rules ADD CONSTRAINT `+constraint+` CHECK (keyword <> 'db-failure')`); err != nil {
		t.Fatalf("adding rejecting constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `ALTER TABLE label_rules DROP CONSTRAINT IF EXISTS `+constraint); err != nil {
			t.Errorf("dropping rejecting constraint: %v", err)
		}
	})

	resp, _ := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "db-failure"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want 500 for failed store write", resp.StatusCode)
	}
	if got := len(enqueuer.CallsOfKind("label_rule_reconcile")); got != 0 {
		t.Errorf("label_rule_reconcile enqueue count after store failure = %d, want 0", got)
	}
}

// 削除も全件再評価を投入する。当たりの表は勝者しか持たないので、CASCADE だけ
// では次点のルールに移らない（docs/data/series.md §8）。
func TestDeleteLabelRule_EnqueuesReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, enqueuer := newAPIServerWithRecordingEnqueuer(t, pool)

	_, created := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X"})
	enqueuer.Reset()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/label-rules/"+itoa(created.Id), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE label rule: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	if got := len(enqueuer.CallsOfKind("label_rule_reconcile")); got != 1 {
		t.Errorf("label_rule_reconcile enqueue count = %d, want 1", got)
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

// 2 回続けて編集すると、API が再評価の投入を 2 回依頼する。
func TestCreateLabelRule_TwoEditsEnqueueTwoJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, enqueuer := newAPIServerWithRecordingEnqueuer(t, pool)

	postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X"})
	postLabelRule(t, srv.URL, map[string]any{"value": "作品Y", "keyword": "作品Y"})

	if got := len(enqueuer.CallsOfKind("label_rule_reconcile")); got != 2 {
		t.Errorf("label_rule_reconcile enqueue count after two edits = %d, want 2", got)
	}
}

// PATCH は上書きで、keyword の変更も再評価を投入する。存在しない id は 404。
func TestUpdateLabelRule_EnqueuesReconcileAndReports404(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, enqueuer := newAPIServerWithRecordingEnqueuer(t, pool)

	_, created := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "作品X", "priority": 0})
	enqueuer.Reset()
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

	if got := len(enqueuer.CallsOfKind("label_rule_reconcile")); got != 1 {
		t.Errorf("label_rule_reconcile enqueue count after edit = %d, want 1", got)
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
	srv, _ := newAPIServerWithRecordingEnqueuer(t, pool)

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

	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "NHK高校講座　日本史　第1回", base, "finished", 1
	})
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "【特集】", base.Add(time.Minute), "finished", 2
	})

	// 分類ルールが無ければ自動キー。記号のみのタイトルは NULL。
	titles := getRecordingsSeries(t, srv.URL)
	if len(titles) != 2 {
		t.Fatalf("recordings = %d, want 2", len(titles))
	}
	byTitle := map[string]Recording{}
	for _, r := range titles {
		byTitle[r.Title] = r
	}
	automatic := byTitle["NHK高校講座　日本史　第1回"]
	if automatic.Series == nil || *automatic.Series != "NHK高校講座" {
		t.Errorf("series = %v, want the automatic key NHK高校講座", automatic.Series)
	}
	if automatic.SeriesKey == nil || *automatic.SeriesKey != "NHK高校講座" {
		t.Errorf("seriesKey = %v, want the automatic key NHK高校講座", automatic.SeriesKey)
	}
	nullAutomatic := byTitle["【特集】"]
	if nullAutomatic.Series != nil || nullAutomatic.SeriesKey != nil {
		t.Errorf("series/seriesKey = %v/%v, want null for a title with no automatic key", nullAutomatic.Series, nullAutomatic.SeriesKey)
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
			t.Errorf("series after adding a rule = %q, want 日本史", ptrStr(r.Series))
		}
		if r.SeriesKey == nil || *r.SeriesKey != "NHK高校講座" {
			t.Errorf("seriesKey after adding a rule = %q, want NHK高校講座", ptrStr(r.SeriesKey))
		}
		var detail Recording
		resp := getJSON(t, srv.URL+fmt.Sprintf("/api/recordings/%d", r.Id), &detail)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("detail status = %d, want 200", resp.StatusCode)
		}
		if detail.Series == nil || *detail.Series != "日本史" || detail.SeriesKey == nil || *detail.SeriesKey != "NHK高校講座" {
			t.Errorf("detail series/seriesKey = %q/%q, want 日本史/NHK高校講座", ptrStr(detail.Series), ptrStr(detail.SeriesKey))
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

func newAPIServerWithRecordingEnqueuer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *recordingEnqueuer) {
	t.Helper()
	enqueuer := newRecordingEnqueuer()
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Ready:          pool.Ping,
		RecordingQuery: pgstoretest.Recordings(pool), LabelRules: pgstoretest.LabelRules(pool), Enqueuer: enqueuer,
	}))
	t.Cleanup(srv.Close)
	return srv, enqueuer
}

// value は自動キーと同じ正規化を通るので、最初の空白で切れる。API は実効の棚キー
// （valueKey）を返し、食い違いが UI に見えるようにする。`NHK高校講座 数学I` と
// `NHK高校講座 化学` は同じ棚キーになる（割るつもりのルールが同じ棚に落ちる）。
func TestLabelRule_ValueKeyIsTheTruncatedShelfKey(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, _ := newAPIServerWithRecordingEnqueuer(t, pool)

	resp, created := postLabelRule(t, srv.URL, map[string]any{"value": "NHK高校講座 数学I", "keyword": "数学"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if created.Value != "NHK高校講座 数学I" || created.ValueKey != "NHK高校講座" {
		t.Errorf("created value/valueKey = %q / %q, want %q / %q",
			created.Value, created.ValueKey, "NHK高校講座 数学I", "NHK高校講座")
	}
	var list []LabelRule
	if r := getJSON(t, srv.URL+"/api/label-rules", &list); r.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", r.StatusCode)
	}
	if len(list) != 1 || list[0].ValueKey != "NHK高校講座" {
		t.Errorf("listed valueKey = %+v, want NHK高校講座", list)
	}

	var preview struct {
		ValueKey string `json:"valueKey"`
	}
	q := url.Values{"value": {"ドラマ「半沢直樹」"}}
	if r := getJSON(t, srv.URL+"/api/label-rule-value-key?"+q.Encode(), &preview); r.StatusCode != http.StatusOK {
		t.Fatalf("value-key status = %d", r.StatusCode)
	}
	if preview.ValueKey != "ドラマ" {
		t.Errorf("preview valueKey = %q, want %q", preview.ValueKey, "ドラマ")
	}
	q = url.Values{"value": {"【】"}}
	getJSON(t, srv.URL+"/api/label-rule-value-key?"+q.Encode(), &preview)
	if preview.ValueKey != "" {
		t.Errorf("preview valueKey for symbols only = %q, want empty", preview.ValueKey)
	}
}

// DB の CHECK は btrim(keyword) <> ”。空白だけのキーワードは 500 ではなく 400。
// priority が int32 を外れる値も黙って折り返さず 400。
func TestCreateLabelRule_RejectsBlankKeywordAndOutOfRangePriority(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv, _ := newAPIServerWithRecordingEnqueuer(t, pool)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"空白だけのキーワード", map[string]any{"value": "作品X", "keyword": "   "}},
		{"int32 を超える priority", map[string]any{"value": "作品X", "keyword": "kw", "priority": 4294967296}},
		{"int32 を下回る priority", map[string]any{"value": "作品X", "keyword": "kw", "priority": -2147483649}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := postLabelRule(t, srv.URL, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	resp, ok := postLabelRule(t, srv.URL, map[string]any{"value": "作品X", "keyword": "kw", "priority": 2147483647})
	if resp.StatusCode != http.StatusCreated || ok.Priority == nil || *ok.Priority != 2147483647 {
		t.Fatalf("max int32 priority: status = %d, priority = %v, want 201 / 2147483647", resp.StatusCode, ok.Priority)
	}
}

// ptrStr はエラーメッセージ用に *string の値を返す（nil は "<nil>"）。
func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// latestStartAt は openapi の「常に UTC」どおり、pgx が返す Location によらず `Z` で返す。
// time.Local を書き換えると同じパッケージで並行する goroutine と data race になるので、
// この pool だけ timestamptz を JST で decode させる（UTC() を外すと +09:00 で返って落ちる）。
func TestListRecordingShelves_LatestStartAtIsUTC(t *testing.T) {
	pool := testutil.SetupDB(t)
	jst := time.FixedZone("JST", 9*60*60)
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name:  "timestamptz",
			OID:   pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: jst},
		})
		return nil
	}
	jstPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("creating JST-scanning pool: %v", err)
	}
	t.Cleanup(jstPool.Close)
	srv := newAPIServer(t, jstPool)

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	resp, err := http.Get(srv.URL + "/api/recording-shelves")
	if err != nil {
		t.Fatalf("GET /api/recording-shelves: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), `"latestStartAt":"2026-01-02T03:04:05Z"`) {
		t.Errorf("body = %s, want latestStartAt in UTC (…Z)", body)
	}
}

// shelfByValue は棚の一覧から値で 1 件引く。無ければ ok=false。
func shelfByValue(shelves []RecordingShelf, value string) (RecordingShelf, bool) {
	for _, s := range shelves {
		if s.Value != nil && *s.Value == value {
			return s, true
		}
	}
	return RecordingShelf{}, false
}

// ジャンルで絞ると、そのジャンルでない回は棚の件数から除かれ、該当する回が 0 件の棚は返らない。
func TestListRecordingShelves_FilterByGenreCountsOnlyMatchingEpisodes(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)
	anime := `[{"lv1":7,"lv2":0,"un1":0,"un2":0}]`
	drama := `[{"lv1":3,"lv2":0,"un1":0,"un2":0}]`

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Genres = json.RawMessage(anime)
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.Genres = json.RawMessage(anime)
	})
	// 同じシリーズでもジャンルの違う回（特番など）は絞り込みで外れる。
	seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(2*time.Hour), func(p *dbtest.RecordingParams) {
		p.Genres = json.RawMessage(drama)
	})
	seedPlayableRecording(t, pool, "ドラマ　作品Y　第1話", 4, base, func(p *dbtest.RecordingParams) {
		p.Genres = json.RawMessage(drama)
	})

	shelves := getShelves(t, srv.URL, url.Values{"genre": {"7"}})
	if len(shelves) != 1 {
		t.Fatalf("shelves = %+v, want only 作品X (作品Y has no anime episode)", shelves)
	}
	x, ok := shelfByValue(shelves, "作品X")
	if !ok || x.Count != 2 || x.PlayableCount != 2 || x.UnwatchedCount != 2 {
		t.Errorf("作品X = %+v, want count / playable / unwatched 2 (the drama episode is excluded)", x)
	}
}

// 2 つのクールにまたがるシリーズは、どちらの期間で絞っても返り、件数・代表・最新はその期間内の回から出す。
func TestListRecordingShelves_FilterByPeriodSplitsSeriesAcrossCours(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	jst := time.FixedZone("JST", 9*60*60)
	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, time.Date(2026, 1, 8, 1, 0, 0, 0, jst))
	winter2 := seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, time.Date(2026, 3, 26, 1, 0, 0, 0, jst))
	spring := seedPlayableRecording(t, pool, "アニメ　作品X　第3話", 3, time.Date(2026, 4, 2, 1, 0, 0, 0, jst))

	cases := []struct {
		name           string
		from, to       time.Time
		count          int
		representative int64
		title          string
		latest         time.Time
	}{
		{"winter", time.Date(2026, 1, 1, 0, 0, 0, 0, jst), time.Date(2026, 4, 1, 0, 0, 0, 0, jst), 2, winter2, "アニメ　作品X　第2話", time.Date(2026, 3, 26, 1, 0, 0, 0, jst)},
		{"spring", time.Date(2026, 4, 1, 0, 0, 0, 0, jst), time.Date(2026, 7, 1, 0, 0, 0, 0, jst), 1, spring, "アニメ　作品X　第3話", time.Date(2026, 4, 2, 1, 0, 0, 0, jst)},
	}
	for _, tc := range cases {
		shelves := getShelves(t, srv.URL, url.Values{
			"from": {tc.from.Format(time.RFC3339)}, "to": {tc.to.Format(time.RFC3339)},
		})
		x, ok := shelfByValue(shelves, "作品X")
		if len(shelves) != 1 || !ok {
			t.Fatalf("%s: shelves = %+v, want one 作品X shelf", tc.name, shelves)
		}
		if x.Count != tc.count || x.RepresentativeId != tc.representative || x.Title != tc.title || !x.LatestStartAt.Equal(tc.latest) {
			t.Errorf("%s: 作品X = %+v, want count %d, representative %d %q, latest %s",
				tc.name, x, tc.count, tc.representative, tc.title, tc.latest)
		}
	}
}

// site で絞っても、別 site の録画に付いた視聴済み印でその回を既読として数える。
// 絞り込みは束ねる側の母集団にだけ当て、視聴済み印の参照には当てない。
func TestListRecordingShelves_FilterKeepsWatchedMarksFromOutsideTheFilter(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Now().Truncate(time.Second)

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Site = "tokyo"
	})
	watchedElsewhere := seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Site = "osaka"
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(time.Hour), func(p *dbtest.RecordingParams) {
		p.Site = "tokyo"
	})
	if _, err := pool.Exec(context.Background(), `INSERT INTO recording_watched (recording_id) VALUES ($1)`, watchedElsewhere); err != nil {
		t.Fatalf("marking watched: %v", err)
	}

	shelves := getShelves(t, srv.URL, url.Values{"site": {"tokyo"}})
	x, ok := shelfByValue(shelves, "作品X")
	if !ok || x.Count != 2 || x.UnwatchedCount != 1 {
		t.Errorf("作品X at tokyo = %+v, want count 2 and unwatched 1 (episode 1 was watched at osaka)", x)
	}
}

// 棚の絞り込みは録画一覧と同じ意味を持つ。条件ごとに、棚の件数の合計が録画一覧の件数と一致する。
// パラメータを詰め替える経路（recordingsFilterFromShelvesParams）で 1 つ落とすとここで落ちる。
func TestListRecordingShelves_FiltersMatchListRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ruleID := seedRule(t, pool, "作品Xを録る")

	seedPlayableRecording(t, pool, "アニメ　作品X　第1話", 1, base, func(p *dbtest.RecordingParams) {
		p.Description = strPtr("宇宙")
		p.Genres = json.RawMessage(`[{"lv1":7,"lv2":0,"un1":0,"un2":0}]`)
		p.Source = "rule"
		p.RuleID = &ruleID
		p.Site = "tokyo"
	})
	seedPlayableRecording(t, pool, "アニメ　作品X　第2話", 2, base.Add(24*time.Hour), func(p *dbtest.RecordingParams) {
		p.Genres = json.RawMessage(`[{"lv1":7,"lv2":0,"un1":0,"un2":0}]`)
		p.ChannelType = "BS"
		p.NetworkID = 4
		p.ServiceID = 101
		p.Site = "osaka"
	})
	dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title, p.ProgramStartAt, p.Status, p.EventID = "ドラマ　作品Y　第1話", base.Add(48*time.Hour), "recording", 3
		p.Description = strPtr("作品Xの裏番組")
		p.Genres = json.RawMessage(`[{"lv1":3,"lv2":0,"un1":0,"un2":0}]`)
		p.Source = "unattributed"
	})

	for _, q := range []url.Values{
		{"q": {"裏番組"}},
		// 既定の titleDescription なら作品Y も当たる（3 件）ので、qTarget を落とすと食い違う。
		{"q": {"作品X"}, "qTarget": {"title"}},
		{"genre": {"7"}},
		{"channelType": {"BS"}},
		{"site": {"tokyo"}},
		{"service": {"400101"}},
		{"status": {"recording"}},
		{"source": {"unattributed"}},
		{"ruleId": {strconv.FormatInt(ruleID, 10)}},
		{"from": {base.Add(time.Hour).Format(time.RFC3339)}},
		{"to": {base.Add(time.Hour).Format(time.RFC3339)}},
	} {
		var recordings []Recording
		if resp := getJSON(t, srv.URL+"/api/recordings?"+q.Encode(), &recordings); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/recordings?%s status = %d", q.Encode(), resp.StatusCode)
		}
		total := 0
		for _, s := range getShelves(t, srv.URL, q) {
			total += s.Count
		}
		if total == 0 || total == 3 || total != len(recordings) {
			t.Errorf("%s: shelves count %d, recordings %d (want equal, and the filter must narrow 3 recordings)", q.Encode(), total, len(recordings))
		}
	}
}

// 不正な絞り込みは録画一覧と同じく 400 にする。
func TestListRecordingShelves_RejectsInvalidFilters(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)
	for _, q := range []string{"genre=16", "status=bogus", "qTarget=bogus", "channelType=XX", "source=bogus"} {
		resp := getJSON(t, srv.URL+"/api/recording-shelves?"+q, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /api/recording-shelves?%s status = %d, want 400", q, resp.StatusCode)
		}
	}
}
