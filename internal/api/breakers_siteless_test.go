package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestResumeCircuitBreaker_DefaultSiteRegistryHole は issue #450 の再現テスト
// （sub issue 本文「含むもの」1）。`config.mirakcs` に `default` という名前の
// site が無いレジストリ（[tokyo, takamatsu]）では、site を持たない
// delete_reconcile の発動を site スコープの resume からはどちらの経路でも
// 解除できない ---
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
	router := api.NewRouter(api.RouterConfig{Pool: pool, Sites: []string{"tokyo", "takamatsu"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// worker が書く形（site 列は空文字列）で発動中の delete_reconcile を用意する。
	if _, err := pool.Exec(ctx, `
INSERT INTO circuit_breakers (site, name, pending, threshold, detail)
VALUES ('', 'delete_reconcile', 150, 100, '{"total":150}'::jsonb)`); err != nil {
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
				t.Errorf("delete_reconcile row site = %q, want empty (site を持たない)", b.Site)
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/breakers does not contain delete_reconcile: %+v", listed)
	}

	// (1) レジストリに "default" という site は無いので 400。
	resp1, err := http.Post(srv.URL+"/api/sites/default/breakers/delete_reconcile/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp1.Body)
		t.Errorf("POST .../sites/default/breakers/delete_reconcile/resume status = %d, want 400 (body=%s)", resp1.StatusCode, body)
	}

	// (2) 登録済みの site (tokyo) を渡しても、delete_reconcile は site を
	// 持たない名前なので 400 になる（「知っている site だが行が無い」404 では
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
		t.Fatalf("circuit_breakers rows for delete_reconcile = %d, want 1 (site スコープの resume が誤って消してはいけない)", remaining)
	}

	// (3) site を持たない新ルートなら site の指定なしに解除できる。
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
// breaker.IsSiteless の検査を外すと、このリクエストはそのまま site 列が
// 空文字列で name が `ruler_deletes` の行を DELETE しようとし、対象行が
// 無いので 404 になる（400 を期待するこのテストが落ちる）。
func TestResumeSitelessCircuitBreaker_400WhenNameHasSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Pool: pool})
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
// 渡すと 400 になることを確認する（タイポを黙って無視しない。既存の
// TestResumeCircuitBreaker_400WhenUnknownName と対になる）。
func TestResumeSitelessCircuitBreaker_400WhenUnknownName(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/breakers/not_a_real_breaker/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
}

// TestResumeCircuitBreaker_400WhenNameIsSiteless は旧ルート（site スコープ）に
// site を持たない名前（delete_reconcile）を渡すと 400 になることを確認する
// （受け入れ 2）。単一サイト（レジストリ未指定 = db.DefaultSite の 1 要素）でも、
// site の存在チェックとは独立に分類だけで弾かれることを見る。
//
// 変異確認: ResumeCircuitBreaker から breaker.IsSiteless の検査を外すと、
// このリクエストは `DELETE ... WHERE site = 'default' AND name =
// 'delete_reconcile'` を実行し、対象行が無いので 404 になる（400 を期待する
// このテストが落ちる）。
func TestResumeCircuitBreaker_400WhenNameIsSiteless(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := api.NewRouter(api.RouterConfig{Pool: pool})
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
	router := api.NewRouter(api.RouterConfig{Pool: pool, Sites: []string{"tokyo", "takamatsu"}})
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
