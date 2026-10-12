package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 意図の書き込み（PUT/DELETE .../intent）は同一トランザクションで RulerPassArgs の
// 投入を要求する（issue #29 の決定「意図の書き込みに ruler ヒントを足す」。
// reservations の書き手を ruler だけにしたため、api は直接 reconcile_pass を
// 投入しない —— ruler_pass が実体化を検出すると自身で reconcile_pass を連鎖投入する
// （internal/worker/ruler_pass.go）ので、ここでは ruler_pass の投入だけを確認する。
func TestPutProgramIntent_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	router := api.NewRouter(api.RouterConfig{Pool: pool, Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	const programID int64 = 500000700031234
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)

	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("initial ruler_pass enqueue calls = %d, want 0", n)
	}

	resp := putIntentRequest(t, srv, programID, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent status = %d, want 204", resp.StatusCode)
	}
	if n := countRulerPassEnqueues(enqueuer); n != 1 {
		t.Fatalf("ruler_pass enqueue calls after put intent{record} = %d, want 1", n)
	}

	enqueuer.Reset()

	resp2 := putIntentRequest(t, srv, programID, "skip")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent{skip} status = %d, want 204", resp2.StatusCode)
	}
	if n := countRulerPassEnqueues(enqueuer); n != 1 {
		t.Fatalf("ruler_pass enqueue calls after put intent{skip} = %d, want 1", n)
	}
}

// 意図の書き込みが失敗（番組が EPG プロジェクションに存在せず 400）すると、同一
// トランザクションなので RulerPassArgs の投入も一緒にロールバックされ、
// ジョブは残らないこと。
func TestPutProgramIntent_ProgramNotInProjection_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	router := api.NewRouter(api.RouterConfig{Pool: pool, Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := putIntentRequest(t, srv, 999888777666555, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("ruler_pass enqueue calls = %d, want 0 (存在しない番組への意図ではヒントを要求しない)", n)
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

func countRulerPassEnqueues(enqueuer *jobqtestutil.RecordingEnqueuer) int {
	return len(enqueuer.CallsOfKind("ruler_pass"))
}
