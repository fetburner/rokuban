package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHealthz(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
}

func TestGetVersion(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
}

func TestAllowedHosts_ValidHost(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"localhost"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestAllowedHosts_InvalidHost(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"localhost"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// /healthz と /metrics は allowlist を免除しているので、
	// 検証が効いていることはデータ側のエンドポイントで確かめる。
	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// リバースプロキシ前段では Host がプロキシ自身の値に書き換わり、元の
// クライアント向け Host は X-Forwarded-Host に移る。TrustForwardedHost を
// opt-in した構成（信頼できるプロキシが必ず前段に居る）では、X-Forwarded-Host
// が allowlist 内なら r.Host が allowlist 外（プロキシのホスト名）でも通す
// （docs/api.md §リバースプロキシ・フレンドリー要件、issue #89 / #216）。
func TestAllowedHosts_ForwardedHostValidPassesEvenIfHostInvalid(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// r.Host はプロキシ自身のホスト名（allowlist 外）。
	req.Host = "internal-proxy.example.com"
	req.Header.Set("X-Forwarded-Host", "rokuban.local")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（X-Forwarded-Host が allowlist 内なら通すべき）", resp.StatusCode, http.StatusOK)
	}
}

// TrustForwardedHost を opt-in した構成では、X-Forwarded-Host が allowlist
// 外なら r.Host の値に関わらず 400 にする（プロキシが外来のヘッダーを
// 上書きする前提の構成なので、ここに来る値はプロキシ自身が付けた値）。
func TestAllowedHosts_ForwardedHostInvalidRejectsEvenIfHostValid(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// r.Host 自体は allowlist 内だが、X-Forwarded-Host が優先されるべき。
	req.Host = "rokuban.local"
	req.Header.Set("X-Forwarded-Host", "evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（X-Forwarded-Host が allowlist 外なら拒否すべき）", resp.StatusCode, http.StatusBadRequest)
	}
}

// X-Forwarded-Host ヘッダーが無いリクエストは従来通り r.Host で検証する
// （前段にリバースプロキシが居ない直接アクセス構成の回帰確認）。
func TestAllowedHosts_NoForwardedHostFallsBackToHost(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example.com"
	// X-Forwarded-Host は設定しない。

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（ヘッダー無しなら r.Host で検証すべき）", resp.StatusCode, http.StatusBadRequest)
	}
}

// 複数プロキシを経由すると X-Forwarded-Host は X-Forwarded-For と同様、
// 各ホップが自分の見た値をカンマ区切りで追記しうる。ポート付きの値が
// 先頭に来た場合、net.SplitHostPort が最後のコロンで割るだけだと
// host="rokuban.local" / port="443, evil.example.com" のように誤分解され、
// 意図せず通ってしまう（レビュー指摘）。先頭カンマ要素を切り出してから
// stripPort する実装であることを確認する。
func TestAllowedHosts_ForwardedHostCommaSeparatedWithPortUsesFirstElement(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "internal-proxy.example.com"
	req.Header.Set("X-Forwarded-Host", "rokuban.local:443, evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（先頭要素 rokuban.local:443 が allowlist 内なら通すべき）", resp.StatusCode, http.StatusOK)
	}
}

// ポートを含まない場合でも、カンマ区切りの先頭要素が allowlist 内なら通す。
// レビュー指摘の再現ケース: 修正前の実装は Get() の生値に stripPort を直接
// 適用していたため、"rokuban.local, evil.example.com"（コロンを含まない）
// では SplitHostPort が失敗して元の文字列がそのまま比較され、正当な先頭
// 要素であっても一致せず 400 になっていた（ポートの有無で結果が変わる
// 一貫性の無さそのもの）。
func TestAllowedHosts_ForwardedHostCommaSeparatedNoPortUsesFirstElement(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "internal-proxy.example.com"
	req.Header.Set("X-Forwarded-Host", "rokuban.local, evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（先頭要素 rokuban.local が allowlist 内なら通すべき）", resp.StatusCode, http.StatusOK)
	}
}

// 先頭要素（クライアントに最も近い値）が allowlist 外なら、後続のカンマ要素に
// allowlist 内の値が含まれていても拒否する。「いずれかの要素が一致すれば通す」
// という誤った実装（更なる抜け道）になっていないことを確認する。
func TestAllowedHosts_ForwardedHostCommaSeparatedRejectsWhenFirstElementInvalid(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "internal-proxy.example.com"
	req.Header.Set("X-Forwarded-Host", "evil.example.com, rokuban.local")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（先頭要素 evil.example.com が allowlist 外なら拒否すべき）", resp.StatusCode, http.StatusBadRequest)
	}
}

// X-Forwarded-Host が複数のヘッダー行に分かれて送られた場合も、最初の行を
// 権威として使う（複数行は連結した1つの値と等価に扱う。RFC 9110 §5.3）。
func TestAllowedHosts_ForwardedHostMultipleHeaderLinesUsesFirstLine(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "internal-proxy.example.com"
	req.Header.Add("X-Forwarded-Host", "rokuban.local")
	req.Header.Add("X-Forwarded-Host", "evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（最初の行 rokuban.local が allowlist 内なら通すべき）", resp.StatusCode, http.StatusOK)
	}
}

// 上のテストの行の順序を逆にすると結果も反転すること（最初の行が権威である
// ことの両方向確認）。
func TestAllowedHosts_ForwardedHostMultipleHeaderLinesRejectsWhenFirstLineInvalid(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "internal-proxy.example.com"
	req.Header.Add("X-Forwarded-Host", "evil.example.com")
	req.Header.Add("X-Forwarded-Host", "rokuban.local")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（最初の行 evil.example.com が allowlist 外なら拒否すべき）", resp.StatusCode, http.StatusBadRequest)
	}
}

// alwaysAllowedHosts（localhost 系の常時許可）は実際の TCP 接続相手が
// localhost であることを根拠にした緩和であり、自己申告値である
// X-Forwarded-Host には適用してはならない。r.Host が allowlist 外でも
// X-Forwarded-Host: localhost を送るだけで allowlist を素通りできてしまう
// バグ（レビュー指摘）の回帰を防ぐ。
func TestAllowedHosts_ForwardedHostLocalhostDoesNotBypassAllowlist(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// r.Host は allowlist 外（DNS rebinding された攻撃者ドメイン等を想定）。
	req.Host = "attacker-controlled.example.com"
	req.Header.Set("X-Forwarded-Host", "localhost")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（X-Forwarded-Host: localhost が allowlist をバイパスしてはいけない）", resp.StatusCode, http.StatusBadRequest)
	}
}

// r.Host が直接 localhost の場合（前段にプロキシが居ない直接アクセス）は
// 従来通り常時許可する（回帰確認）。
func TestAllowedHosts_DirectLocalhostStillBypassesAllowlist(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	// X-Forwarded-Host は設定しない。

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（r.Host が localhost なら常時許可すべき）", resp.StatusCode, http.StatusOK)
	}
}

func TestAllowedHosts_EmptyAllowsAll(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "anything.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (empty allowlist should allow all)", resp.StatusCode, http.StatusOK)
	}
}

// 直接露出構成（前段にリバースプロキシが居ない、TrustForwardedHost の既定
// false）では X-Forwarded-Host を一切見ない。DNS rebinding の攻撃ページは
// 同一オリジンとして任意のリクエストヘッダーを付けられるので、この免除が
// 無いと `X-Forwarded-Host: <allowlist に載っている値>` を自己申告するだけで
// allowlist を素通りできてしまう（issue #216）。r.Host には DNS rebinding
// された攻撃者ドメインが来る想定。
func TestAllowedHosts_UntrustedForwardedHostDoesNotBypassAllowlist(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// r.Host は DNS rebinding された攻撃者ドメイン（allowlist 外）。
	req.Host = "attacker-controlled.example.com"
	// 攻撃側が allowlist に載っている値を自己申告する。
	req.Header.Set("X-Forwarded-Host", "rokuban.local")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（TrustForwardedHost 未設定なら X-Forwarded-Host の自己申告で allowlist を素通りしてはいけない）", resp.StatusCode, http.StatusBadRequest)
	}
}

// TrustForwardedHost を明示的に opt-in した、正しいリバースプロキシ構成
// （プロキシが外来の X-Forwarded-Host を上書きする）では、従来どおり
// X-Forwarded-Host を検証対象にして通す（上のテストとの両方向確認）。
func TestAllowedHosts_TrustedForwardedHostStillPasses(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}, TrustForwardedHost: true})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// r.Host はプロキシ自身のホスト名（allowlist 外だが、プロキシ経由なので安全）。
	req.Host = "internal-proxy.example.com"
	req.Header.Set("X-Forwarded-Host", "rokuban.local")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d（TrustForwardedHost: true なら X-Forwarded-Host が allowlist 内で通すべき）", resp.StatusCode, http.StatusOK)
	}
}

func newTestDistFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          {Data: []byte("<html>app</html>")},
		"assets/index-Ab1.js": {Data: []byte("console.log('app')")},
		"favicon.svg":         {Data: []byte("<svg/>")},
	}
}

func TestSPA_IndexHTML(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

func TestSPA_HashedAssetImmutable(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/assets/index-Ab1.js")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	want := "public, max-age=31536000, immutable"
	if cc := resp.Header.Get("Cache-Control"); cc != want {
		t.Errorf("Cache-Control = %q, want %q", cc, want)
	}
}

func TestSPA_FallbackToIndex(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/some/client/route")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q (SPA fallback)", cc, "no-cache")
	}
}

func TestSPA_DirectIndexHTMLNoCache(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

// public/ 由来のファイルはハッシュを持たないので、差し替えても URL が変わらない。
// immutable を付けたり無指定にすると古いファビコンが出続ける。
func TestSPA_UnhashedAssetNoCache(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

func TestSPA_APITakesPrecedence(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("API /healthz should still work with SPA enabled, got status=%q", body.Status)
	}
}

// /metrics は registry を渡したときだけ公開されること。
func TestMetrics_Endpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_test_metric_total",
		Help: "test",
	}))

	router := NewRouter(RouterConfig{MetricsRegistry: reg})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !strings.Contains(string(body), "rokuban_test_metric_total") {
		t.Errorf("body does not contain the registered metric:\n%s", body)
	}
	// Prometheus の text exposition format であること
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestMetrics_DisabledWithoutRegistry(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Error("/metrics should not be registered without a registry")
	}
}

// api ロール単独（Mounter を持たない構成）では /api/events が生えないこと。
//
// SSE 配信は notifier ロールの担当に分離した（M2-19）。api は mirakc にも
// ファイルシステムにも依存しない純粋なリクエスト/レスポンス層になり、
// これによって初めて docs/overview.md が既に謳っていた
// 「api ロールは scale-to-zero 可能」が名実ともに成立する。
func TestEvents_NotMountedForAPIOnly(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (api ロール単独では /api/events は登録されない)", resp.StatusCode)
	}
}

// /metrics と /healthz は Host allowlist を免除すること。
//
// 監視基盤は Pod IP やサービス名で叩くため allowlist に載せようがない
// （IP は動的）。allowlist の内側に置くと k8s の liveness probe と
// Prometheus の scrape が 400 で落ちる。
func TestAllowedHosts_InfraPathsAreExempt(t *testing.T) {
	reg := prometheus.NewRegistry()
	router := NewRouter(RouterConfig{
		AllowedHosts:    []string{"rokuban.local"},
		MetricsRegistry: reg,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, path := range []string{"/healthz", "/metrics"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Prometheus / kubelet が Pod IP で叩く状況
			req.Host = "10.42.0.7:40773"

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200（allowlist を免除すべき）", resp.StatusCode)
			}
		})
	}

	// データを扱うエンドポイントは免除しない
	req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "10.42.0.7:40773"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/api/version status = %d, want 400（allowlist は効いているべき）", resp.StatusCode)
	}
}

// localhost 系は allowlist の設定に関わらず許可すること
// （docs/configuration.md の記述に合わせる）。
func TestAllowedHosts_LocalhostAlwaysAllowed(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, host := range []string{"localhost", "localhost:40773", "127.0.0.1:40773", "[::1]:40773"} {
		t.Run(host, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+"/api/version", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// /api/ 配下の未マッチは SPA に落とさず 404 にする（issue #209）。
//
// 落とすと「無い」が「200 の HTML」になる。実害の出た経路が下の
// TestSPA_LivePlaylistNotFoundWhenLiveDisabled で、こちらはその一般形。
func TestSPA_APIPathsNotFallback(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Error == "" {
		t.Error("error message is empty")
	}
}

// issue #209 の再現そのもの。live.enabled が false のとき streamer はライブの
// ルートを登録しないので、このパスは未マッチになる。SPA に落とすと
// probeLivePlaylist（web/src/lib/live.ts）が HTML 200 を成功扱いし、
// 「無効な機能」ではなく「壊れた再生」として見える。
//
// **URL は「ライブを有効にすれば実在する」現在の形でなければならない**
// （`/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/...`。
// issue #217 で id 空間を一覧 API に揃えたときに変わった）。有効にしても
// 存在しないパスを置くと、このテストは直上の TestSPA_APIPathsNotFallback と
// 同じ「`/api/` 配下の未マッチが JSON 404 になる」しか主張しなくなる ---
// 実際、旧形式のまま残っていたためレビューで指摘された。api は streamer を
// import できない（streamer → api の依存があるので循環する）ので、実際に
// Mount した状態との対比は `internal/streamer` の
// TestLiveMount_DisabledDoesNotFallBackToSPA が持つ（あちらは有効時に同じ
// パスが 404 でなくなることまで見る）。
func TestSPA_LivePlaylistNotFoundWhenLiveDisabled(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/sites/default/networks/31920/services/53248/live/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want non-HTML (SPA フォールバックに落ちている)", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "<html>") {
		t.Errorf("body = %q, want non-HTML", raw)
	}
}

// クライアントルートは従来どおり index.html に落ちる（/api/ の 404 化で
// SPA のディープリンクを壊していないこと。/live 直リンクを含む）。
func TestSPA_ClientRoutesStillFallback(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, path := range []string{"/live", "/recordings", "/programs", "/apiary"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
		if string(raw) != "<html>app</html>" {
			t.Errorf("GET %s: body = %q, want index.html", path, raw)
		}
	}
}

// `/api`（末尾スラッシュ無し）も SPA に落とさない。落とすと末尾スラッシュの
// 有無で「404」と「HTML 200」に分かれ、docs の「`/api/` 配下は 404」という
// 記述の境界が 1 文字ぶんずれる（issue #209 のレビュー指摘）。
func TestSPA_APIRootNotFallback(t *testing.T) {
	router := NewRouter(RouterConfig{DistFS: newTestDistFS()})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want non-HTML", ct)
	}
}
