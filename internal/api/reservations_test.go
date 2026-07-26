package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// CreateReservation はチャンネル識別情報を EPG プロジェクションからスナップショットする
// （mirakc の programId 内部構造への依存を予約行から消すための列。issue #21 の前提でもある）。
func TestCreateReservation_SnapshotsChannelIdentity(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 400000600021234
	insertProgramFixture(t, pool, ctx, programID, 40000, 6000)

	body := `{"programId":400000600021234,"title":"チャンネルスナップショットテスト",` +
		`"startAt":"2026-08-01T21:00:00+09:00","durationMs":1800000}`
	resp, err := http.Post(srv.URL+"/api/reservations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var networkID, serviceID int
	var channelType, channel string
	if err := pool.QueryRow(ctx,
		`SELECT network_id, service_id, channel_type, channel FROM reservations WHERE site = 'default' AND program_id = $1`,
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

// 番組が EPG プロジェクションに存在しない場合、programId から算術で推測せず 400 を返す。
func TestCreateReservation_ProgramNotInProjection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 111222333444555
	body := `{"programId":111222333444555,"title":"存在しない番組",` +
		`"startAt":"2026-08-01T21:00:00+09:00","durationMs":1800000}`
	resp, err := http.Post(srv.URL+"/api/reservations", "application/json", strings.NewReader(body))
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
}
