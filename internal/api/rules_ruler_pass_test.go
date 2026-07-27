package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// ルール作成/更新/削除は同一トランザクションで RulerPassArgs を投入する
// （ヒント経路。docs/recording.md §3.1「ヒントは api がルール書き込みと同一
// トランザクションで InsertTx する」）。dual-write を避けるためのものなので、
// ここでは実際に river_job にジョブが現れることを確認する。
func TestRuleCRUD_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}

	router := NewRouter(RouterConfig{Pool: pool, RiverClient: riverClient})
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

	router := NewRouter(RouterConfig{Pool: pool, RiverClient: riverClient})
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

// RiverClient が nil でもルール CRUD 自体は動くこと（h.insertRulerPassHint の
// nil チェックの回帰。既存の TestRulesCRUD が RouterConfig{Pool: pool} だけで
// 動いていることの裏付けでもある）。
func TestCreateRule_WithoutRiverClient_StillSucceeds(t *testing.T) {
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
