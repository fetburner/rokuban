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
	if got == nil || *got != want {
		t.Fatalf("series = %v, want %q", got, want)
	}
}

// 予約の series は EPG の実効シリーズを読む。ルール削除後は自動キーへ戻り、
// EPG 行が無い予約は null のまま返す。一覧・単体取得の両 API を確認する。
func TestReservationSeries_FollowsEPGEffectiveSeries(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 1150000115041031
	const missingEPGProgramID int64 = 1150000115041032
	const title = "アニメ　作品X　第1話"

	insertReservationWithSnapshot(t, pool, ctx, programID, title, "テスト局", 11500, 1150)
	insertReservationWithSnapshot(t, pool, ctx, missingEPGProgramID, "EPG 消失番組", "テスト局", 11500, 1150)
	insertProgramFixtureForSite(t, pool, ctx, "default", programID, 11500, 1150)
	if _, err := pool.Exec(ctx, `UPDATE epg_programs SET name = $1 WHERE site = 'default' AND program_id = $2`, title, programID); err != nil {
		t.Fatalf("updating EPG title: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO label_rules (key, value, keyword, priority)
VALUES ('series', '分類シリーズ', '作品X', 10)`); err != nil {
		t.Fatalf("inserting series rule: %v", err)
	}

	client := http.DefaultClient
	listURL := srv.URL + "/api/reservations"
	list := getReservationSeriesBody(t, client, listURL)
	assertReservationSeries(t, reservationSeries(t, list, programID), "分類シリーズ")
	if got := reservationSeries(t, list, missingEPGProgramID); got != nil {
		t.Fatalf("reservation without an EPG row series = %q, want null", *got)
	}

	detailURL := fmt.Sprintf("%s/api/sites/default/programs/%d/reservation", srv.URL, programID)
	detail := getReservationSeriesBody(t, client, detailURL)
	assertReservationSeries(t, reservationSeries(t, detail, programID), "分類シリーズ")

	if _, err := pool.Exec(ctx, `DELETE FROM label_rules WHERE keyword = '作品X'`); err != nil {
		t.Fatalf("deleting series rule: %v", err)
	}
	list = getReservationSeriesBody(t, client, listURL)
	assertReservationSeries(t, reservationSeries(t, list, programID), "作品X")
	detail = getReservationSeriesBody(t, client, detailURL)
	assertReservationSeries(t, reservationSeries(t, detail, programID), "作品X")
}
