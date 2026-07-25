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
