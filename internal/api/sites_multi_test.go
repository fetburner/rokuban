package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// getJSONForTest は GET して 200 なら body を out にデコードする（epg_test.go の
// getJSON と同じ形。あちらは package api、こちらは package api_test なので
// 共有できず、この 1 ファイル内だけの複製として持つ）。
func getJSONForTest(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", url, err)
		}
	}
	return resp
}

// GET /api/sites はレジストリを定義順のまま返す。
func TestListSites_ReturnsConfiguredRegistry(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Pool: pool, Sites: []string{"tokyo", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sites")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := []string{"tokyo", "osaka"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sites = %v, want %v (定義順)", got, want)
	}
}

// Sites 未設定（テストの部分構成）は db.DefaultSite の 1 要素になる
// （既存の「site が空なら db.DefaultSite」規約を集合に持ち上げただけ）。
func TestListSites_DefaultsToDefaultSiteWhenUnconfigured(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sites")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got []string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != db.DefaultSite {
		t.Errorf("sites = %v, want [%q]", got, db.DefaultSite)
	}
}

// insertProgramFixtureForSite は insertProgramFixture（intents_test.go）の
// site 引数付き版。多サイトのテストは site 別に固有の networkID/serviceID を
// 使う程度では区別できない（EPG プロジェクションの PK が (site, program_id) の
// ため）ので、ここで直接 site を指定する。
func insertProgramFixtureForSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, programID int64, networkID, serviceID int32) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`,
		site, networkID, serviceID); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}

	start := time.Now().Add(24 * time.Hour)
	end := start.Add(30 * time.Minute)
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ($1, $2::bigint, $3, $4, 0, $5::timestamptz, 1800000, $6::timestamptz, true, 'テスト番組', '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`,
		site, programID, networkID, serviceID, start, end); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}
}

// TestMultiSiteRegistry_HandlesAllRegisteredSites は 2 サイトのレジストリで
// 1 プロセスの api が両サイトの /programs・/intent・/overrides・
// /breakers/{name}/resume を処理できることを確認する（issue #184 M4-12 受け入れ）。
//
// **単一サイトの fixture では検出できない回帰を狙う。** 旧実装の `req.Site !=
// h.site`（h.site は起動時に固定された 1 文字列）は、レジストリを 2 要素にした
// 途端に「2 つ目の site が常に unknown 扱いになる」形で壊れる。レジストリが
// 1 要素の既存テストはこの壊れ方を検出できない（h.site を 1 つに固定しても
// その 1 つに対しては常に一致するため）。この述語を `req.Site != h.site` に
// 戻すとこのテストは falls over（2 つ目のサイトが軒並み unknown site になる）。
//
// **もう 1 つ別の罠も同じ fixture で狙う。** site 検査（`knownSite`）を通した
// 後、`epg.go` / `search.go` / `reservations_overlaps.go` は検査済みの
// `req.Site` を EPG プロジェクションへの問い合わせ引数に**そのまま**渡す
// 必要がある。もしここで（h.site のような）固定の 1 値を使っていたら、
// 検査自体は通るのに実際には別サイトのデータを読んでしまう ---
// ステータスコードだけを見るテストではこの壊れ方を検出できないため、
// 各エンドポイントの**レスポンス本体**を site ごとに区別できる形で確認する
// （programId を site ごとに変えてあるので、取り違えれば 404 か別サイトの
// programId が返る）。
func TestMultiSiteRegistry_HandlesAllRegisteredSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool, Sites: []string{"tokyo", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const (
		programIDTokyo int64 = 100000000000001
		programIDOsaka int64 = 200000000000002
	)
	// insertProgramFixtureForSite が焼き込む放送時刻（now + 24h、30 分番組）に
	// 実際に重なる窓で問い合わせる。2020 年のような過去窓だと常に空配列が返り、
	// 「site を取り違えて別サイトの空プロジェクションを読んだ」場合と
	// 「正しく自サイトを読んだが単に一致が無い」場合を区別できない。
	// url.QueryEscape が要る --- RFC3339 のタイムゾーンオフセットの "+" を
	// エスケープしないと、クエリ文字列中で空白として解釈され「不正な日時」
	// 400 になる（time.Now() がローカルタイム = +09:00 を返す環境で踏む）。
	windowStart := url.QueryEscape(time.Now().Add(23 * time.Hour).Format(time.RFC3339))
	windowEnd := url.QueryEscape(time.Now().Add(26 * time.Hour).Format(time.RFC3339))

	insertProgramFixtureForSite(t, pool, ctx, "tokyo", programIDTokyo, 10001, 20001)
	insertProgramFixtureForSite(t, pool, ctx, "osaka", programIDOsaka, 10002, 20002)
	// GetProgramReservation は reservations 行を読む。ruler を経由しないと
	// PutProgramIntent だけでは行が作られない（reservations の書き手は ruler
	// だけ、issue #29）ため、この確認のためだけに直接作る。
	insertReservationFixtureForSite(t, pool, ctx, "tokyo", programIDTokyo, 10001, 20001)
	insertReservationFixtureForSite(t, pool, ctx, "osaka", programIDOsaka, 10002, 20002)

	registered := []struct {
		site           string
		programID      string // pathで使う文字列
		otherProgramID string // 取り違え検出用（自サイトに存在しないはずの id）
		networkID      int    // insertProgramFixtureForSite に渡した値
		otherNetworkID int    // 他サイトの networkID（取り違え検出用）
	}{
		{"tokyo", itoa(programIDTokyo), itoa(programIDOsaka), 10001, 10002},
		{"osaka", itoa(programIDOsaka), itoa(programIDTokyo), 10002, 10001},
	}
	for _, r := range registered {
		t.Run("registered/"+r.site, func(t *testing.T) {
			// GET .../services も自サイトの service だけを返す（site を取り違えると
			// 他サイトの networkID が混ざる／自サイトの networkID が返らない）。
			var services []api.Service
			svcresp := getJSONForTest(t, srv.URL+"/api/sites/"+r.site+"/services", &services)
			if svcresp.StatusCode != http.StatusOK {
				t.Fatalf("GET .../services (site=%s) status = %d, want 200", r.site, svcresp.StatusCode)
			}
			networkIDs := make([]int, 0, len(services))
			for _, s := range services {
				networkIDs = append(networkIDs, s.NetworkId)
			}
			if !slices.Contains(networkIDs, r.networkID) {
				t.Errorf("GET .../services (site=%s) = %v, want to contain networkId %d", r.site, networkIDs, r.networkID)
			}
			if slices.Contains(networkIDs, r.otherNetworkID) {
				t.Errorf("GET .../services (site=%s) = %v, must not contain other site's networkId %d", r.site, networkIDs, r.otherNetworkID)
			}

			// GET .../programs は自サイトの programId だけを返す（他サイトの
			// programId が混ざる／返らないことの両方を見る）。
			var programs []api.ProgramListItem
			presp := getJSONForTest(t, srv.URL+"/api/sites/"+r.site+"/programs?start="+windowStart+"&end="+windowEnd, &programs)
			if presp.StatusCode != http.StatusOK {
				t.Fatalf("GET .../programs status = %d, want 200", presp.StatusCode)
			}
			ids := make([]string, 0, len(programs))
			for _, p := range programs {
				ids = append(ids, itoa(p.ProgramId))
			}
			if !slices.Contains(ids, r.programID) {
				t.Errorf("GET .../programs (site=%s) = %v, want to contain %s", r.site, ids, r.programID)
			}
			if slices.Contains(ids, r.otherProgramID) {
				t.Errorf("GET .../programs (site=%s) = %v, must not contain other site's %s", r.site, ids, r.otherProgramID)
			}

			// GET .../programs/{programId} 単体取得（site を取り違えると
			// 「別サイトのプロジェクションに存在しない」404 になる）。
			var program api.Program
			gresp := getJSONForTest(t, srv.URL+"/api/sites/"+r.site+"/programs/"+r.programID, &program)
			if gresp.StatusCode != http.StatusOK {
				t.Fatalf("GET .../programs/%s (site=%s) status = %d, want 200", r.programID, r.site, gresp.StatusCode)
			}
			if itoa(program.ProgramId) != r.programID {
				t.Errorf("GET .../programs/%s (site=%s) returned programId=%d", r.programID, r.site, program.ProgramId)
			}

			// POST .../programs/search（条件なし = 全件マッチ）も自サイトの
			// programId だけを返す。
			var searchMatches []struct {
				Site      string `json:"site"`
				ProgramId int64  `json:"programId"`
			}
			sresp, err := http.Post(srv.URL+"/api/sites/"+r.site+"/programs/search", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sresp.Body.Close() }()
			if sresp.StatusCode != http.StatusOK {
				t.Fatalf("POST .../programs/search (site=%s) status = %d, want 200", r.site, sresp.StatusCode)
			}
			if err := json.NewDecoder(sresp.Body).Decode(&searchMatches); err != nil {
				t.Fatal(err)
			}
			searchIDStrs := make([]string, 0, len(searchMatches))
			for _, m := range searchMatches {
				searchIDStrs = append(searchIDStrs, itoa(m.ProgramId))
			}
			if !slices.Contains(searchIDStrs, r.programID) {
				t.Errorf("POST .../programs/search (site=%s) = %v, want to contain %s", r.site, searchIDStrs, r.programID)
			}
			if slices.Contains(searchIDStrs, r.otherProgramID) {
				t.Errorf("POST .../programs/search (site=%s) = %v, must not contain other site's %s", r.site, searchIDStrs, r.otherProgramID)
			}

			// GET .../programs/{programId}/overlaps は EPG プロジェクションから
			// 放送時間を引く（reservations_overlaps.go）。site を取り違えると
			// 「program not found in EPG projection」404 になる。
			oresp, err := http.Get(srv.URL + "/api/sites/" + r.site + "/programs/" + r.programID + "/overlaps")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = oresp.Body.Close() }()
			if oresp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(oresp.Body)
				t.Fatalf("GET .../overlaps (site=%s) status = %d, want 200 (body=%s)", r.site, oresp.StatusCode, body)
			}

			// PUT .../intent（書き込み系。unknown なら 400）。既知の site なら
			// program fixture が通っているので 204。
			req, err := http.NewRequest(http.MethodPut,
				srv.URL+"/api/sites/"+r.site+"/programs/"+r.programID+"/intent",
				strings.NewReader(`{"action":"record"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			iresp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = iresp.Body.Close() }()
			if iresp.StatusCode != http.StatusNoContent {
				t.Errorf("PUT .../intent status = %d, want 204", iresp.StatusCode)
			}

			// PATCH .../overrides（書き込み系）
			oreq, err := http.NewRequest(http.MethodPatch,
				srv.URL+"/api/sites/"+r.site+"/programs/"+r.programID+"/overrides",
				strings.NewReader(`{"priority":5}`))
			if err != nil {
				t.Fatal(err)
			}
			oreq.Header.Set("Content-Type", "application/json")
			opresp, err := http.DefaultClient.Do(oreq)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = opresp.Body.Close() }()
			if opresp.StatusCode != http.StatusNoContent {
				t.Errorf("PATCH .../overrides status = %d, want 204", opresp.StatusCode)
			}

			// GET .../programs/{programId}/reservation は (site, programId) を
			// 宛先に引く（issue #99）。取り違えると「予約が無い」404 になる。
			var reservation api.Reservation
			rresp := getJSONForTest(t, srv.URL+"/api/sites/"+r.site+"/programs/"+r.programID+"/reservation", &reservation)
			if rresp.StatusCode != http.StatusOK {
				t.Fatalf("GET .../reservation (site=%s) status = %d, want 200", r.site, rresp.StatusCode)
			}
			if reservation.Site != r.site {
				t.Errorf("GET .../reservation (site=%s) returned reservation.Site=%q", r.site, reservation.Site)
			}

			// POST .../breakers/{name}/resume（書き込み系。既知の site だが
			// 発動していないので 404 --- unknown site の 400 とは区別できる）。
			bresp, err := http.Post(srv.URL+"/api/sites/"+r.site+"/breakers/ruler_deletes/resume", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = bresp.Body.Close() }()
			if bresp.StatusCode != http.StatusNotFound {
				t.Errorf("POST .../breakers/resume status = %d, want 404 (known site, not tripped)", bresp.StatusCode)
			}
		})
	}

	// レジストリに無い site は読み取り系 404 / 書き込み系 400（現状維持）。
	t.Run("unregistered/kyoto", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/sites/kyoto/programs?start=2020-01-01T00:00:00Z&end=2020-01-02T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET .../programs status = %d, want 404", resp.StatusCode)
		}

		req, err := http.NewRequest(http.MethodPut,
			srv.URL+"/api/sites/kyoto/programs/1/intent", strings.NewReader(`{"action":"record"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		iresp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = iresp.Body.Close() }()
		if iresp.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT .../intent status = %d, want 400", iresp.StatusCode)
		}

		bresp, err := http.Post(srv.URL+"/api/sites/kyoto/breakers/ruler_deletes/resume", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = bresp.Body.Close() }()
		if bresp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST .../breakers/resume status = %d, want 400", bresp.StatusCode)
		}
		var body api.ErrorResponse
		if err := json.NewDecoder(bresp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Error, "unknown site") {
			t.Errorf("error = %q, want mention of unknown site", body.Error)
		}
	})
}

// GET /api/recordings・/api/reservations・/api/capacity/overages は api が
// site に束縛されなくなったことで全サイトの行を返す（issue #184 M4-12）。
// 述語を「h.site 一致」に戻すと、2 サイト目のデータが一覧から消えて
// このテストが落ちる。
func TestListRecordings_ReturnsRowsFromAllSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	insertRecordingFixtureForSite(t, pool, ctx, "tokyo", 1001)
	insertRecordingFixtureForSite(t, pool, ctx, "osaka", 1002)

	resp, err := http.Get(srv.URL + "/api/recordings")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []api.Recording
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	sites := map[string]bool{}
	for _, r := range got {
		sites[r.Site] = true
	}
	if !sites["tokyo"] || !sites["osaka"] {
		t.Errorf("sites in response = %v, want both tokyo and osaka present: %+v", sites, got)
	}
}

func insertRecordingFixtureForSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, eventID int32) {
	t.Helper()
	start := time.Now().Add(-2 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO recordings (
  site, source, service_name, channel_type, channel, network_id, service_id, event_id,
  title, program_start_at, program_duration_ms, status, created_at
) VALUES ($1, 'manual', 'テスト局', 'GR', '27', 10000, 20000, $2,
  'テスト録画', $3, 1800000, 'finished', now())`,
		site, eventID, start); err != nil {
		t.Fatalf("inserting recordings fixture: %v", err)
	}
}

func TestListReservations_ReturnsRowsFromAllSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	insertReservationFixtureForSite(t, pool, ctx, "tokyo", 300000000000001, 30001, 40001)
	insertReservationFixtureForSite(t, pool, ctx, "osaka", 300000000000002, 30002, 40002)

	resp, err := http.Get(srv.URL + "/api/reservations")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []api.Reservation
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	sites := map[string]bool{}
	for _, r := range got {
		sites[r.Site] = true
	}
	if !sites["tokyo"] || !sites["osaka"] {
		t.Errorf("sites in response = %v, want both tokyo and osaka present: %+v", sites, got)
	}
}

func insertReservationFixtureForSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, programID int64, networkID, serviceID int32) {
	t.Helper()
	start := time.Now().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ($1, $2, 'テスト番組', $3, 1800000, $4, $5, 'GR', '27', $6, 'テスト局')`,
		site, programID, start, networkID, serviceID, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base)
VALUES ($1, $2, '{}'::jsonb)`, site, programID); err != nil {
		t.Fatalf("inserting reservation fixture: %v", err)
	}
}

// GET /api/capacity/overages も全サイトの超過区間を返す（issue #184 M4-12）。
// internal/capacity.Compute は site ごとに独立に判定するので、2 サイト分の
// 需要・チューナーを別々に用意し、両方の超過が結果に出ることを確認する。
func TestListCapacityOverages_ReturnsOveragesFromAllSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	start := time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)

	// tokyo: GR 1 本しかないところに別チャンネル 2 予約 → shortfall 1
	insertCapacityTunerForSite(t, pool, ctx, "tokyo", 0, []string{"GR"})
	insertCapacityReservationForSite(t, pool, ctx, "tokyo", 500001, "27", start)
	insertCapacityReservationForSite(t, pool, ctx, "tokyo", 500002, "25", start)

	// osaka も同じ形で用意する（別サイトなので tokyo のチューナー本数とは独立）
	insertCapacityTunerForSite(t, pool, ctx, "osaka", 0, []string{"GR"})
	insertCapacityReservationForSite(t, pool, ctx, "osaka", 500003, "27", start)
	insertCapacityReservationForSite(t, pool, ctx, "osaka", 500004, "25", start)

	q := "?start=" + start.Add(-time.Hour).Format(time.RFC3339Nano) + "&end=" + start.Add(2*time.Hour).Format(time.RFC3339Nano)
	resp, err := http.Get(srv.URL + "/api/capacity/overages" + q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got []api.CapacityOverage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	sites := map[string]bool{}
	for _, o := range got {
		sites[o.Site] = true
	}
	if !sites["tokyo"] || !sites["osaka"] {
		t.Errorf("overage sites = %v, want both tokyo and osaka present: %+v", sites, got)
	}
}

func insertCapacityTunerForSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, index int, types []string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO tuner_sync (site, tuner_index, name, types, is_available, is_fault)
VALUES ($1, $2, $3, $4, true, false)`, site, index, "tuner-"+site, types); err != nil {
		t.Fatalf("inserting tuner_sync row: %v", err)
	}
}

func insertCapacityReservationForSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, programID int64, channel string, startAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ($1, $2, 'テスト番組', $3, 3600000, 32678, 5168, 'GR', $4, $5, 'テスト局')`,
		site, programID, startAt, channel, int32(programID%100000)); err != nil {
		t.Fatalf("inserting program_snapshot row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base) VALUES ($1, $2, '{}')`,
		site, programID); err != nil {
		t.Fatalf("inserting reservation row: %v", err)
	}
}

// ルール作成時に対象サイトを指定しない（省略 = 全サイト）と、レジストリの
// 全サイトに ruler_pass ヒントが投入される（issue #184 M4-12「含むもの」3）。
func TestCreateRule_NoSitesSpecified_EnqueuesHintForEveryRegistrySite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient, Sites: []string{"tokyo", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/rules", "application/json", strings.NewReader(`{"name":"全サイト対象"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	sites := rulerPassJobSites(t, ctx, pool)
	want := map[string]bool{"tokyo": true, "osaka": true}
	if len(sites) != 2 || !want[sites[0]] || !want[sites[1]] {
		t.Errorf("ruler_pass job sites = %v, want exactly {tokyo, osaka}", sites)
	}
}

// ルール作成時に対象サイトを明示すると、そのサイトにだけヒントが投入される。
func TestCreateRule_SitesSpecified_EnqueuesHintOnlyForThoseSites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient, Sites: []string{"tokyo", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/rules", "application/json", strings.NewReader(`{"name":"東京限定","sites":["tokyo"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	sites := rulerPassJobSites(t, ctx, pool)
	if len(sites) != 1 || sites[0] != "tokyo" {
		t.Errorf("ruler_pass job sites = %v, want exactly [tokyo]", sites)
	}
}

// DeleteRule は rule_sites が ON DELETE CASCADE で消える前に対象サイトを
// 読んでおく必要がある。読む場所を削除の後に戻すと rule_sites が既に空になり、
// 「指定なし = 全サイト」のフォールバックが誤って発動して、削除対象ではなかった
// サイトにまで ruler_pass が飛ぶ（過剰投入）。
func TestDeleteRule_EnqueuesHintOnlyForRuleTargetSites_NotAllRegistrySites(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient, Sites: []string{"tokyo", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/rules", "application/json", strings.NewReader(`{"name":"大阪限定","sites":["osaka"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created api.Rule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	clearRulerPassJobsForTest(t, ctx, pool)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/rules/"+itoa(created.Id), nil)
	if err != nil {
		t.Fatal(err)
	}
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dresp.Body.Close() }()
	if dresp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", dresp.StatusCode)
	}

	sites := rulerPassJobSites(t, ctx, pool)
	if len(sites) != 1 || sites[0] != "osaka" {
		t.Errorf("ruler_pass job sites after delete = %v, want exactly [osaka] "+
			"(rule_sites を削除後に読むと空になり、誤って全サイト分投入される)", sites)
	}
}

func rulerPassJobSites(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT args->>'site' FROM river_job WHERE kind = 'ruler_pass' ORDER BY args->>'site'`)
	if err != nil {
		t.Fatalf("querying ruler_pass job sites: %v", err)
	}
	defer rows.Close()
	var sites []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning ruler_pass job site: %v", err)
		}
		sites = append(sites, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating ruler_pass job sites: %v", err)
	}
	return sites
}

func clearRulerPassJobsForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM river_job WHERE kind = 'ruler_pass'`); err != nil {
		t.Fatalf("clearing ruler_pass jobs: %v", err)
	}
}
