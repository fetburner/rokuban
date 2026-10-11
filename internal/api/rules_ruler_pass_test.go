package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// ルール作成/更新/削除は同一トランザクションで RulerPassArgs を投入する
// （ヒント経路。docs/recording.md §3.1「ヒントは api がルール書き込みと同一
// トランザクションで EnqueueTx する」）。dual-write を避けるためのものなので、
// ここでは実際に River のジョブが現れることを確認する。
func TestRuleCRUD_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := NewRouter(RouterConfig{Pool: pool, Enqueuer: legacyEnqueuerForTest(riverClient)})
	srv := httptest.NewServer(router)
	defer srv.Close()

	if n := countRulerPassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("initial ruler_pass job count = %d, want 0", n)
	}

	// Create
	body := map[string]any{"name": "アニメ全録", "priority": 20}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created Rule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if n := countRulerPassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("ruler_pass job count after create = %d, want 1", n)
	}

	// UniqueOpts の合流で次の書き込みが弾かれてしまうと「投入されたこと」を検証
	// できなくなるので、都度クリアしてから次の操作を見る。
	clearRulerPassJobs(t, ctx, pool)

	// Update
	updateBody := map[string]any{"name": "アニメ全録2", "priority": 30}
	raw, _ = json.Marshal(updateBody)
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/api/rules/"+itoa(created.Id), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	if n := countRulerPassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("ruler_pass job count after update = %d, want 1", n)
	}
	clearRulerPassJobs(t, ctx, pool)

	// Delete
	req, err = http.NewRequest(http.MethodDelete, srv.URL+"/api/rules/"+itoa(created.Id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if n := countRulerPassJobs(t, ctx, pool); n != 1 {
		t.Fatalf("ruler_pass job count after delete = %d, want 1", n)
	}
}

// ルール書き込みが失敗（存在しない ID の削除で 404）すると、同一トランザクション
// なので RulerPassArgs の投入も一緒にロールバックされ、ジョブは残らないこと。
func TestDeleteRule_NotFound_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := NewRouter(RouterConfig{Pool: pool, Enqueuer: legacyEnqueuerForTest(riverClient)})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/rules/999999", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete status = %d, want 404", resp.StatusCode)
	}

	if n := countRulerPassJobs(t, ctx, pool); n != 0 {
		t.Fatalf("ruler_pass job count = %d, want 0 "+
			"(存在しないルールの削除はロールバックされ、ヒントも投入されないはず)", n)
	}
}

func TestInsertRulerPassHint_FollowsTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	h := &Server{enqueuer: legacyEnqueuerForTest(riverClient)}

	rolledBack, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning rollback transaction: %v", err)
	}
	defer func() { _ = rolledBack.Rollback(ctx) }()
	if err := h.insertRulerPassHint(ctx, rolledBack, db.DefaultSite); err != nil {
		t.Fatalf("insertRulerPassHint(rollback): %v", err)
	}
	if got := countRulerPassJobs(t, ctx, pool); got != 0 {
		t.Fatalf("ruler_pass job count before rollback = %d, want 0", got)
	}
	if err := rolledBack.Rollback(ctx); err != nil {
		t.Fatalf("rolling back transaction: %v", err)
	}
	if got := countRulerPassJobs(t, ctx, pool); got != 0 {
		t.Fatalf("ruler_pass job count after rollback = %d, want 0", got)
	}

	committed, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning commit transaction: %v", err)
	}
	defer func() { _ = committed.Rollback(ctx) }()
	if err := h.insertRulerPassHint(ctx, committed, db.DefaultSite); err != nil {
		t.Fatalf("insertRulerPassHint(commit): %v", err)
	}
	if got := countRulerPassJobs(t, ctx, pool); got != 0 {
		t.Fatalf("ruler_pass job count before commit = %d, want 0", got)
	}
	if err := committed.Commit(ctx); err != nil {
		t.Fatalf("committing transaction: %v", err)
	}
	if got := countRulerPassJobs(t, ctx, pool); got != 1 {
		t.Fatalf("ruler_pass job count after commit = %d, want 1", got)
	}
}

// Enqueuer が nil でもルール CRUD 自体は動くこと（h.insertRulerPassHint の
// nil チェックの回帰。既存の TestRulesCRUD が RouterConfig{Pool: pool} だけで
// 動いていることの裏付けでもある）。
func TestCreateRule_WithoutEnqueuer_StillSucceeds(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := map[string]any{"name": "リバークライアントなし"}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
}

func countRulerPassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	return len(riveradaptertest.MustListRiverJobsOfKind(t, ctx, pool, "ruler_pass"))
}

func clearRulerPassJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	riveradaptertest.MustDeleteRiverJobsOfKind(t, ctx, pool, "ruler_pass")
}
