package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// 予約の作成/取消は同一トランザクションで ReconcilePassArgs を投入する
// （ヒント経路。docs/recording.md §3.2「予約の作成 / 取消」）。dual-write を
// 避けるためのものなので、ここでは実際に river_job にジョブが現れることを確認する。
func TestCreateReservation_EnqueuesReconcilePassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 500000700031234
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)

	if n := countReconcilePassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("initial reconcile_pass job count = %d, want 0", n)
	}

	body := `{"programId":500000700031234,"title":"予約ヒントテスト",` +
		`"startAt":"2026-08-01T21:00:00+09:00","durationMs":1800000}`
	resp, err := http.Post(srv.URL+"/api/reservations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created api.Reservation
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if n := countReconcilePassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("reconcile_pass job count after create = %d, want 1", n)
	}

	// UniqueOpts の合流で次の書き込みが弾かれてしまうと「投入されたこと」を検証
	// できなくなるので、クリアしてから取消側を見る。
	clearReconcilePassJobs(t, ctx, pool)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/reservations/"+itoa(created.Id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if n := countReconcilePassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("reconcile_pass job count after delete = %d, want 1", n)
	}
}

// 予約作成が失敗（番組が EPG プロジェクションに存在せず 400）すると、同一
// トランザクションなので ReconcilePassArgs の投入も一緒にロールバックされ、
// ジョブは残らないこと。
func TestCreateReservation_ProgramNotInProjection_DoesNotEnqueueReconcilePassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient})
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := `{"programId":999888777666555,"title":"存在しない番組",` +
		`"startAt":"2026-08-01T21:00:00+09:00","durationMs":1800000}`
	resp, err := http.Post(srv.URL+"/api/reservations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if n := countReconcilePassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("reconcile_pass job count = %d, want 0 "+
			"(存在しない番組の予約はロールバックされ、ヒントも投入されないはず)", n)
	}
}

func countReconcilePassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'reconcile_pass'`).Scan(&n); err != nil {
		t.Fatalf("counting reconcile_pass jobs: %v", err)
	}
	return n
}

func clearReconcilePassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM river_job WHERE kind = 'reconcile_pass'`); err != nil {
		t.Fatalf("clearing reconcile_pass jobs: %v", err)
	}
}
