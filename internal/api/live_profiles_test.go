package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// intPtr は optional な height を組み立てるための小さなヘルパ。
func intPtr(v int) *int { return &v }

// TestListLiveProfiles は一覧が config の定義順に name と height を返すことを見る。
//
// **順序そのものが既定の根拠である**（先頭 = `?profile=` 省略時のプロファイル）ので、
// name の集合だけでなく並びまで固定する。height は表示用で、0 は
// 「スケールしない」を表す（値そのものが設定の写しであることも見る）。
func TestListLiveProfiles(t *testing.T) {
	router := NewRouter(RouterConfig{
		LiveProfiles: []LiveProfileSummary{
			{Name: "high", Height: intPtr(720)},
			{Name: "low", Height: intPtr(480)},
		},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body []LiveProfileSummary
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// 並びは設定順（リテラルで書く。実装の定数と比較しない）。
	if len(body) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(body), body)
	}
	if body[0].Name != "high" || body[1].Name != "low" {
		t.Errorf("names = [%q, %q], want [high, low]", body[0].Name, body[1].Name)
	}
	if body[0].Height == nil || *body[0].Height != 720 {
		t.Errorf("height[0] = %v, want 720", body[0].Height)
	}
	if body[1].Height == nil || *body[1].Height != 480 {
		t.Errorf("height[1] = %v, want 480", body[1].Height)
	}
}

// TestListLiveProfiles_EmptyWhenNotConfigured は未定義（`live.enabled: false` を含む）の
// ときの形を見る。**null ではなく空配列** --- フロントは length で「セレクタを出すか」を
// 決めるので、null は別の分岐を書かせる。
func TestListLiveProfiles_EmptyWhenNotConfigured(t *testing.T) {
	router := NewRouter(RouterConfig{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body []LiveProfileSummary
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body == nil {
		t.Fatal("body is null, want empty array")
	}
	if len(body) != 0 {
		t.Errorf("len = %d, want 0", len(body))
	}
}

// TestListLiveProfiles_OmitsHeightWhenUnset は height 未設定（スケールしない）が
// 省略されることを見る。0 を詰めると「未設定」と「0」が公開面で潰れる。
func TestListLiveProfiles_OmitsHeightWhenUnset(t *testing.T) {
	router := NewRouter(RouterConfig{
		LiveProfiles: []LiveProfileSummary{{Name: "original"}},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body []LiveProfileSummary
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len = %d, want 1", len(body))
	}
	if body[0].Height != nil {
		t.Errorf("height = %v, want omitted", *body[0].Height)
	}
}
