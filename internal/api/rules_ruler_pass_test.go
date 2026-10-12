package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

// ルール作成/更新/削除は同一トランザクションで RulerPassArgs を投入する
// （ヒント経路。docs/recording/ruler.md §3.1「api がルール書き込みと同一
// トランザクションで EnqueueTx する」）。dual-write を避けるためのものなので、
// ここでは各操作が ruler_pass の投入を依頼することを確認する。
func TestRuleCRUD_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := newRecordingEnqueuer()
	router := NewRouter(RouterConfig{Pool: pool, Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("initial ruler_pass enqueue calls = %d, want 0", n)
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
	if n := countRulerPassEnqueues(enqueuer); n != 1 {
		t.Fatalf("ruler_pass enqueue calls after create = %d, want 1", n)
	}

	// 記録をクリアし、次の操作が行った投入要求だけを見る。
	enqueuer.Reset()

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
	if n := countRulerPassEnqueues(enqueuer); n != 1 {
		t.Fatalf("ruler_pass enqueue calls after update = %d, want 1", n)
	}
	enqueuer.Reset()

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
	if n := countRulerPassEnqueues(enqueuer); n != 1 {
		t.Fatalf("ruler_pass enqueue calls after delete = %d, want 1", n)
	}
}

// ルール書き込みが失敗（存在しない ID の削除で 404）すると、投入要求も行われない。
func TestDeleteRule_NotFound_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := newRecordingEnqueuer()
	router := NewRouter(RouterConfig{Pool: pool, Enqueuer: enqueuer})
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

	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("ruler_pass enqueue calls = %d, want 0 "+
			"(存在しないルールの削除はロールバックされ、ヒントも投入されないはず)", n)
	}
}

func TestInsertRulerPassHint_FollowsTransaction(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := newRecordingEnqueuer()
	h := &Server{enqueuer: enqueuer}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := h.insertRulerPassHint(ctx, tx, db.DefaultSite); err != nil {
		t.Fatalf("insertRulerPassHint: %v", err)
	}
	calls := enqueuer.CallsOfKind("ruler_pass")
	if len(calls) != 1 {
		t.Fatalf("ruler_pass enqueue count = %d, want 1", len(calls))
	}
	if !calls[0].Transactional || calls[0].Tx != tx {
		t.Errorf("enqueue call = %#v, want EnqueueTx with caller transaction", calls[0])
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

func countRulerPassEnqueues(enqueuer *recordingEnqueuer) int {
	return len(enqueuer.CallsOfKind("ruler_pass"))
}
