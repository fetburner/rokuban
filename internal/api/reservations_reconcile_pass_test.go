package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// 意図の書き込み（PUT/DELETE .../intent）は同一トランザクションで RulerPassArgs を
// 投入する（issue #29 の決定「意図の書き込みに ruler ヒントを足す」。
// reservations の書き手を ruler だけにしたため、api は直接 reconcile_pass を
// 投入しない —— ruler_pass が実体化を検出すると自身で reconcile_pass を連鎖投入する
// （internal/worker/ruler_pass.go）ので、ここでは ruler_pass の投入だけを確認する。
func TestPutProgramIntent_EnqueuesRulerPassHint(t *testing.T) {
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

	if n := countRulerPassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("initial ruler_pass job count = %d, want 0", n)
	}

	resp := putIntentRequest(t, srv, programID, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent status = %d, want 204", resp.StatusCode)
	}
	if n := countRulerPassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("ruler_pass job count after put intent{record} = %d, want 1", n)
	}

	// UniqueOpts の合流で次の書き込みが弾かれてしまうと「投入されたこと」を検証
	// できなくなるので、クリアしてから取消側を見る。
	clearRulerPassJobs(t, ctx, pool)

	resp2 := putIntentRequest(t, srv, programID, "skip")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent{skip} status = %d, want 204", resp2.StatusCode)
	}
	if n := countRulerPassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("ruler_pass job count after put intent{skip} = %d, want 1", n)
	}
}

// 意図の書き込みが失敗（番組が EPG プロジェクションに存在せず 400）すると、同一
// トランザクションなので RulerPassArgs の投入も一緒にロールバックされ、
// ジョブは残らないこと。
func TestPutProgramIntent_ProgramNotInProjection_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := api.NewRouter(api.RouterConfig{Pool: pool, RiverClient: riverClient})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := putIntentRequest(t, srv, 999888777666555, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if n := countRulerPassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("ruler_pass job count = %d, want 0 "+
			"(存在しない番組への意図はロールバックされ、ヒントも投入されないはず)", n)
	}
}

func putIntentRequest(t *testing.T, srv *httptest.Server, programID int64, action string) *http.Response {
	t.Helper()
	body := `{"action":"` + action + `"}`
	req, err := http.NewRequest(http.MethodPut,
		srv.URL+"/api/sites/default/programs/"+itoa(programID)+"/intent", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func countRulerPassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'ruler_pass'`).Scan(&n); err != nil {
		t.Fatalf("counting ruler_pass jobs: %v", err)
	}
	return n
}

func clearRulerPassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM river_job WHERE kind = 'ruler_pass'`); err != nil {
		t.Fatalf("clearing ruler_pass jobs: %v", err)
	}
}
