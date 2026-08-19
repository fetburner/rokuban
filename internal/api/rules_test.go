package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRulesCRUD(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{Pool: pool})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// Create
	body := map[string]any{
		"name":     "アニメ全録",
		"priority": 20,
		"textMatches": []map[string]any{
			{"target": "name", "mode": "keyword", "value": "アニメ"},
		},
		"channelTypes": []string{"BS"},
		"genres":       []int{7},
		"sites":        []string{"default"},
	}
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
	if created.Id == 0 || created.Name != "アニメ全録" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if created.TextMatches == nil || len(*created.TextMatches) != 1 {
		t.Fatalf("textMatches = %v", created.TextMatches)
	}

	// List
	resp, err = http.Get(srv.URL + "/api/rules")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var list []Rule
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}

	// Get
	resp, err = http.Get(srv.URL + "/api/rules/" + itoa(created.Id))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}

	// Invalid regex (unbalanced paren — POSIX ARE 非互換)
	bad := map[string]any{
		"name": "bad",
		"textMatches": []map[string]any{
			{"target": "name", "mode": "regex", "value": "(unclosed"},
		},
	}
	raw, _ = json.Marshal(bad)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid regex status = %d, want 400", resp.StatusCode)
	}

	// filenameTemplate: 未知フィールドは 400（text/template の Execute で検出）
	badTemplate := map[string]any{
		"name":             "bad-template-field",
		"filenameTemplate": "{{.NoSuchField}}",
	}
	raw, _ = json.Marshal(badTemplate)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown filenameTemplate field status = %d, want 400", resp.StatusCode)
	}

	// filenameTemplate: 構文エラー（閉じ忘れ）も 400（text/template の Parse で検出）
	malformedTemplate := map[string]any{
		"name":             "bad-template-syntax",
		"filenameTemplate": "{{.Title",
	}
	raw, _ = json.Marshal(malformedTemplate)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed filenameTemplate status = %d, want 400", resp.StatusCode)
	}

	// filenameTemplate: 有効なテンプレートは通る
	goodTemplate := map[string]any{
		"name":             "good-template",
		"filenameTemplate": "{{.Year}}/{{.Month}}/{{.Title}}_{{.Hour}}{{.Min}}",
	}
	raw, _ = json.Marshal(goodTemplate)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid filenameTemplate status = %d, want 201", resp.StatusCode)
	}

	// dedupeThreshold: 値域外は 400、境界の両側を確認する。
	// -0.1 / 0 は下限違反、1.1 は上限違反。0 は "similarity() >= 0" が恒真になり
	// 録画を黙って止める危険な値なので、境界そのもの（下限側）として弾く対象に含める。
	for _, tc := range []float64{-0.1, 0} {
		body := map[string]any{"name": "dedupe-threshold-low", "dedupeEnabled": true, "dedupeThreshold": tc}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dedupeThreshold=%v status = %d, want 400", tc, resp.StatusCode)
		}
	}
	{
		body := map[string]any{"name": "dedupe-threshold-high", "dedupeEnabled": true, "dedupeThreshold": 1.1}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dedupeThreshold=1.1 status = %d, want 400", resp.StatusCode)
		}
	}
	// 妥当な値（境界の内側 0.5 と上限ちょうど 1）は通る。
	for _, tc := range []float64{0.5, 1} {
		body := map[string]any{"name": "dedupe-threshold-ok", "dedupeEnabled": true, "dedupeThreshold": tc}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("dedupeThreshold=%v status = %d, want 201", tc, resp.StatusCode)
		}
	}

	// dedupeWindowSeconds: 0 以下は 400（0 は「時間窓なし」ではなく恒偽。窓なしは
	// フィールドを省略する）、正値は通る。境界の両側を見る。
	{
		body := map[string]any{"name": "dedupe-window-negative", "dedupeWindowSeconds": -1}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dedupeWindowSeconds=-1 status = %d, want 400", resp.StatusCode)
		}
	}
	{
		body := map[string]any{"name": "dedupe-window-zero", "dedupeWindowSeconds": 0}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dedupeWindowSeconds=0 status = %d, want 400 "+
				"(0 is not \"no window\": it makes the window condition always false)", resp.StatusCode)
		}
	}
	{
		body := map[string]any{"name": "dedupe-window-positive", "dedupeWindowSeconds": 86400}
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("dedupeWindowSeconds=86400 status = %d, want 201", resp.StatusCode)
		}
	}

	// Delete
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/rules/"+itoa(created.Id), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	var del DeleteRuleResponse
	if err := json.NewDecoder(resp.Body).Decode(&del); err != nil {
		t.Fatal(err)
	}
	if del.Id != created.Id {
		t.Fatalf("delete id = %d", del.Id)
	}

	resp, err = http.Get(srv.URL + "/api/rules/" + itoa(created.Id))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d", resp.StatusCode)
	}
}

// encodeProfiles に config に無い名前があると 400 になること（issue #64）。
// 既知名は通ることも両方向で確認する。
func TestCreateRule_UnknownEncodeProfile(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 未知プロファイル → 400
	body := map[string]any{
		"name":           "with-unknown-profile",
		"encodeProfiles": []string{"no-such-profile"},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown encode profile status = %d, want 400", resp.StatusCode)
	}
	var errBody ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBody.Error, "unknown encode profile") {
		t.Errorf("error body = %q, want mention of unknown encode profile", errBody.Error)
	}

	// 既知プロファイル → 201
	okBody := map[string]any{
		"name":           "with-known-profile",
		"encodeProfiles": []string{"h264"},
	}
	raw, _ = json.Marshal(okBody)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("known encode profile status = %d, want 201", resp.StatusCode)
	}

	// EncodeProfileNames 未注入なら検証スキップ（後方互換・部分構成のテスト）
	routerNoProfiles := NewRouter(RouterConfig{Pool: pool})
	srv2 := httptest.NewServer(routerNoProfiles)
	defer srv2.Close()
	raw, _ = json.Marshal(body)
	resp, err = http.Post(srv2.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("without EncodeProfileNames status = %d, want 201 (validation skipped)", resp.StatusCode)
	}
}

// rule_sites に未知の site 名（タイポ含む）があると create/update が 400 になり、
// レジストリにある site 名と rule_sites 空（全サイト）は従来どおり通ることを確認する
// （issue #315。書き込み時のレジストリ照合が無いと、タイポがログを読まないと原因に
// 辿り着けない「意図したサイトで録られない」に化ける）。
func TestRuleSites_UnknownSiteRejected(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"tokyo", "osaka"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// 未知の site 名 → 400（保存されない）
	badBody := map[string]any{
		"name":  "typo-site",
		"sites": []string{"tokyo", "toukyou"},
	}
	raw, _ := json.Marshal(badBody)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown site status = %d, want 400", resp.StatusCode)
	}
	var errBody ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBody.Error, "unknown site") {
		t.Errorf("error body = %q, want mention of unknown site", errBody.Error)
	}

	// 未知 site を含むルールが保存されていないことを確認する（一覧が空のまま）。
	listResp, err := http.Get(srv.URL + "/api/rules")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listResp.Body.Close() }()
	var list []Rule
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rules were persisted despite unknown site: %+v", list)
	}

	// レジストリにある site 名 → 201
	goodBody := map[string]any{
		"name":  "known-sites",
		"sites": []string{"tokyo", "osaka"},
	}
	raw, _ = json.Marshal(goodBody)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("known sites status = %d, want 201", resp.StatusCode)
	}
	var created Rule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// rule_sites 空（未指定 = 全サイト）→ 201
	allSitesBody := map[string]any{"name": "all-sites"}
	raw, _ = json.Marshal(allSitesBody)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("omitted sites status = %d, want 201", resp.StatusCode)
	}

	// update でも同じ照合が効く → 未知 site で 400、既存の rule_sites は変わらない。
	updateBody := map[string]any{
		"name":  "known-sites",
		"sites": []string{"toukyou"},
	}
	raw, _ = json.Marshal(updateBody)
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/rules/"+itoa(created.Id), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update with unknown site status = %d, want 400", resp.StatusCode)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
