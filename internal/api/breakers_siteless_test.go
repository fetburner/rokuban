package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestResumeCircuitBreaker_DefaultSiteRegistryHole は issue #450 の再現テスト
// （sub issue 本文「含むもの」1）。`config.mirakcs` に `default` という名前の
// site が無いレジストリ（[tokyo, takamatsu]）でも、置き場の範囲の
// delete_reconcile は site の範囲の resume からは解除できない ---
//   - `default` はレジストリに無いので 400（unknown site）
//   - 登録済みの `tokyo` を渡しても、delete_reconcile は site を持たない名前
//     なので 400（分類が「知っている site だが行が無い」404 より先に立つ）
//
// 一方、site を持たない新ルート `POST /api/breakers/{name}/resume` なら
// site の指定なしに解除できる（204）。
//
// 実装前（分類も新ルートも無い状態）でこのテストを実行すると:
//   - 2 番目のアサーション（site=tokyo）は分類が無いので DELETE が実行され、
//     対象行が無く 404 になる（期待する 400 と食い違う）
//   - 3 番目のアサーション（新ルート）はルート自体が存在しないので 404 になる
//     （期待する 204 と食い違う）
//
// という形で red になる。
func TestResumeCircuitBreaker_DefaultSiteRegistryHole(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := newBreakerRouterWithSites(pool, []string{"tokyo", "takamatsu"})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// worker が書く形（置き場名を scope にする）で発動中の delete_reconcile を用意する。
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (scope, name, pending, threshold, detail)
VALUES ('test-store', 'delete_reconcile', 150, 100, '{"total":150}'::jsonb)`); err != nil {
		t.Fatalf("inserting circuit breaker fixture: %v", err)
	}

	// GET /api/breakers には見える（issue 本文の「一覧には現れる」の確認）。
	listResp, err := http.Get(srv.URL + "/api/breakers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listResp.Body.Close() }()
	var listed []struct {
		Site string `json:"site"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("decoding GET /api/breakers response: %v", err)
	}
	found := false
	for _, b := range listed {
		if b.Name == "delete_reconcile" {
			found = true
			if b.Site != "" {
				t.Errorf("delete_reconcile API site = %q, want empty (the store scope is internal)", b.Site)
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/breakers does not contain delete_reconcile: %+v", listed)
	}

	// (1) レジストリに "default" という site は無いので 400。ResumeCircuitBreaker
	// は置き場の範囲の分類を knownSite より先に検査するので、site が
	// レジストリに無くても（運用者が最も打ちがちな旧コマンドがこの形）
	// 「unknown site」ではなく新ルートへの案内が返る（レビュー指摘: この順序を
	// 主張するアサーション）。
	resp1, err := http.Post(srv.URL+"/api/sites/default/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp1.Body.Close() }()
	body1, _ := io.ReadAll(resp1.Body)
	if resp1.StatusCode != http.StatusBadRequest {
		t.Errorf("POST .../sites/default/breakers/delete_reconcile/resume status = %d, want 400 (body=%s)", resp1.StatusCode, body1)
	}
	if !strings.Contains(string(body1), "/api/breakers/delete_reconcile/resume") {
		t.Errorf("body = %s, want guidance toward POST /api/breakers/delete_reconcile/resume "+
			"(scope classification must precede knownSite, or an unregistered-site typo of the old "+
			"command gives only \"unknown site\" with no pointer to the new route)", body1)
	}

	// (2) 登録済みの site (tokyo) を渡しても、delete_reconcile は置き場
	// scope の名前なので 400 になる（「知っている site だが行が無い」404 では
	// なく、そもそもこのエンドポイントの対象ではないという 400）。
	resp2, err := http.Post(srv.URL+"/api/sites/tokyo/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp2.Body)
		t.Errorf("POST .../sites/tokyo/breakers/delete_reconcile/resume status = %d, want 400 (body=%s)", resp2.StatusCode, body)
	}

	// 上記 2 つのどちらも行を消してはいけない。
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM circuit_breakers WHERE name = 'delete_reconcile'`).Scan(&remaining); err != nil {
		t.Fatalf("counting circuit_breakers: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("circuit_breakers rows for delete_reconcile = %d, want 1 (site の範囲の resume が誤って消してはいけない)", remaining)
	}

	// (3) site 無指定ルートは config の置き場の範囲で解除する。
	resp3, err := http.Post(srv.URL+"/api/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("POST /api/breakers/delete_reconcile/resume status = %d, want 204 (body=%s)", resp3.StatusCode, body)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM circuit_breakers WHERE name = 'delete_reconcile'`).Scan(&remaining); err != nil {
		t.Fatalf("counting circuit_breakers after resume: %v", err)
	}
	if remaining != 0 {
		t.Errorf("circuit_breakers rows for delete_reconcile after resume = %d, want 0", remaining)
	}
}

// TestResumeSitelessCircuitBreaker_400WhenNameHasSite は新ルートに site を持つ
// 名前（ruler_deletes）を渡すと 400 になることを確認する（受け入れ 2）。
//
// 変異確認: internal/api の ResumeSitelessCircuitBreaker から
// ScopeKindFor の検査を外すと、このリクエストはそのまま scope が空文字列の
// 空文字列で name が `ruler_deletes` の行を DELETE しようとし、対象行が
// 無いので 404 になる（400 を期待するこのテストが落ちる）。
func TestResumeSitelessCircuitBreaker_400WhenNameHasSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := newBreakerRouter(pool)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/ruler_deletes/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
}

// TestResumeSitelessCircuitBreaker_400WhenUnknownName は新ルートに未知の名前を
// 渡すと 400 になり、応答が「知らない名前」だと分かることを確認する
// （タイポを黙って無視しない。既存の TestResumeCircuitBreaker_400WhenUnknownName
// と対になる）。
//
// 変異確認: unknown-name の検査を丸ごと削っても、未知の名前は ScopeUnknown
// なので分類チェックが 400 を返し、ステータスだけを見るアサーションは
// 通り続ける（レビュー指摘）。このテストがその検査の唯一の貢献であるメッセージ
// を見るので、検査を削るとメッセージが分類チェックの文言に変わり落ちる。
func TestResumeSitelessCircuitBreaker_400WhenUnknownName(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := newBreakerRouter(pool)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/not_a_real_breaker/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "unknown circuit breaker") {
		t.Errorf("body = %s, want it to mention \"unknown circuit breaker\"", body)
	}
}

// TestResumeSitelessCircuitBreaker_404WhenNotTripped は発動していない
// 置き場範囲ブレーカーへの resume が 404 になることを確認する（罠: 0 行なら
// 404 にする実装を残す。breakers_test.go の
// TestResumeCircuitBreaker_404WhenNotTripped と対になる）。
//
// 変異確認: ResumeSitelessCircuitBreaker の `if n == 0` を `if n < 0` に
// 変えると（削除件数は execrows なので常に 0 以上）このガードが絶対に発火
// しなくなり、0 行 DELETE でも 204 を返してしまう。このテストはその変異で
// 落ちる（204 != 404）。
func TestResumeSitelessCircuitBreaker_404WhenNotTripped(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := newBreakerRouter(pool)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 (body=%s)", resp.StatusCode, body)
	}
}

// TestResumeSitelessCircuitBreaker_400WhenNoStoreConfigured は api の設定に FS 置き場が
// 無いとき（api は置き場無しでも起動できる）、再開する行の範囲を決められず 400 に
// なることを確認する。404 にすると「発動していない」と読めてしまう。
//
// 変異確認: breakerStoreScope == "" の検査を外すと範囲が空文字列の行を探して
// 404 になり、このテストが落ちる。
func TestResumeSitelessCircuitBreaker_400WhenNoStoreConfigured(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (scope, name, pending, threshold, detail)
VALUES ('test-store', 'delete_reconcile', 150, 100, '{"total":150}'::jsonb)`); err != nil {
		t.Fatalf("inserting circuit breaker fixture: %v", err)
	}
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool)}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "no FS store") {
		t.Errorf("body = %s, want it to say that the api config has no FS store", body)
	}
	if got := countCircuitBreakers(t, pool, ctx); got != 1 {
		t.Errorf("circuit breaker rows = %d, want 1 (a rejected resume must not delete)", got)
	}
}

// TestResumeCircuitBreaker_400WhenNameIsSiteless は旧ルート（site の範囲）に
// 置き場の範囲の名前（delete_reconcile）を渡すと 400 になることを確認する
// （受け入れ 2）。単一サイト（レジストリ未指定 = store.DefaultSite の 1 要素）でも、
// site の存在チェックとは独立に分類だけで弾かれることを見る。
//
// 変異確認: ResumeCircuitBreaker から範囲分類の検査を外すと、
// このリクエストは `DELETE ... WHERE scope = 'default' AND name =
// 'delete_reconcile'` を実行し、対象行が無いので 404 になる（400 を期待する
// このテストが落ちる）。
func TestResumeCircuitBreaker_400WhenNameIsSiteless(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := newBreakerRouter(pool)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/sites/default/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
}

// TestEPGEndpoint_UnregisteredDefaultSiteStill404 は「site 無しルートを足す」
// 解が既存の `knownSite` を緩めていないことの確認（罠: knownSite を緩めない）。
// レジストリに `default` という site が無い構成で EPG 系エンドポイントに
// `{site}=default` を渡すと、これまでどおり 404 になる。
func TestEPGEndpoint_UnregisteredDefaultSiteStill404(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Ready: pool.Ping, Sites: []string{"tokyo", "takamatsu"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sites/default/programs?start=2020-01-01T00:00:00Z&end=2020-01-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET .../sites/default/programs status = %d, want 404", resp.StatusCode)
	}
}

// 派生物の置き場が S3 のとき、その置き場の範囲の delete_reconcile も、site 無指定ルートで再開できる。
// FS 置き場の行が無いときだけ、派生物の置き場の行を引く。
func TestResumeSitelessCircuitBreaker_ResumesDerivedStoreScope(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	router := api.NewRouter(api.RouterConfig{
		Ready: pool.Ping, BreakerAdmin: pgstoretest.Breakers(pool), Sites: []string{"tokyo"},
		MediaStoreName: "test-store", DerivedStoreName: "s3main",
	})
	srv := httptest.NewServer(router)
	defer srv.Close()
	for _, scope := range []string{"test-store", "s3main"} {
		if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (scope, name, pending, threshold, detail)
VALUES ($1, 'delete_reconcile', 150, 100, '{"total":150}'::jsonb)`, scope); err != nil {
			t.Fatalf("inserting circuit breaker fixture for %s: %v", scope, err)
		}
	}
	resume := func() int {
		resp, err := http.Post(srv.URL+"/api/breakers/delete_reconcile/resume", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	remaining := func() []string {
		rows, err := pool.Query(ctx, `SELECT scope FROM circuit_breakers WHERE name = 'delete_reconcile' ORDER BY scope`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var scope string
			if err := rows.Scan(&scope); err != nil {
				t.Fatal(err)
			}
			out = append(out, scope)
		}
		return out
	}
	if got := resume(); got != http.StatusNoContent {
		t.Fatalf("first resume status = %d, want 204", got)
	}
	if got := remaining(); len(got) != 1 || got[0] != "s3main" {
		t.Fatalf("scopes after the first resume = %v, want only [s3main]", got)
	}
	if got := resume(); got != http.StatusNoContent {
		t.Fatalf("second resume status = %d, want 204 for the derived store scope", got)
	}
	if got := remaining(); len(got) != 0 {
		t.Fatalf("scopes after the second resume = %v, want none", got)
	}
	if got := resume(); got != http.StatusNotFound {
		t.Fatalf("third resume status = %d, want 404 when nothing is tripped", got)
	}
}
