package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
)

// liveProfileEntry は応答 1 件分（`height` の有無を見たいので生の JSON に近い形）。
type liveProfileEntry struct {
	Name   string `json:"name"`
	Height *int   `json:"height"`
}

// fetchLiveProfiles は起動中のサーバーから一覧を取る。
func fetchLiveProfiles(t *testing.T, base string) []liveProfileEntry {
	t.Helper()
	resp, err := http.Get(base + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body []liveProfileEntry
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return body
}

// TestServerLiveProfiles_MatchesConfig は GET /api/live-profiles が config の
// `live.profiles` を**設定順のまま** name と height で返すことを見る。
//
// **順序が既定の根拠である**（先頭 = `?profile=` 省略時のプロファイル。
// `internal/streamer` の `LiveConfig.profile`）ので、並びまで固定する。
//
// **`height` を書いていないプロファイルは省略される**（`liveProfileSummaries` の
// `p.Height > 0` ガード）。省略と 0 を潰すと「height が設定されていない」という
// 事実が公開面から消える（フロントは 0 を「0p」とは表示しない。理由は
// `cmd/rokuban/server.go` の `liveProfileSummaries`）。
//
// 壊し方: `liveProfileSummaries` を config の逆順に写す / `height` を写さない /
// ガードを外して `&p.Height` を常に詰める。
func TestServerLiveProfiles_MatchesConfig(t *testing.T) {
	base := startTestServer(t, "api", liveEnabledYAML)
	body := fetchLiveProfiles(t, base)

	if len(body) != 3 {
		t.Fatalf("len = %d, want 3: %#v", len(body), body)
	}
	got := []string{body[0].Name, body[1].Name, body[2].Name}
	want := []string{"hd", "sd", "original"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("names = %v, want %v（config の定義順）", got, want)
			break
		}
	}
	if body[0].Height == nil || *body[0].Height != 720 {
		t.Errorf("height[0] = %v, want 720", body[0].Height)
	}
	if body[1].Height == nil || *body[1].Height != 480 {
		t.Errorf("height[1] = %v, want 480", body[1].Height)
	}
	// height 未設定（= スケールしない）は省略。0 を返さない。
	if body[2].Height != nil {
		t.Errorf("height[2] = %v, want omitted（height を書いていない）", *body[2].Height)
	}

	// **機微情報を載せない。** 生の JSON を読み直して、config の定義そのもの
	// （コーデック・品質・引数）が 1 文字も現れないことを見る。
	raw, err := http.Get(base + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = raw.Body.Close() }()
	var any1 any
	if err := json.NewDecoder(raw.Body).Decode(&any1); err != nil {
		t.Fatalf("decoding raw response: %v", err)
	}
	encoded, err := json.Marshal(any1)
	if err != nil {
		t.Fatalf("re-encoding response: %v", err)
	}
	for _, leaked := range []string{"libx264", "aac", "video_codec", "audio_codec"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("response contains %q, want it omitted（config の定義そのものを載せない）: %s",
				leaked, encoded)
		}
	}
}

// TestServerLiveProfiles_RoleIndependent は一覧が**どのロールでも同じ答え**を
// 返すことを見る。生成ルートはロールで絞られないので、答えがロールで変わると
// 同一デプロイの中で矛盾する（issue #209 で `live` がまさにこの形で壊れていた。
// そのときの再発防止テストが `TestServerCapabilities_LiveIsRoleIndependent`）。
//
// 壊し方: `cmd/rokuban/server.go` の `LiveProfiles:` の代入を
// `if slices.Contains(roles, "api") {` の内側へ移す。
func TestServerLiveProfiles_RoleIndependent(t *testing.T) {
	for _, roles := range []string{"api", "notifier"} {
		t.Run(roles, func(t *testing.T) {
			base := startTestServer(t, roles, liveEnabledYAML)
			body := fetchLiveProfiles(t, base)
			if len(body) != 3 {
				t.Fatalf("roles=%s: len = %d, want 3（config は 3 プロファイル）: %#v",
					roles, len(body), body)
			}
			if body[0].Name != "hd" {
				t.Errorf("roles=%s: first = %q, want hd", roles, body[0].Name)
			}
		})
	}
}

// TestServerLiveProfiles_ReturnedEvenWhenLiveDisabled は **`live.enabled: false`
// でも、profiles が書かれていれば一覧が空にならない**ことを固定する。
//
// 一覧は「config に何が定義されているか」の写しであり、「有効かどうか」は
// `GET /api/capabilities` の `live` の問いである（2 箇所で判定しない）。
// `config.compose.yml` は `enabled: false` と `profiles` を並べて出荷しているので、
// これは机上の形ではない。**この 1 件が無いと、doc コメントと openapi の
// description の主張（無効なら空）を誰も押さえない。**
func TestServerLiveProfiles_ReturnedEvenWhenLiveDisabled(t *testing.T) {
	base := startTestServer(t, "api", liveDisabledWithProfilesYAML)
	body := fetchLiveProfiles(t, base)

	if len(body) != 1 {
		t.Fatalf("len = %d, want 1（`enabled: false` でも定義は写す）: %#v", len(body), body)
	}
	if body[0].Name != "hd" {
		t.Errorf("name = %q, want hd", body[0].Name)
	}
	if body[0].Height == nil || *body[0].Height != 720 {
		t.Errorf("height = %v, want 720", body[0].Height)
	}
}

// TestServerLiveProfiles_EmptyWhenLiveAbsent は config に `live:` 節が無いとき
// 空配列になることを見る。**null ではない** --- フロントは length でセレクタの
// 出し分けを決めるので、null は別の分岐を書かせる。
func TestServerLiveProfiles_EmptyWhenLiveAbsent(t *testing.T) {
	base := startTestServer(t, "api", liveAbsentYAML)

	raw, err := http.Get(base + "/api/live-profiles")
	if err != nil {
		t.Fatalf("GET /api/live-profiles: %v", err)
	}
	defer func() { _ = raw.Body.Close() }()
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", raw.StatusCode)
	}
	var body []liveProfileEntry
	if err := json.NewDecoder(raw.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body == nil {
		t.Fatal("body is null, want empty array")
	}
	if len(body) != 0 {
		t.Errorf("len = %d, want 0", len(body))
	}
}

// TestLiveProfileSummaries は config → 公開面の写し（`liveProfileSummaries`）を
// **DB も実サーバーも使わずに**固定する。
//
// 実プロセスを起動する `TestServerLiveProfiles_*` は config 全体（`live:` 節の
// 解釈を含む）を通すが、Postgres を要求する。この 1 件は純関数だけを見るので、
// DB が無い環境でも「height の 0 を省略する」「順序を保つ」を押さえられる。
//
// 壊し方: `if p.Height > 0` を外して常に `&p.Height` を詰める / 逆順に append する。
func TestLiveProfileSummaries(t *testing.T) {
	got := liveProfileSummaries([]config.LiveProfile{
		{Name: "hd", Height: 720},
		{Name: "original"},
		{Name: "sd", Height: 480},
	})

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %#v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	want := []string{"hd", "original", "sd"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names = %v, want %v（config の定義順）", names, want)
			break
		}
	}
	if got[0].Height == nil || *got[0].Height != 720 {
		t.Errorf("height[0] = %v, want 720", got[0].Height)
	}
	if got[2].Height == nil || *got[2].Height != 480 {
		t.Errorf("height[2] = %v, want 480", got[2].Height)
	}
	// height 未設定（= スケールしない）は省略する。0 を詰めると「height が
	// 設定されていない」という事実が潰れる（表示は変わらない ---
	// `liveProfileLabel` は 0 でも名前だけを返す）。
	if got[1].Height != nil {
		t.Errorf("height[1] = %v, want omitted（height を書いていない）", *got[1].Height)
	}
}
