package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 受け入れ: 同一条件で検索 API と rulequery.MatchPrograms（ruler 経路が呼ぶのと同じ
// クエリ関数）の集合が一致する。site はレジストリを明示した RouterConfig から取り、
// レジストリ既定値の "default" に頼らない（含むもの 6）。
func TestSearchPrograms_MatchesRulerPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const site = "siteX"

	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	for _, row := range []struct {
		id   int64
		name string
		g    string
	}{
		{2001, "ニュースワイド", `[{"lv1":0}]`},
		{2002, "アニメ特番", `[{"lv1":7}]`},
		{2003, "NHKスペシャル", `[{"lv1":8}]`},
	} {
		st := start.Add(time.Duration(row.id) * time.Minute)
		_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genres
) VALUES ($1, $2::bigint, 32736, 1024, $3::integer, $4::timestamptz, 1800000, $5::timestamptz, true, $6::text, '', $7::jsonb)
ON CONFLICT (site, program_id) DO NOTHING`,
			site, row.id, int32(row.id), st, st.Add(30*time.Minute), row.name, row.g)
		if err != nil {
			t.Fatal(err)
		}
	}

	router := NewRouter(RouterConfig{Pool: pool, Sites: []string{site}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	apiMatches := postSearchPrograms(t, srv, map[string]any{
		"textMatches": []map[string]any{
			{"target": "name", "mode": "keyword", "value": "ニュース"},
		},
	})
	apiIDs := make([]int64, len(apiMatches))
	for i, m := range apiMatches {
		if m.Site != site {
			t.Fatalf("apiMatches[%d].Site = %q, want %q", i, m.Site, site)
		}
		apiIDs[i] = m.ProgramId
	}

	rulerMatches, err := rulequery.MatchPrograms(ctx, pool, rulequery.Conditions{
		Sites:       []string{site},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rulerIDs := make([]int64, len(rulerMatches))
	for i, m := range rulerMatches {
		rulerIDs[i] = m.ProgramID
	}

	if len(apiIDs) != len(rulerIDs) {
		t.Fatalf("api=%v ruler=%v", apiIDs, rulerIDs)
	}
	for i := range apiIDs {
		if apiIDs[i] != rulerIDs[i] {
			t.Fatalf("api=%v ruler=%v", apiIDs, rulerIDs)
		}
	}
	if len(apiIDs) != 1 || apiIDs[0] != 2001 {
		t.Fatalf("ids = %v, want [2001]", apiIDs)
	}

	// 全角検索でも同じ集合
	fwMatches := postSearchPrograms(t, srv, map[string]any{
		"textMatches": []map[string]any{
			{"target": "name", "mode": "keyword", "value": "ＮＨＫ"},
		},
	})
	fwAPI := make([]int64, len(fwMatches))
	for i, m := range fwMatches {
		if m.Site != site {
			t.Fatalf("fwMatches[%d].Site = %q, want %q", i, m.Site, site)
		}
		fwAPI[i] = m.ProgramId
	}
	fwRulerMatches, err := rulequery.MatchPrograms(ctx, pool, rulequery.Conditions{
		Sites:       []string{site},
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fwAPI) != 1 || fwAPI[0] != 2003 || len(fwRulerMatches) != 1 || fwAPI[0] != fwRulerMatches[0].ProgramID {
		t.Fatalf("fullwidth api=%v ruler=%v", fwAPI, fwRulerMatches)
	}
}

// searchMatchRow は検索 API のレスポンス 1 行（デコード用の複製）。
// このファイルは package api、sites_multi_test.go は package api_test なので
// 型を共有できず、この 1 ファイル内だけの複製として持つ。
type searchMatchRow struct {
	Site      string `json:"site"`
	ProgramId int64  `json:"programId"`
}

// searchFixtureGenre は insertSearchProgramFixture が焼き込むジャンル
// （テスト側の "genres": [9] 条件と対にして使う。両方の呼び出し元が同じ値を
// 使うので定数化する）。
const searchFixtureGenre = 9

// insertSearchProgramFixture は (site, programID) の EPG 行を 1 件挿入する。
// 同一 programID を複数 site に挿入すれば「同一放送が複数サイトでマッチする」
// 状況（docs/recording/ruler.md「サイトの扱い」の N 予約）を再現できる。
func insertSearchProgramFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, programID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genres
) VALUES ($1, $2::bigint, 32736, 1024, $3::integer, $4::timestamptz, 1800000, $5::timestamptz, true, 'テスト番組', '', $6::jsonb)
ON CONFLICT (site, program_id) DO NOTHING`,
		site, programID, int32(programID), start, start.Add(30*time.Minute), fmt.Sprintf(`[{"lv1":%d}]`, searchFixtureGenre)); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}
}

// postSearchPrograms は新しい検索 API を叩き、200 を要求して行にデコードする。
func postSearchPrograms(t *testing.T, srv *httptest.Server, body map[string]any) []searchMatchRow {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/api/programs/search"
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s status = %d, want 200 (body=%s)", path, resp.StatusCode, b)
	}
	var matches []searchMatchRow
	if err := json.NewDecoder(resp.Body).Decode(&matches); err != nil {
		t.Fatal(err)
	}
	return matches
}

// TestSearchPrograms_SitesOmittedDefaultsToAllRegisteredSites は sites を省略すると
// レジストリの全 site を対象にすることを確認する（#530 含むもの 3）。同一 programId が
// 2 サイトでマッチすれば畳まずに 2 行返る（受け入れ「[{site, programId}] を返し、
// 同一放送が 2 サイトでマッチしたら 2 行出る」）。
func TestSearchPrograms_SitesOmittedDefaultsToAllRegisteredSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 6001
	insertSearchProgramFixture(t, pool, ctx, "siteA", programID)
	insertSearchProgramFixture(t, pool, ctx, "siteB", programID)

	router := NewRouter(RouterConfig{Pool: pool, Sites: []string{"siteA", "siteB"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	matches := postSearchPrograms(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
	})
	gotSites := map[string]bool{}
	for _, m := range matches {
		if m.ProgramId != programID {
			t.Fatalf("unexpected programId in matches: %+v", matches)
		}
		gotSites[m.Site] = true
	}
	if len(matches) != 2 || !gotSites["siteA"] || !gotSites["siteB"] {
		t.Fatalf("matches = %+v, want 1 row each for siteA and siteB", matches)
	}
}

// TestSearchPrograms_SitesFilterExcludesOtherSites は sites を明示すると、その集合外の
// site の行を絞ることを確認する。旧実装はパスの {site} を常に使い body の sites を
// 実質無視していたため（#530 の罠）、この経路が正しく直っていないと壊れる。
func TestSearchPrograms_SitesFilterExcludesOtherSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 6002
	insertSearchProgramFixture(t, pool, ctx, "siteA", programID)
	insertSearchProgramFixture(t, pool, ctx, "siteB", programID)

	router := NewRouter(RouterConfig{Pool: pool, Sites: []string{"siteA", "siteB"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 検索パスは site に依存せず、sites では siteB だけを指定する。
	matches := postSearchPrograms(t, srv, map[string]any{
		"genres": []int{searchFixtureGenre},
		"sites":  []string{"siteB"},
	})
	if len(matches) != 1 || matches[0].Site != "siteB" || matches[0].ProgramId != programID {
		t.Fatalf("matches = %+v, want [{siteB %d}]", matches, programID)
	}
}

// TestSearchPrograms_UnknownSiteReturns400 は sites に未知の site 名を入れると 400 に
// なることを確認する（validateRuleSites と同じ規律。#530 含むもの 2）。
func TestSearchPrograms_UnknownSiteReturns400(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool, Sites: []string{"siteA"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	raw, err := json.Marshal(map[string]any{"sites": []string{"no-such-site"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/programs/search", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSearchPrograms_LegacySitePathIsGone は、site をパスに残す旧ルートを
// 互換目的で生やさないことを確認する（破壊的変更。issue #558）。
//
// 具体的なステータスコードには結合しない。「パスに {site} を持つ既存の
// GET /api/sites/{site}/programs/{programId} と形が重なるので chi が 405 を
// 返す」という主張は、そのルートを消せば 404 に変わり無関係な結合になる。
// ここで閉じるのは「旧パスでは検索が実行されない」ことだけ（= 200 で
// SearchMatchRow の一覧が返ってこない）。
func TestSearchPrograms_LegacySitePathIsGone(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool, Sites: []string{"siteA"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/sites/siteA/programs/search", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("legacy path status = %d, want not 200 (search must not run on the legacy path)", resp.StatusCode)
	}
}
