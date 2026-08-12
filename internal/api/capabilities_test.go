package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// getCapabilities は GET /api/capabilities を叩いて応答をデコードする。
func getCapabilities(t *testing.T, cfg RouterConfig) Capabilities {
	t.Helper()

	srv := httptest.NewServer(NewRouter(cfg))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("GET /api/capabilities: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return body
}

// live は config の live.enabled をそのまま映す。**両方向を見る** ---
// 片方だけだと定数を返す実装（常に true / 常に false）が通ってしまう。
func TestGetCapabilities_LiveFollowsConfig(t *testing.T) {
	if got := getCapabilities(t, RouterConfig{LiveEnabled: true}); !got.Live {
		t.Errorf("live = false, want true (LiveEnabled: true)")
	}
	if got := getCapabilities(t, RouterConfig{LiveEnabled: false}); got.Live {
		t.Errorf("live = true, want false (LiveEnabled: false)")
	}
}

// 未注入（テストの部分構成）は「すべて無効」に倒す。無効な機能の導線が
// 出る側に倒れると issue #209 の壊れ方（機能しない導線）を再演する。
func TestGetCapabilities_DefaultsToDisabled(t *testing.T) {
	if got := getCapabilities(t, RouterConfig{}); got.Live {
		t.Errorf("live = true, want false (未注入の既定)")
	}
}

// JSON のキー名は契約。フロント（orval 生成物）がこの名前で読むので、
// Go の構造体だけ見るテストでは守れない。
func TestGetCapabilities_JSONShape(t *testing.T) {
	srv := httptest.NewServer(NewRouter(RouterConfig{LiveEnabled: true}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("GET /api/capabilities: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	live, ok := raw["live"]
	if !ok {
		t.Fatalf(`response has no "live" key: %#v`, raw)
	}
	if live != true {
		t.Errorf(`"live" = %#v, want true`, live)
	}
}

// 能力 API は生成ルートなのでロールに関わらず生える（api ロールを持たない
// プロセスでも応答する）。**注入をロール所属で門番すると、同じ config の
// 別プロセスに聞いたときだけ答えが変わる**（issue #209 のレビュー指摘）。
// ここでは api の部分構成（DistFS 無し = SPA を配らないプロセス相当）でも
// 同じ値を返すことを固定する。cmd 側の代入位置は cmd/rokuban の
// TestServerRouterConfig_LiveEnabledIsRoleIndependent が見る。
func TestGetCapabilities_IndependentOfSPAServing(t *testing.T) {
	withSPA := getCapabilities(t, RouterConfig{LiveEnabled: true, DistFS: newTestDistFS()})
	withoutSPA := getCapabilities(t, RouterConfig{LiveEnabled: true})

	if withSPA != withoutSPA {
		t.Errorf("capabilities differ by SPA serving: %+v vs %+v", withSPA, withoutSPA)
	}
	if !withSPA.Live {
		t.Errorf("live = false, want true")
	}
}
