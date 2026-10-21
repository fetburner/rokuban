package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 意図・上書きの書き込みが commit した後に ruler_pass のヒントを best effort で投入する。
// ruler_pass が予約の実体化を検出すると reconcile_pass を連鎖投入する。
func TestPutProgramIntent_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	router := api.NewRouter(api.RouterConfig{Ready: pool.Ping,
		ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer})
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
	if calls := enqueuer.CallsOfKind("ruler_pass"); calls[0].Transactional {
		t.Fatalf("intent hint = %#v, want a post-commit Enqueue call", calls[0])
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

func TestPutProgramIntent_EnqueuesAfterCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 500000700031235
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)

	enqueuer := &committedRowEnqueuer{
		pool: pool, table: "program_intents", site: "default", programID: programID, wantPresent: true,
	}
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := putIntentRequest(t, srv, programID, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent status = %d, want 204", resp.StatusCode)
	}
	if enqueuer.calls != 1 || !enqueuer.visibleAtEnqueue || !enqueuer.rulerPassArgsCorrect {
		t.Fatalf("program intent enqueue state = %+v, want committed intent and ruler_pass args", enqueuer)
	}
	if enqueuer.transactional {
		t.Fatal("intent hint used EnqueueTx, want Enqueue after store commit")
	}
}

func TestPatchProgramOverrides_EnqueuesAfterCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 500000700031236
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)

	enqueuer := &committedRowEnqueuer{
		pool: pool, table: "program_overrides", site: "default", programID: programID, wantPresent: true,
	}
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := doPatch(t, srv, overridesPath(programID), `{"priority":3}`)
	if resp.StatusCode != http.StatusNoContent {
		_ = resp.Body.Close()
		t.Fatalf("patch overrides status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if enqueuer.calls != 1 || !enqueuer.visibleAtEnqueue || !enqueuer.rulerPassArgsCorrect {
		t.Fatalf("program overrides enqueue state = %+v, want committed overrides and ruler_pass args", enqueuer)
	}
	if enqueuer.transactional {
		t.Fatal("overrides hint used EnqueueTx, want Enqueue after store commit")
	}
}

func TestPatchProgramOverrides_StoreFailureDoesNotEnqueue(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := doPatch(t, srv, overridesPath(999888777666556), `{"priority":3}`)
	if resp.StatusCode != http.StatusBadRequest {
		_ = resp.Body.Close()
		t.Fatalf("patch status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("ruler_pass enqueue calls = %d, want 0 after store failure", n)
	}
}

// 番組が EPG プロジェクションにも凍結スナップショットにも無い場合は意図が失敗し、
// ruler_pass のヒントも投入しない。
func TestPutProgramIntent_ProgramNotInProjection_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	router := api.NewRouter(api.RouterConfig{Ready: pool.Ping,
		ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer})
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

func TestDeleteProgramIntent_EnqueuesAfterCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 500000700031237
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)
	reservations := pgstore.NewReservations(pool)
	if err := reservations.PutIntent(ctx, "default", programID, "record"); err != nil {
		t.Fatalf("seeding program intent: %v", err)
	}

	enqueuer := &committedRowEnqueuer{
		pool: pool, table: "program_intents", site: "default", programID: programID, wantPresent: false,
	}
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: reservations, Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := doDelete(t, srv, "/api/sites/default/programs/"+itoa(programID)+"/intent")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete intent status = %d, want 204", resp.StatusCode)
	}
	if enqueuer.calls != 1 || !enqueuer.called || enqueuer.visibleAtEnqueue || enqueuer.transactional || !enqueuer.rulerPassArgsCorrect {
		t.Fatalf("delete intent enqueue state = %+v, want committed deletion and ruler_pass args before Enqueue", enqueuer)
	}
}

func TestDeleteProgramOverrides_EnqueuesAfterCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 500000700031238
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)
	reservations := pgstore.NewReservations(pool)
	priority := 3
	if err := reservations.PatchOverrides(ctx, "default", programID, reservation.OverridesPatch{Priority: &priority}); err != nil {
		t.Fatalf("seeding program overrides: %v", err)
	}

	enqueuer := &committedRowEnqueuer{
		pool: pool, table: "program_overrides", site: "default", programID: programID, wantPresent: false,
	}
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: reservations, Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := doDelete(t, srv, overridesPath(programID))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete overrides status = %d, want 204", resp.StatusCode)
	}
	if enqueuer.calls != 1 || !enqueuer.called || enqueuer.visibleAtEnqueue || enqueuer.transactional || !enqueuer.rulerPassArgsCorrect {
		t.Fatalf("delete overrides enqueue state = %+v, want committed deletion and ruler_pass args before Enqueue", enqueuer)
	}
}

func TestPutProgramIntent_EnqueueFailureDoesNotFailCommittedWrite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	const programID int64 = 500000700031239
	insertProgramFixture(t, pool, ctx, programID, 50000, 7000)

	enqueuer := &committedRowEnqueuer{
		pool: pool, table: "program_intents", site: "default", programID: programID,
		wantPresent: true, enqueueErr: errors.New("queue unavailable"),
	}
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, ReservationStore: pgstore.NewReservations(pool), Enqueuer: enqueuer,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp := putIntentRequest(t, srv, programID, "record")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put intent after enqueue failure status = %d, want 204", resp.StatusCode)
	}
	if enqueuer.calls != 1 || !enqueuer.called || !enqueuer.visibleAtEnqueue || enqueuer.transactional || !enqueuer.rulerPassArgsCorrect {
		t.Fatalf("enqueue state = %+v, want committed write, ruler_pass args, and no EnqueueTx", enqueuer)
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

type committedRowEnqueuer struct {
	pool                 *pgxpool.Pool
	table                string
	site                 string
	programID            int64
	wantPresent          bool
	visibleAtEnqueue     bool
	calls                int
	called               bool
	rulerPassArgsCorrect bool
	transactional        bool
	enqueueErr           error
}

func (e *committedRowEnqueuer) Enqueue(ctx context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	e.called = true
	e.calls++
	rulerArgs, ok := args.(jobs.RulerPassArgs)
	e.rulerPassArgsCorrect = ok && args.Kind() == "ruler_pass" && rulerArgs.Site == e.site
	query := ""
	switch e.table {
	case "program_intents":
		query = `SELECT EXISTS (SELECT 1 FROM program_intents WHERE site = $1 AND program_id = $2)`
	case "program_overrides":
		query = `SELECT EXISTS (SELECT 1 FROM program_overrides WHERE site = $1 AND program_id = $2)`
	default:
		return jobq.Result{}, errors.New("unexpected table")
	}
	if err := e.pool.QueryRow(ctx, query, e.site, e.programID).Scan(&e.visibleAtEnqueue); err != nil {
		return jobq.Result{}, err
	}
	if e.visibleAtEnqueue != e.wantPresent || !e.rulerPassArgsCorrect {
		return jobq.Result{}, errors.New("row visibility does not match expected committed state")
	}
	return jobq.Result{}, e.enqueueErr
}

func (e *committedRowEnqueuer) EnqueueTx(context.Context, db.Tx, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	e.transactional = true
	return jobq.Result{}, errors.New("unexpected transactional enqueue")
}
