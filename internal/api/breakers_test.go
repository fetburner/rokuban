package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/testutil"
)

// insertCircuitBreakerFixture は circuit_breakers 行を直接 INSERT する（ruler /
// reconciler の発動ロジックを経由しない）。行の存在そのものが「発動中」を表す
// テーブルなので、発動状態はこの直接 INSERT だけで再現できる
// （internal/db/migrations/00011_circuit_breakers.sql のコメント参照）。
func insertCircuitBreakerFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string, pending, threshold int, detail string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (site, name, pending, threshold, detail)
VALUES ('default', $1, $2, $3, $4::jsonb)`, name, pending, threshold, detail); err != nil {
		t.Fatalf("inserting circuit breaker fixture %q: %v", name, err)
	}
}

// countCircuitBreakers は circuit_breakers の行数を返す（resume が対象外の行を
// 巻き込んでいないことの確認に使う）。
func countCircuitBreakers(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM circuit_breakers`).Scan(&n); err != nil {
		t.Fatalf("counting circuit breakers: %v", err)
	}
	return n
}

func existsCircuitBreaker(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM circuit_breakers WHERE site = 'default' AND name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("checking circuit breaker %q: %v", name, err)
	}
	return n > 0
}

// circuitBreakerResp は GET /api/breakers のレスポンス要素から確認に要る部分だけを持つ。
type circuitBreakerResp struct {
	Site      string `json:"site"`
	Name      string `json:"name"`
	Pending   int    `json:"pending"`
	Threshold int    `json:"threshold"`
	Detail    struct {
		Total    int `json:"total"`
		Programs []struct {
			ProgramID int64  `json:"programId"`
			Title     string `json:"title"`
		} `json:"programs"`
	} `json:"detail"`
}

// 1. 発動しているブレーカーが一覧に現れ、detail の中身（total と programs）が読める。
func TestListCircuitBreakers_ReturnsTrippedBreakerWithDetail(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	detail := `{"total":42,"programs":[{"programId":1234,"title":"テスト番組"}]}`
	insertCircuitBreakerFixture(t, pool, ctx, "ruler_deletes", 42, 50, detail)

	resp, err := http.Get(srv.URL + "/api/breakers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got []circuitBreakerResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	b := got[0]
	if b.Site != "default" || b.Name != "ruler_deletes" {
		t.Errorf("site/name = %q/%q, want default/ruler_deletes", b.Site, b.Name)
	}
	if b.Pending != 42 || b.Threshold != 50 {
		t.Errorf("pending/threshold = %d/%d, want 42/50", b.Pending, b.Threshold)
	}
	if b.Detail.Total != 42 {
		t.Errorf("detail.total = %d, want 42", b.Detail.Total)
	}
	if len(b.Detail.Programs) != 1 || b.Detail.Programs[0].ProgramID != 1234 || b.Detail.Programs[0].Title != "テスト番組" {
		t.Errorf("detail.programs = %+v, want [{1234 テスト番組}]", b.Detail.Programs)
	}
}

// 2. 何も発動していなければ空配列（null ではない）が返る。
func TestListCircuitBreakers_EmptyWhenNoneTripped(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/breakers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	if body != "[]\n" && body != "[]" {
		t.Errorf("body = %q, want literal empty array (not null)", body)
	}

	var got []circuitBreakerResp
	if err := json.Unmarshal(bodyBytes, &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// 3. resume で行が消え 204。
func TestResumeCircuitBreaker_DeletesRowAnd204(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	insertCircuitBreakerFixture(t, pool, ctx, "ruler_deletes", 10, 50, `{"total":10}`)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/default/breakers/ruler_deletes/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if existsCircuitBreaker(t, pool, ctx, "ruler_deletes") {
		t.Error("circuit breaker row still exists after resume")
	}
}

// 4. 発動していないブレーカーへの resume は 404。
func TestResumeCircuitBreaker_404WhenNotTripped(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/default/breakers/ruler_deletes/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// 5. 未知の name への resume は 400。
func TestResumeCircuitBreaker_400WhenUnknownName(t *testing.T) {
	pool := testutil.SetupDB(t)

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/default/breakers/not_a_real_breaker/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// 6. 複数発動しているとき、resume は指定した 1 つだけを消す（他を巻き込まない）。
func TestResumeCircuitBreaker_OnlyDeletesTargetedBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	insertCircuitBreakerFixture(t, pool, ctx, "ruler_deletes", 10, 50, `{"total":10}`)
	insertCircuitBreakerFixture(t, pool, ctx, "reconcile_total_loss", 1, 1, `{"total":1}`)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/default/breakers/ruler_deletes/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	if existsCircuitBreaker(t, pool, ctx, "ruler_deletes") {
		t.Error("targeted circuit breaker still exists after resume")
	}
	if !existsCircuitBreaker(t, pool, ctx, "reconcile_total_loss") {
		t.Error("untargeted circuit breaker was deleted (should not be affected)")
	}
	if n := countCircuitBreakers(t, pool, ctx); n != 1 {
		t.Errorf("remaining circuit breaker rows = %d, want 1", n)
	}
}

// 7. site をパスに含めない場合の資源不一致（issue #102）の回帰確認。
//
// circuit_breakers の PK は (site, name) だが、GET /api/breakers はサイト
// 横断で一覧を返す。resume が h.site 固定だと、一覧に見えている他サイトの
// 発動を確認済みの運用者が resume を叩いても届かず、見えているものを
// 再開できなかった。site をパスに通した今は、他サイトを指定すると
// （このプロセスは 1 site しか持たないため）400 になる —— 少なくとも
// 「見えているのに再開できないのに 404 で『発動していない』と誤読させる」
// ことはなくなり、意図が明確な 400 になる。
func TestResumeCircuitBreaker_400WhenSiteMismatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 他サイト（このプロセスの site ではない）の発動を用意する。
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (site, name, pending, threshold, detail)
VALUES ('other-site', 'ruler_deletes', 10, 50, '{"total":10}'::jsonb)`); err != nil {
		t.Fatalf("inserting circuit breaker fixture for other site: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/other-site/breakers/ruler_deletes/resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	// 他サイトの行は消えていない（誤って自サイトの条件で別サイトの行を
	// 触っていないことの確認）。
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM circuit_breakers WHERE site = 'other-site' AND name = 'ruler_deletes'`).Scan(&n); err != nil {
		t.Fatalf("checking other-site circuit breaker: %v", err)
	}
	if n != 1 {
		t.Errorf("other-site circuit breaker row count = %d, want 1 (should be untouched)", n)
	}
}

// 8. internal/breaker.All が定義する全ブレーカー名について、「発動 → GET
// /api/breakers に出る → resume で消える」の一往復が通ることを確認する
// (issue #199)。
//
// これは internal/breaker.All（値の権威）と internal/api の
// knownCircuitBreakerNames（resume が検証に使う既知集合）がずれていないかの
// 回帰テストでもある。knownCircuitBreakerNames は breaker.All から導出する
// 実装になっているので現状ずれようがないが、将来 breaker.go に新しい識別子を
// 足して breaker.All への追加を忘れると（Go の定数はリフレクションで列挙
// できないため、コンパイラも静的解析もそれを検出できない）、その識別子は
// このテストの対象に入らないまま「発動はするが resume できない」という
// issue #199 と同じ形の穴になり得る。このテストのループを breaker.All から
// 生成しているのはそのための保険 —— 新しい識別子を breaker.All に追加した
// 瞬間にこのテストの対象にも自動的に入る。
func TestCircuitBreaker_TripListResumeRoundTripForEveryKnownName(t *testing.T) {
	for _, name := range breaker.All {
		t.Run(name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()

			router := api.NewRouter(api.RouterConfig{Pool: pool})
			srv := httptest.NewServer(router)
			defer srv.Close()

			insertCircuitBreakerFixture(t, pool, ctx, name, 7, 10, `{"total":7}`)

			// 発動中として一覧に出る。
			resp, err := http.Get(srv.URL + "/api/breakers")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /api/breakers status = %d, want 200", resp.StatusCode)
			}
			var got []circuitBreakerResp
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			found := false
			for _, b := range got {
				if b.Name == name {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("breaker %q not present in GET /api/breakers response %+v", name, got)
			}

			// resume で消える（400 で拒否されない）。
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/sites/default/breakers/"+name+"/resume", nil)
			if err != nil {
				t.Fatal(err)
			}
			resumeResp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resumeResp.Body.Close() }()
			if resumeResp.StatusCode != http.StatusNoContent {
				body, _ := io.ReadAll(resumeResp.Body)
				t.Fatalf("resume status = %d, want 204 (body: %s)", resumeResp.StatusCode, body)
			}
			if existsCircuitBreaker(t, pool, ctx, name) {
				t.Errorf("circuit breaker %q still exists after resume", name)
			}
		})
	}
}
