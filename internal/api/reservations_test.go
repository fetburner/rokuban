package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// PutProgramIntent はチャンネル識別情報を EPG プロジェクションからスナップショットする
// （mirakc の programId 内部構造への依存を予約行から消すための列。issue #21 の前提でもある）。
func TestPutProgramIntent_SnapshotsChannelIdentity(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 400000600021234
	insertProgramFixture(t, pool, ctx, programID, 40000, 6000)

	body := `{"action":"record"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		srv.URL+"/api/sites/default/programs/400000600021234/intent", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	// チャンネル識別のスナップショットは #27 で program_snapshots に抽出された。
	var networkID, serviceID int
	var channelType, channel string
	if err := pool.QueryRow(ctx,
		`SELECT network_id, service_id, channel_type, channel FROM program_snapshots WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&networkID, &serviceID, &channelType, &channel); err != nil {
		t.Fatalf("querying snapshotted columns: %v", err)
	}
	if networkID != 40000 || serviceID != 6000 {
		t.Errorf("network_id/service_id = %d/%d, want 40000/6000", networkID, serviceID)
	}
	if channelType != "GR" || channel != "27" {
		t.Errorf("channel_type/channel = %q/%q, want GR/27", channelType, channel)
	}
}

// ProgramIntentInput には title / startAt / durationMs が無い（#27 の決定「値の出所を
// EPG 射影ただ 1 つに固定する」。openapi.yaml ProgramIntentInput 参照）。つまりクライアントは
// 番組の事実を送れない。ここでは「意図を書いてもスナップショットが EPG プロジェクションの
// 値で正しく埋まる」ことを確認する。
//
// これが崩れると、UI が古い番組表を握ったまま予約したときに GC の比較対象
// （program_snapshots.start_at + duration_ms）がクライアントの申告に引きずられ、
// ユーザーの skip 意図が早すぎる GC で消えることがあった（#27 が挙げる壊れ方）。
func TestPutProgramIntent_SnapshotsProgramFactsFromProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 700000800091234
	insertProgramFixture(t, pool, ctx, programID, 70000, 8000)

	// 射影に登録されている「真の」番組の事実を先に控えておく。
	var wantTitle string
	var wantStart time.Time
	var wantDuration int64
	if err := pool.QueryRow(ctx,
		`SELECT name, start_at, duration_ms FROM epg_programs WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&wantTitle, &wantStart, &wantDuration); err != nil {
		t.Fatalf("querying epg_programs fixture: %v", err)
	}

	body := `{"action":"record"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		srv.URL+"/api/sites/default/programs/700000800091234/intent", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	var gotTitle string
	var gotStart time.Time
	var gotDuration int64
	if err := pool.QueryRow(ctx,
		`SELECT title, start_at, duration_ms FROM program_snapshots WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&gotTitle, &gotStart, &gotDuration); err != nil {
		t.Fatalf("querying program_snapshots: %v", err)
	}

	if gotTitle != wantTitle {
		t.Errorf("snapshot title = %q, want %q (from EPG projection)", gotTitle, wantTitle)
	}
	if !gotStart.Equal(wantStart) {
		t.Errorf("snapshot start_at = %v, want %v (from EPG projection)", gotStart, wantStart)
	}
	if gotDuration != wantDuration {
		t.Errorf("snapshot duration_ms = %d, want %d (from EPG projection)", gotDuration, wantDuration)
	}
}

// 番組が EPG プロジェクションに存在しない場合、programId から算術で推測せず 400 を返す。
// program_intents / reservations のどちらにも行を作らない。
func TestPutProgramIntent_ProgramNotInProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 111222333444555
	body := `{"action":"record"}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		srv.URL+"/api/sites/default/programs/111222333444555/intent", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reservation rows = %d, want 0 (should not create a row on 400)", n)
	}

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM program_intents WHERE site = 'default' AND program_id = $1`,
		programID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("program_intents rows = %d, want 0 (should not create a row on 400)", n)
	}
}
