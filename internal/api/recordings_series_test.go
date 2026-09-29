package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedEpgProgramAt は site を指定して EPG 番組を 1 行作る（N 拠点の同じ放送を
// 作るため。epg_test.go の seedEpgProgram は DefaultSite 固定）。
func seedEpgProgramAt(t *testing.T, pool *pgxpool.Pool, site string, programID int64, name string, start time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at,
                          is_free, name, description)
VALUES ($1, $2, 32678, 5168, 1, $3, 3600000, $4, true, $5, '')`,
		site, programID, start, start.Add(time.Hour), name); err != nil {
		t.Fatalf("seeding epg program %q at %s: %v", name, site, err)
	}
}

// createSeriesLabelRule は分類ルールを API 経由で 1 本作る。
func createSeriesLabelRule(t *testing.T, baseURL string, value, keyword string) {
	t.Helper()
	resp, _ := postLabelRule(t, baseURL, map[string]any{"value": value, "keyword": keyword})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating label rule %q/%q: status %d", value, keyword, resp.StatusCode)
	}
}

// listRecordingsForSeries は `GET /api/recordings?seriesOf=` を叩いて id を返す。
func listRecordingsForSeries(t *testing.T, baseURL string, query string) []int64 {
	t.Helper()
	var got []Recording
	resp := getJSON(t, baseURL+"/api/recordings?"+query, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, query)
	}
	ids := make([]int64, len(got))
	for i, rec := range got {
		ids[i] = rec.Id
	}
	return ids
}

// ハブ（`?seriesOf=`）は起点の実効シリーズと同じ録画だけを返す。同じ放送が
// 2 拠点から録れていれば 2 行のまま並ぶ（行の同一性を変えない）。
func TestListRecordings_SeriesOfReturnsTheSameSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	// 同じ棚（自動キー 作品X）の 3 件。
	first := seedRecording(t, pool, "アニメ　作品X　第1話", base.Add(-2*time.Hour), "finished", 1)
	second := seedRecording(t, pool, "アニメ　作品X　第2話", base.Add(-time.Hour), "finished", 2)
	// **failed も履歴として出る。**
	failed := seedRecording(t, pool, "アニメ　作品X　第3話", base, "failed", 3)
	// 別の棚。
	seedRecording(t, pool, "アニメ　作品Y　第1話", base.Add(time.Hour), "finished", 4)

	got := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", first))
	want := map[int64]bool{first: true, second: true, failed: true}
	if len(got) != len(want) {
		t.Fatalf("seriesOf=%d returned %v, want the 3 recordings of 作品X", first, got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("seriesOf=%d returned %d, which is not in the series", first, id)
		}
	}

	// 起点が誰でも同じ集合になる（起点は行ではなくシリーズの同定に使う）。
	fromSecond := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", second))
	gotSet := map[int64]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	same := len(fromSecond) == len(got)
	for _, id := range fromSecond {
		same = same && gotSet[id]
	}
	if !same {
		t.Errorf("seriesOf=%d returned %v, want the same series as seriesOf=%d (%v)", second, fromSecond, first, got)
	}
}

// superseded は「本物の record に枠を譲った擬似 failed 行」なので、ハブの一覧から
// 外す。無条件一覧が履歴として両方残すのとは別の判断である（ハブは「同じシリーズの
// 録画」の一覧で、枠を譲った行はその回の録画ではない）。
func TestListRecordings_SeriesOfExcludesSuperseded(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	first := seedRecording(t, pool, "アニメ　作品X　第1話", base, "finished", 1)
	superseded := seedRecording(t, pool, "アニメ　作品X　第2話", base.Add(time.Hour), "failed", 2)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET superseded_at = now() WHERE id = $1", superseded); err != nil {
		t.Fatalf("superseding: %v", err)
	}

	got := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", first))
	if len(got) != 1 || got[0] != first {
		t.Errorf("seriesOf=%d returned %v, want only %d (the superseded row must be excluded)", first, got, first)
	}

	// **superseded を外すのは `seriesOf` を付けたときだけ。** 無条件一覧は
	// 従来どおり履歴として両方を返す（この絞り込みが一覧の既定を変えていないこと）。
	var all []Recording
	resp := getJSON(t, srv.URL+"/api/recordings", &all)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(all) != 2 {
		t.Errorf("the unfiltered list returned %d rows, want 2 (seriesOf must not change the default)", len(all))
	}
}

// purge 済みの tombstone を起点にしても、`recording_series` は `purged_at` で
// 絞らないので、そのシリーズの生きている行を返す（openapi.yaml の `seriesOf`）。
func TestListRecordings_SeriesOfFromPurgedTombstoneReturnsTheSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	origin := seedRecording(t, pool, "アニメ　作品X　第1話", base, "finished", 1)
	alive := seedRecording(t, pool, "アニメ　作品X　第2話", base.Add(time.Hour), "finished", 2)
	if _, err := pool.Exec(context.Background(),
		"UPDATE recordings SET deleted_at = now(), purged_at = now() WHERE id = $1", origin); err != nil {
		t.Fatalf("purging: %v", err)
	}

	got := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", origin))
	if len(got) != 1 || got[0] != alive {
		t.Errorf("seriesOf=%d (purged) returned %v, want only %d", origin, got, alive)
	}
}

// 起点の実効シリーズが NULL（自動キーを導出できず、どのルールも当たらない録画）なら
// 0 件。**NULL 同士を等しいと見なしてはならない** --- 見なすと「棚の無い録画を
// 全部集めたハブ」が返る。存在しない id も同じく 0 件。
func TestListRecordings_SeriesOfWithNoSeriesIsEmpty(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	nullSeries := seedRecording(t, pool, "【特集】", base, "finished", 1)
	seedRecording(t, pool, "【再】", base.Add(time.Hour), "finished", 2)
	seedRecording(t, pool, "アニメ　作品X　第1話", base.Add(2*time.Hour), "finished", 3)

	got := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", nullSeries))
	if len(got) != 0 {
		t.Errorf("seriesOf from a recording with no series returned %v, want none", got)
	}

	got = listRecordingsForSeries(t, srv.URL, "seriesOf=999999")
	if len(got) != 0 {
		t.Errorf("seriesOf from an unknown id returned %v, want none", got)
	}
}

// 分類ルールで棚が割れているときは、ルールの値でハブが引ける。自動キーは
// 2 つを同じ棚（NHK高校講座）に併合するが、キーワードが片方にしか当たらない
// ルールがそれを割る（docs/data/series.md §8「2 層: 分類ルール → 自動キー」）。
func TestListRecordings_SeriesOfUsesTheLabelRuleValue(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	// **ルールを先に作る。** 当たりは録画の INSERT トリガーが付けるので、
	// 後から足したルールは全件再評価（worker のジョブ）を待つ。
	createSeriesLabelRule(t, srv.URL, "数学I", "数学")

	base := time.Now().Truncate(time.Second)
	math := seedRecording(t, pool, "NHK高校講座　数学I　第1回", base, "finished", 1)
	// 自動キーは「NHK高校講座」で同じだが、ルールが数学Iを割る。
	seedRecording(t, pool, "NHK高校講座　化学　第1回", base.Add(time.Hour), "finished", 2)

	got := listRecordingsForSeries(t, srv.URL, fmt.Sprintf("seriesOf=%d", math))
	if len(got) != 1 || got[0] != math {
		t.Errorf("seriesOf=%d returned %v, want only the recording the rule pulls into 数学I", math, got)
	}
}

// キーセットページングは `seriesOf` を付けても動く（カーソル軸は変わらない）。
func TestListRecordings_SeriesOfKeepsKeysetPaging(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	first := seedRecording(t, pool, "アニメ　作品X　第1話", base.Add(-2*time.Hour), "finished", 1)
	second := seedRecording(t, pool, "アニメ　作品X　第2話", base.Add(-time.Hour), "finished", 2)
	third := seedRecording(t, pool, "アニメ　作品X　第3話", base, "finished", 3)

	var page []Recording
	resp := getJSON(t, srv.URL+fmt.Sprintf("/api/recordings?seriesOf=%d&limit=2", first), &page)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(page) != 2 || page[0].Id != third || page[1].Id != second {
		t.Fatalf("first page = %v, want [%d %d] (descending)", recordingIDs(page), third, second)
	}

	last := page[len(page)-1]
	next := fmt.Sprintf("/api/recordings?seriesOf=%d&limit=2&before=%s&beforeId=%d",
		first, url.QueryEscape(last.StartAt.UTC().Format(time.RFC3339Nano)), last.Id)
	var secondPage []Recording
	resp = getJSON(t, srv.URL+next, &secondPage)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, next)
	}
	if len(secondPage) != 1 || secondPage[0].Id != first {
		t.Errorf("second page = %v, want [%d]", recordingIDs(secondPage), first)
	}
}

// recordingIDs は比較用に id だけを取り出す。
func recordingIDs(recs []Recording) []int64 {
	ids := make([]int64, len(recs))
	for i, rec := range recs {
		ids[i] = rec.Id
	}
	return ids
}

// ハブの「次回」は、起点の実効シリーズと同じ実効シリーズで、まだ始まっていない
// EPG の番組を返す。site は畳まない（同じ放送が 2 拠点にあれば 2 行）。
func TestListRecordingUpcoming_ReturnsFutureProgramsOfTheSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	origin := seedRecording(t, pool, "アニメ　作品X　第1話", base.Add(-24*time.Hour), "finished", 1)

	seedEpgProgramAt(t, pool, db.DefaultSite, 1, "アニメ　作品X　第2話", base.Add(24*time.Hour))
	seedEpgProgramAt(t, pool, "other", 2, "アニメ　作品X　第2話", base.Add(24*time.Hour))
	// 過去の回・別のシリーズ・自動キーの無い番組は出ない。
	seedEpgProgramAt(t, pool, db.DefaultSite, 3, "アニメ　作品X　第1話", base.Add(-time.Hour))
	seedEpgProgramAt(t, pool, db.DefaultSite, 4, "アニメ　作品Y　第1話", base.Add(24*time.Hour))
	seedEpgProgramAt(t, pool, db.DefaultSite, 5, "【特集】", base.Add(24*time.Hour))

	var got []ProgramSearchMatch
	resp := getJSON(t, srv.URL+fmt.Sprintf("/api/recordings/%d/upcoming", origin), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 2 {
		t.Fatalf("upcoming = %+v, want the 2 rows of the same broadcast on 2 sites", got)
	}
	sites := map[string]bool{got[0].Site: true, got[1].Site: true}
	if !sites[db.DefaultSite] || !sites["other"] {
		t.Errorf("upcoming sites = %v, want both %s and other (sites must not be folded)", sites, db.DefaultSite)
	}
	for _, m := range got {
		if m.Name != "アニメ　作品X　第2話" || m.ProgramId != 1 && m.ProgramId != 2 {
			t.Errorf("upcoming row = %+v, want 第2話", m)
		}
		if !m.StartAt.After(base) {
			t.Errorf("upcoming row starts at %s, want a future program", m.StartAt)
		}
	}
}

// 起点の実効シリーズが NULL、または行が無ければ空配列（null ではなく `[]`）。
func TestListRecordingUpcoming_EmptySeriesIsAnEmptyArray(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	base := time.Now().Truncate(time.Second)
	origin := seedRecording(t, pool, "【特集】", base, "finished", 1)
	seedEpgProgramAt(t, pool, db.DefaultSite, 1, "【特集】", base.Add(time.Hour))

	for _, id := range []int64{origin, 999999} {
		var raw json.RawMessage
		resp := getJSON(t, srv.URL+fmt.Sprintf("/api/recordings/%d/upcoming", id), &raw)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if string(raw) != "[]" {
			t.Errorf("upcoming for %d = %s, want []", id, raw)
		}
	}
}

// 分類ルールで棚が割れているときは、EPG 側も同じ値で引ける（録画と EPG の
// 実効シリーズが同じ空間であることの検査。ここがずれると「次回」が常に空になる）。
func TestListRecordingUpcoming_UsesTheLabelRuleValue(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := newAPIServer(t, pool)

	createSeriesLabelRule(t, srv.URL, "数学I", "数学")
	base := time.Now().Truncate(time.Second)
	origin := seedRecording(t, pool, "NHK高校講座　数学I　第1回", base.Add(-24*time.Hour), "finished", 1)

	// 自動キーは「NHK高校講座」で起点と一致しない（ルールが割った先の値 数学I が一致する）。
	seedEpgProgramAt(t, pool, db.DefaultSite, 1, "NHK高校講座　数学I　第2回", base.Add(24*time.Hour))
	seedEpgProgramAt(t, pool, db.DefaultSite, 2, "NHK高校講座　化学　第2回", base.Add(24*time.Hour))

	var got []ProgramSearchMatch
	resp := getJSON(t, srv.URL+fmt.Sprintf("/api/recordings/%d/upcoming", origin), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(got) != 1 || got[0].ProgramId != 1 {
		t.Errorf("upcoming = %+v, want the 数学I program only", got)
	}
}
