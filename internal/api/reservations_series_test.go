package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// reservationSeries は、一覧と単体取得で series が省略されず、null も区別できるよう
// JSON オブジェクトとして予約を読む。
func reservationSeries(t *testing.T, body json.RawMessage, programID int64) *string {
	t.Helper()

	var objects []map[string]json.RawMessage
	if len(body) == 0 || body[0] != '[' {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatalf("decoding reservation object: %v", err)
		}
		objects = []map[string]json.RawMessage{object}
	} else if err := json.Unmarshal(body, &objects); err != nil {
		t.Fatalf("decoding reservation list: %v", err)
	}

	for _, object := range objects {
		var gotProgramID int64
		if err := json.Unmarshal(object["programId"], &gotProgramID); err != nil {
			t.Fatalf("decoding programId: %v", err)
		}
		if gotProgramID != programID {
			continue
		}
		raw, ok := object["series"]
		if !ok {
			t.Fatalf("reservation %d omitted required series field", programID)
		}
		if string(raw) == "null" {
			return nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decoding reservation %d series: %v", programID, err)
		}
		return &value
	}
	t.Fatalf("reservation %d not found", programID)
	return nil
}

func reservationSeriesForSite(t *testing.T, body json.RawMessage, site string, programID int64) *string {
	t.Helper()

	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(body, &objects); err != nil {
		t.Fatalf("decoding reservation list: %v", err)
	}
	for _, object := range objects {
		var gotSite string
		if err := json.Unmarshal(object["site"], &gotSite); err != nil {
			t.Fatalf("decoding site: %v", err)
		}
		var gotProgramID int64
		if err := json.Unmarshal(object["programId"], &gotProgramID); err != nil {
			t.Fatalf("decoding programId: %v", err)
		}
		if gotSite != site || gotProgramID != programID {
			continue
		}
		raw, ok := object["series"]
		if !ok {
			t.Fatalf("reservation %s/%d omitted required series field", site, programID)
		}
		if string(raw) == "null" {
			return nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decoding reservation %s/%d series: %v", site, programID, err)
		}
		return &value
	}
	t.Fatalf("reservation %s/%d not found", site, programID)
	return nil
}

func getReservationSeriesBody(t *testing.T, client *http.Client, url string) json.RawMessage {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	var body json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	return body
}

func assertReservationSeries(t *testing.T, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("series = <nil>, want %q", want)
	}
	if *got != want {
		t.Fatalf("series = %q, want %q", *got, want)
	}
}

// 予約の series は EPG の実効シリーズを読む。ルール削除後は自動キーへ戻り、
// EPG 行が無い予約は null のまま返す。一覧・単体取得の両 API を確認する。
func TestReservationSeries_FollowsEPGEffectiveSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool, Sites: []string{"default", "osaka"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041031
	const missingEPGProgramID int64 = 1150000115041032
	const nullSeriesProgramID int64 = 1150000115041033
	const title = "アニメ　作品X　第1話"

	insertReservationWithSnapshot(t, pool, ctx, programID, title, "テスト局", 11500, 1150)
	insertReservationWithSnapshot(t, pool, ctx, missingEPGProgramID, "EPG 消失番組", "テスト局", 11500, 1150)
	insertReservationWithSnapshot(t, pool, ctx, nullSeriesProgramID, "！！！", "テスト局", 11500, 1150)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
)
VALUES ('osaka', $1, '別作品　ドラマ　第1話', now() + interval '24 hours', 1800000,
        11500, 1150, 'GR', '27', 1033, 'テスト局')`, programID); err != nil {
		t.Fatalf("inserting same-id reservation snapshot at another site: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reservations (site, program_id, base) VALUES ('osaka', $1, '{}'::jsonb)`, programID); err != nil {
		t.Fatalf("inserting same-id reservation at another site: %v", err)
	}
	insertProgramFixtureForSite(t, pool, ctx, "default", programID, 11500, 1150)
	insertProgramFixtureForSite(t, pool, ctx, "osaka", programID, 11500, 1150)
	insertProgramFixtureForSite(t, pool, ctx, "default", nullSeriesProgramID, 11500, 1150)
	if _, err := pool.Exec(ctx, `UPDATE epg_programs SET name = $1 WHERE site = 'default' AND program_id = $2`, title, programID); err != nil {
		t.Fatalf("updating EPG title: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE epg_programs SET name = $1 WHERE site = 'default' AND program_id = $2`, "！！！", nullSeriesProgramID); err != nil {
		t.Fatalf("updating null-series EPG title: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE epg_programs SET name = $1 WHERE site = 'osaka' AND program_id = $2`, "別作品　ドラマ　第1話", programID); err != nil {
		t.Fatalf("updating same-id EPG title at another site: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO label_rules (key, value, keyword, priority)
VALUES ('series', '分類シリーズ', '作品X', 10)`); err != nil {
		t.Fatalf("inserting series rule: %v", err)
	}

	client := http.DefaultClient
	listURL := srv.URL + "/api/reservations"
	list := getReservationSeriesBody(t, client, listURL)
	assertReservationSeries(t, reservationSeriesForSite(t, list, "default", programID), "分類シリーズ")
	assertReservationSeries(t, reservationSeriesForSite(t, list, "osaka", programID), "別作品")
	if got := reservationSeries(t, list, missingEPGProgramID); got != nil {
		t.Fatalf("reservation without an EPG row series = %q, want null", *got)
	}
	if got := reservationSeries(t, list, nullSeriesProgramID); got != nil {
		t.Fatalf("reservation with an EPG row but no derivable series = %q, want null", *got)
	}

	detailURL := fmt.Sprintf("%s/api/sites/default/programs/%d/reservation", srv.URL, programID)
	detail := getReservationSeriesBody(t, client, detailURL)
	assertReservationSeries(t, reservationSeries(t, detail, programID), "分類シリーズ")
	osakaDetailURL := fmt.Sprintf("%s/api/sites/osaka/programs/%d/reservation", srv.URL, programID)
	osakaDetail := getReservationSeriesBody(t, client, osakaDetailURL)
	assertReservationSeries(t, reservationSeries(t, osakaDetail, programID), "別作品")

	if _, err := pool.Exec(ctx, `DELETE FROM label_rules WHERE keyword = '作品X'`); err != nil {
		t.Fatalf("deleting series rule: %v", err)
	}
	list = getReservationSeriesBody(t, client, listURL)
	assertReservationSeries(t, reservationSeriesForSite(t, list, "default", programID), "作品X")
	detail = getReservationSeriesBody(t, client, detailURL)
	assertReservationSeries(t, reservationSeries(t, detail, programID), "作品X")
	osakaDetail = getReservationSeriesBody(t, client, osakaDetailURL)
	assertReservationSeries(t, reservationSeries(t, osakaDetail, programID), "別作品")
}
