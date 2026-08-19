package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown site status = %d, want 400", resp.StatusCode)
	}
	var errBody ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
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

	// レジストリにある site 名 → 201。かつ、その sites が実際に保存されていること
	// （201 を返しつつ sites を落とす実装でも通ってしまわないように、ステータスだけでなく
	// レスポンスの sites の中身まで確認する）。
	goodBody := map[string]any{
		"name":  "known-sites",
		"sites": []string{"tokyo", "osaka"},
	}
	raw, _ = json.Marshal(goodBody)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("known sites status = %d, want 201", resp.StatusCode)
	}
	var created Rule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if created.Sites == nil || !slices.Equal(*created.Sites, []string{"osaka", "tokyo"}) {
		t.Fatalf("created.Sites = %v, want [osaka tokyo]", created.Sites)
	}

	// rule_sites 空（未指定 = 全サイト）→ 201
	allSitesBody := map[string]any{"name": "all-sites"}
	raw, _ = json.Marshal(allSitesBody)
	resp, err = http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("omitted sites status = %d, want 201", resp.StatusCode)
	}

	// update でも同じ照合が効く → 未知 site で 400。
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
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update with unknown site status = %d, want 400", resp.StatusCode)
	}

	// 400 の後、既存の rule_sites が変わっていないことを実際に GET して確認する
	// （validate は tx の外なので今は自明に不変だが、将来 tx 内に移す変更を止める資産にする）。
	getResp, err := http.Get(srv.URL + "/api/rules/" + itoa(created.Id))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = getResp.Body.Close() }()
	var afterFailedUpdate Rule
	if err := json.NewDecoder(getResp.Body).Decode(&afterFailedUpdate); err != nil {
		t.Fatal(err)
	}
	if afterFailedUpdate.Sites == nil || !slices.Equal(*afterFailedUpdate.Sites, []string{"osaka", "tokyo"}) {
		t.Fatalf("sites after rejected update = %v, want unchanged [osaka tokyo]", afterFailedUpdate.Sites)
	}
}

// sites に空文字列要素があると 400 になることを確認する（issue #315 のレビューで発見:
// 空文字列を黙って無視すると「絞り込みたい」意図が黙って「全サイト」に反転する。
// validateEncodeProfiles が空名を拒否する流儀に揃える）。
func TestRuleSites_EmptyElementRejected(t *testing.T) {
	pool := testutil.SetupDB(t)
	router := NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"tokyo", "osaka"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	body := map[string]any{
		"name":  "empty-site-element",
		"sites": []string{"tokyo", ""},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty site element status = %d, want 400", resp.StatusCode)
	}
}

// レジストリから site が消えたあとに残る既存行は書き込み時照合の対象外（掃除しない）。
// クライアントは GET で得た sites をそのまま載せ直して PATCH する（web の編集フォームも
// この形）ので、保存済みの名前を照合対象にすると「消えた site を持つルールは名前や
// 優先度を直すだけの編集も一切保存できない」というロックアウトになる。保存済みの名前は
// 免除し、**新しく足された未知の名前だけ** 400 にすることを両方向で確認する
// （docs/schema/rules.md の rule_sites 節）。
func TestRuleSites_RegistryDriftAllowsRoundTripUpdate(t *testing.T) {
	pool := testutil.SetupDB(t)

	// tokyo がまだレジストリにある間に作成する。
	routerBefore := NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"tokyo", "osaka"},
	})
	srvBefore := httptest.NewServer(routerBefore)
	defer srvBefore.Close()

	body := map[string]any{
		"name":  "site-later-removed",
		"sites": []string{"tokyo"},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srvBefore.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created Rule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// tokyo がレジストリから消えたプロセスに切り替える（同じ DB、別 config を模す）。
	routerAfter := NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"osaka"},
	})
	srvAfter := httptest.NewServer(routerAfter)
	defer srvAfter.Close()

	patch := func(t *testing.T, bodyMap map[string]any) (int, Rule) {
		t.Helper()
		b, err := json.Marshal(bodyMap)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPatch, srvAfter.URL+"/api/rules/"+itoa(created.Id), bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out Rule
		if res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, out
	}

	// 保存済みの sites をそのまま載せ直す read-modify-write な PATCH（名前だけ変える）は
	// 通り、消えた site も残る。
	status, updated := patch(t, map[string]any{
		"name":  "renamed-while-site-missing",
		"sites": []string{"tokyo"},
	})
	if status != http.StatusOK {
		t.Fatalf("round-trip update after site removed from registry status = %d, want 200", status)
	}
	if updated.Name != "renamed-while-site-missing" {
		t.Errorf("name after round-trip update = %q, want renamed-while-site-missing", updated.Name)
	}
	if updated.Sites == nil || !slices.Equal(*updated.Sites, []string{"tokyo"}) {
		t.Errorf("sites after round-trip update = %v, want [tokyo]", updated.Sites)
	}

	// 免除は「保存済みの名前」に限る。新しく未知の名前を足す PATCH は 400。
	status, _ = patch(t, map[string]any{
		"name":  "renamed-while-site-missing",
		"sites": []string{"tokyo", "toukyou"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("adding a new unknown site status = %d, want 400", status)
	}

	// 免除はルール単位。保存済みでない消えた site 名は他のルールでも通らない。
	otherBody := map[string]any{"name": "other-rule", "sites": []string{"osaka"}}
	rawOther, _ := json.Marshal(otherBody)
	otherResp, err := http.Post(srvAfter.URL+"/api/rules", "application/json", bytes.NewReader(rawOther))
	if err != nil {
		t.Fatal(err)
	}
	var other Rule
	if err := json.NewDecoder(otherResp.Body).Decode(&other); err != nil {
		t.Fatal(err)
	}
	_ = otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusCreated {
		t.Fatalf("other rule create status = %d, want 201", otherResp.StatusCode)
	}
	rawOther, _ = json.Marshal(map[string]any{"name": "other-rule", "sites": []string{"tokyo"}})
	otherReq, _ := http.NewRequest(http.MethodPatch, srvAfter.URL+"/api/rules/"+itoa(other.Id), bytes.NewReader(rawOther))
	otherReq.Header.Set("Content-Type", "application/json")
	otherUpdate, err := http.DefaultClient.Do(otherReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = otherUpdate.Body.Close() }()
	if otherUpdate.StatusCode != http.StatusBadRequest {
		t.Fatalf("borrowing another rule's missing site status = %d, want 400", otherUpdate.StatusCode)
	}
}

// 免除は「そのルールに保存済みの名前」なので、**create には効かない**。検索画面の
// 「別の新しいルールとして保存」（フォーク。POST /api/rules に preserve した sites を
// 載せる）は、レジストリから site が消えた後は 400 になる。これは意図した現状であり
// （免除の切り方を変えるか、web 側で明示的に外させるかは #315 で判断を仰いでいる）、
// docs/schema/rules.md の rule_sites 節と docs/frontend/search.md が同じことを書いている。
// 挙動を測らずに docs に書かないためのテストなので、そちらを変えるならここも落ちる。
func TestRuleSites_RegistryDriftRejectsFork(t *testing.T) {
	pool := testutil.SetupDB(t)

	// tokyo がまだレジストリにある間に「フォーク元」を作る。
	srvBefore := httptest.NewServer(NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"tokyo", "osaka"},
	}))
	defer srvBefore.Close()

	raw, _ := json.Marshal(map[string]any{"name": "fork-source", "sites": []string{"tokyo"}})
	resp, err := http.Post(srvBefore.URL+"/api/rules", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var source Rule
	if err := json.NewDecoder(resp.Body).Decode(&source); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork source create status = %d, want 201", resp.StatusCode)
	}
	if source.Sites == nil || !slices.Equal(*source.Sites, []string{"tokyo"}) {
		t.Fatalf("fork source sites = %v, want [tokyo]", source.Sites)
	}

	// tokyo がレジストリから消えたプロセスに切り替える。
	srvAfter := httptest.NewServer(NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"osaka"},
	}))
	defer srvAfter.Close()

	// web のフォークが送るボディ（GET で得た sites をそのまま preserve して POST）。
	rawFork, _ := json.Marshal(map[string]any{
		"name":  source.Name + " のコピー",
		"sites": *source.Sites,
	})
	forkResp, err := http.Post(srvAfter.URL+"/api/rules", "application/json", bytes.NewReader(rawFork))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = forkResp.Body.Close() }()
	if forkResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fork after site removed from registry status = %d, want 400", forkResp.StatusCode)
	}
	var forkErr ErrorResponse
	if err := json.NewDecoder(forkResp.Body).Decode(&forkErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(forkErr.Error, `unknown site "tokyo"`) {
		t.Errorf(`fork error body = %q, want mention of unknown site "tokyo"`, forkErr.Error)
	}

	// 同じ site を持つ元ルールの PATCH（上書き保存）は通り続ける —— 壊れているのは
	// フォーク経路だけで、免除そのものは効いている。
	rawPatch, _ := json.Marshal(map[string]any{"name": "fork-source", "sites": *source.Sites})
	patchReq, err := http.NewRequest(http.MethodPatch, srvAfter.URL+"/api/rules/"+itoa(source.Id), bytes.NewReader(rawPatch))
	if err != nil {
		t.Fatal(err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = patchResp.Body.Close() }()
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("overwrite of the fork source status = %d, want 200", patchResp.StatusCode)
	}
}

// PATCH は「存在しない id + 未知 site」に 404 ではなく 400 を返す（validateRuleInput が
// GetRule より前）。免除集合を tx の外で読むこの実装では、存在しないルールの savedRuleSites
// が空集合になることに依存しているので、この順序には意味がある（順序を入れ替えるなら、
// 404 を先に返す実装として意識的にやること）。UpdateRule のコメントと対になるテスト。
func TestUpdateRule_UnknownSiteBeatsNotFound(t *testing.T) {
	pool := testutil.SetupDB(t)
	srv := httptest.NewServer(NewRouter(RouterConfig{
		Pool:  pool,
		Sites: []string{"tokyo"},
	}))
	defer srv.Close()

	patch := func(t *testing.T, body map[string]any) int {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/api/rules/999999", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode
	}

	if status := patch(t, map[string]any{"name": "ghost", "sites": []string{"toukyou"}}); status != http.StatusBadRequest {
		t.Errorf("missing rule + unknown site status = %d, want 400", status)
	}
	// 入力が妥当なら 404 に落ちる（400 が全部を飲み込んでいるわけではない）。
	if status := patch(t, map[string]any{"name": "ghost", "sites": []string{"tokyo"}}); status != http.StatusNotFound {
		t.Errorf("missing rule + known site status = %d, want 404", status)
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
