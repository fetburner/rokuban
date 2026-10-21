package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// ルールの commit 後に ruler_pass をヒントとして投入する。
func TestRuleCRUD_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := newRecordingEnqueuer()
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool), Enqueuer: enqueuer})
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
	if calls := enqueuer.CallsOfKind("ruler_pass"); len(calls) != 1 || calls[0].Transactional {
		t.Fatalf("create enqueue calls = %#v, want a post-commit Enqueue call", calls)
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
	if calls := enqueuer.CallsOfKind("ruler_pass"); len(calls) != 1 || calls[0].Transactional {
		t.Fatalf("update enqueue calls = %#v, want a post-commit Enqueue call", calls)
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
	if calls := enqueuer.CallsOfKind("ruler_pass"); len(calls) != 1 || calls[0].Transactional {
		t.Fatalf("delete enqueue calls = %#v, want a post-commit Enqueue call", calls)
	}
}

// ルール書き込みが失敗（存在しない ID の削除で 404）すると、投入要求も行われない。
func TestDeleteRule_NotFound_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := newRecordingEnqueuer()
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool), Enqueuer: enqueuer})
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

func TestRuleHintEnqueueFollowsStoreCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := &ruleCommitCheckingEnqueuer{recordingEnqueuer: newRecordingEnqueuer()}
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool), Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	enqueuer.check = func(ctx context.Context) error {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM rules WHERE name = 'committed before hint'`).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("visible committed rules = %d, want 1 when Enqueue ran", count)
		}
		return nil
	}
	body := map[string]any{"name": "committed before hint"}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	calls := enqueuer.CallsOfKind("ruler_pass")
	if len(calls) != 1 || calls[0].Transactional {
		t.Fatalf("ruler_pass calls = %#v, want one post-commit Enqueue", calls)
	}
}

func TestRuleHintEnqueueFailureDoesNotUndoCommit(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := &ruleCommitCheckingEnqueuer{recordingEnqueuer: newRecordingEnqueuer()}
	enqueuer.check = func(context.Context) error { return fmt.Errorf("forced enqueue failure") }
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool), Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader([]byte(`{"name":"committed despite hint failure"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 despite enqueue failure", resp.StatusCode)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rules WHERE name = 'committed despite hint failure'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("committed rules = %d, want 1 after enqueue failure", count)
	}
}

// Enqueuer が nil の部分構成でもルールを保存できる。
func TestCreateRule_WithoutEnqueuer_StillSucceeds(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool)})
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

type ruleCommitCheckingEnqueuer struct {
	*recordingEnqueuer
	check func(context.Context) error
}

func (e *ruleCommitCheckingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if e.check != nil {
		if err := e.check(ctx); err != nil {
			return jobq.Result{}, err
		}
	}
	return e.recordingEnqueuer.Enqueue(ctx, args, opts...)
}

// store が失敗した書き込み（子条件の主キー違反で 500）では、ヒントを投入しない。
func TestRuleWriteFailure_DoesNotEnqueueRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	enqueuer := newRecordingEnqueuer()
	router := NewRouter(RouterConfig{Ready: pool.Ping, Sites: []string{"dup"}, RuleStore: pgstore.NewRules(pool), ProgramSearch: pgstore.NewEPG(pool), Enqueuer: enqueuer})
	srv := httptest.NewServer(router)
	defer srv.Close()

	send := func(method, path, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if got := send(http.MethodPost, "/api/rules", `{"name":"dup create","sites":["dup","dup"]}`); got != http.StatusInternalServerError {
		t.Fatalf("create with duplicate sites status = %d, want 500", got)
	}
	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("ruler_pass enqueue calls after failed create = %d, want 0", n)
	}

	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO rules (name) VALUES ('existing') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := send(http.MethodPatch, fmt.Sprintf("/api/rules/%d", id), `{"name":"dup update","sites":["dup","dup"]}`); got != http.StatusInternalServerError {
		t.Fatalf("update with duplicate sites status = %d, want 500", got)
	}
	if n := countRulerPassEnqueues(enqueuer); n != 0 {
		t.Fatalf("ruler_pass enqueue calls after failed update = %d, want 0", n)
	}
}

// failingRegexSearch は ValidateRegex だけを差し替えた ProgramSearch。
type failingRegexSearch struct {
	store.ProgramSearch
	err error
}

func (f failingRegexSearch) ValidateRegex(context.Context, string) error { return f.err }

// ルールの正規表現検査は、不正なパターンだけを 400 にし、それ以外の失敗は 500 にする。
func TestRuleRegexValidation_ClassifiesErrors(t *testing.T) {
	pool := testutil.SetupDB(t)
	body := `{"name":"re","textMatches":[{"target":"name","mode":"regex","value":"x"}]}`
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"invalid pattern", fmt.Errorf("bad: %w", store.ErrInvalidPattern), http.StatusBadRequest},
		{"other failure", errors.New("connection reset"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter(RouterConfig{Ready: pool.Ping, RuleStore: pgstore.NewRules(pool),
				ProgramSearch: failingRegexSearch{ProgramSearch: pgstore.NewEPG(pool), err: tc.err}})
			srv := httptest.NewServer(router)
			defer srv.Close()
			resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader([]byte(body)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("create status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
