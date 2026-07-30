package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 受け入れ: 同一条件で検索 API と rulequery.MatchProgramIDs（ruler 経路）の集合が一致する。
func TestSearchPrograms_MatchesRulerPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ('default', 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	for _, row := range []struct {
		id   int64
		name string
		g    string
	}{
		{2001, "ニュースワイド", "{0}"},
		{2002, "アニメ特番", "{7}"},
		{2003, "NHKスペシャル", "{8}"},
	} {
		st := start.Add(time.Duration(row.id) * time.Minute)
		_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', $1::bigint, 32736, 1024, $2::integer, $3::timestamptz, 1800000, $4::timestamptz, true, $5::text, '', $6::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`,
			row.id, int32(row.id), st, st.Add(30*time.Minute), row.name, row.g)
		if err != nil {
			t.Fatal(err)
		}
	}

	router := NewRouter(RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := map[string]any{
		"textMatches": []map[string]any{
			{"target": "name", "mode": "keyword", "value": "ニュース"},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/sites/default/programs/search", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	var apiIDs []int64
	if err := json.NewDecoder(resp.Body).Decode(&apiIDs); err != nil {
		t.Fatal(err)
	}

	// ruler と同じ MatchProgramIDs
	rulerIDs, err := rulequery.MatchProgramIDs(ctx, pool, "default", rulequery.Conditions{
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}},
	})
	if err != nil {
		t.Fatal(err)
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
	bodyFW := map[string]any{
		"textMatches": []map[string]any{
			{"target": "name", "mode": "keyword", "value": "ＮＨＫ"},
		},
	}
	raw, _ = json.Marshal(bodyFW)
	resp, err = http.Post(srv.URL+"/api/sites/default/programs/search", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var fwAPI []int64
	if err := json.NewDecoder(resp.Body).Decode(&fwAPI); err != nil {
		t.Fatal(err)
	}
	fwRuler, err := rulequery.MatchProgramIDs(ctx, pool, "default", rulequery.Conditions{
		TextMatches: []rulequery.TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fwAPI) != 1 || fwAPI[0] != 2003 || fwAPI[0] != fwRuler[0] {
		t.Fatalf("fullwidth api=%v ruler=%v", fwAPI, fwRuler)
	}
}
