package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	// dedupeWindowSeconds: 負値は 400、0 は境界として通る。
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
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("dedupeWindowSeconds=0 status = %d, want 201", resp.StatusCode)
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
