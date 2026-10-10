package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

// TestCutProfileSelection_RejectedOnEveryAPIPath は live.enabled=false の構成で
// cut のみのルール / override / 追記 API が拒否され、cut でない profile を含めば
// 通ることを API の 3 経路ぶん固定する（4 経路目は ingest の凍結）。
//
// **1 経路でも抜けると、その経路からしか作れない録画が「確認に再生が要り、
// 再生に encode が要り、encode に確認が要る」循環に落ちる。**
func TestCutProfileSelection_RejectedOnEveryAPIPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
		LiveEnabled:        false,
	}))
	t.Cleanup(srv.Close)

	// 1. ルール（POST /api/rules）。
	t.Run("rule", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"name": "cut-only", "encodeProfiles": []string{"cut"}})
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cut-only rule status = %d, want 400", resp.StatusCode)
		}

		// cut でないプロファイルを 1 つ含めば通る（片側だけ見ていないことの確認）。
		raw, _ = json.Marshal(map[string]any{"name": "cut-plus-h264", "encodeProfiles": []string{"cut", "h264"}})
		resp2, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp2.Body.Close() }()
		if resp2.StatusCode != http.StatusCreated {
			t.Fatalf("cut+h264 rule status = %d, want 201", resp2.StatusCode)
		}
	})

	// 2. 予約 overrides（PATCH /api/sites/{site}/programs/{programId}/overrides）。
	t.Run("overrides", func(t *testing.T) {
		const programID int64 = 2100000110011234
		ruleID := insertRuleFixture(t, pool, ctx)
		insertReservationDirect(t, pool, ctx, programID, &ruleID, 21000, 2100)
		resp := doPatch(t, srv, overridesPath(programID),
			`{"encodeProfiles":["cut"],"keepOriginal":"until_encoded"}`)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cut-only overrides status = %d, want 400", resp.StatusCode)
		}
		resp = doPatch(t, srv, overridesPath(programID),
			`{"encodeProfiles":["cut","h264"],"keepOriginal":"until_encoded"}`)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("cut+h264 overrides status = %d, want 204", resp.StatusCode)
		}
	})

	// 3. 録画への事後追加（POST /api/recordings/{id}/encode-profiles）。
	t.Run("append", func(t *testing.T) {
		id := insertIngestedRecordingFixture(t, pool, ctx)
		raw, _ := json.Marshal(map[string]any{"profiles": []string{"cut"}})
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encode-profiles",
			"application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("cut-only append status = %d, want 400", resp.StatusCode)
		}
		// cut でないプロファイルは通る（この経路が死んでいないことの確認）。
		raw, _ = json.Marshal(map[string]any{"profiles": []string{"h264"}})
		resp2, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encode-profiles",
			"application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp2.Body.Close() }()
		if resp2.StatusCode != http.StatusNoContent {
			t.Fatalf("h264 append status = %d, want 204", resp2.StatusCode)
		}
		raw, _ = json.Marshal(map[string]any{"profiles": []string{"cut"}})
		resp3, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encode-profiles",
			"application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp3.Body.Close() }()
		if resp3.StatusCode != http.StatusNoContent {
			t.Fatalf("cut append after h264 status = %d, want 204", resp3.StatusCode)
		}
	})
}

// TestCutProfileSelection_AllowedOnEveryAPIPathWithLive は live.enabled=true の構成で
// 原本 HLS を確認に使えるため、cut だけのルール / override / 追記 API が通ることを
// 固定する。live 無効へ条件が固定されると、このテストが失敗する。
func TestCutProfileSelection_AllowedOnEveryAPIPathWithLive(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
		LiveEnabled:        true,
	}))
	t.Cleanup(srv.Close)

	t.Run("rule", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"name": "cut-only-live", "encodeProfiles": []string{"cut"}})
		resp, err := http.Post(srv.URL+"/api/rules", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("cut-only rule status = %d, want 201", resp.StatusCode)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		const programID int64 = 2100000110011235
		ruleID := insertRuleFixture(t, pool, ctx)
		insertReservationDirect(t, pool, ctx, programID, &ruleID, 21000, 2101)
		resp := doPatch(t, srv, overridesPath(programID),
			`{"encodeProfiles":["cut"],"keepOriginal":"until_encoded"}`)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("cut-only overrides status = %d, want 204", resp.StatusCode)
		}
	})

	t.Run("append", func(t *testing.T) {
		id := insertIngestedRecordingFixture(t, pool, ctx)
		raw, _ := json.Marshal(map[string]any{"profiles": []string{"cut"}})
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encode-profiles",
			"application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("cut-only append status = %d, want 204", resp.StatusCode)
		}
	})
}

// insertIngestedRecordingFixture は原本 media_asset まで持つ録画を 1 件作る
// （事後追加 API は原本が active でないと 409 になる）。
func insertIngestedRecordingFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO recordings (
  source, site, network_id, service_id, event_id,
  service_name, channel_type, channel, title,
  program_start_at, program_duration_ms, status
) VALUES ('manual', 'default', 21000, 2100, (SELECT COALESCE(max(event_id), 99) + 1 FROM recordings), 'テスト局', 'GR', '27',
          'カット検証', now() - interval '1 day', 1800000, 'finished')
RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("inserting recording fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1::bigint, 'original', 'cut-validation/original-' || $1::bigint::text || '.m2ts', 1024)`, id); err != nil {
		t.Fatalf("inserting original media_asset: %v", err)
	}
	return id
}

// TestAddEncodeProfiles_CutRuleAppliesToMergedSelection は事後追加の cut 規則が
// 「追加分だけ」ではなく「既存 ∪ 追加分」に当たることを両方向で固定する。
func TestAddEncodeProfiles_CutRuleAppliesToMergedSelection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	add := func(id int64, profile string) int {
		raw, _ := json.Marshal(map[string]any{"profiles": []string{profile}})
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encode-profiles",
			"application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	withPolicy := func(profiles string) int64 {
		id := insertIngestedRecordingFixture(t, pool, ctx)
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'always', $2::text[])`,
			id, profiles); err != nil {
			t.Fatal(err)
		}
		return id
	}

	if got := add(withPolicy(`{h264}`), "cut"); got != http.StatusNoContent {
		t.Errorf("[h264] + cut status = %d, want 204", got)
	}
	if got := add(withPolicy(`{cut}`), "cut"); got != http.StatusBadRequest {
		t.Errorf("[cut] + cut status = %d, want 400", got)
	}
	if got := add(withPolicy(`{}`), "cut"); got != http.StatusBadRequest {
		t.Errorf("[] + cut status = %d, want 400", got)
	}
}

// seedDesiredEncodeProfiles は録画の凍結済み desired を作る（reencode は desired に
// 無い profile を 409 にする）。
func seedDesiredEncodeProfiles(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id int64, profiles string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles) VALUES ($1, 'always', $2::text[])`,
		id, profiles); err != nil {
		t.Fatal(err)
	}
}

// TestReencodeRecordingProfile_StatusCodes は cut の作り直し endpoint が本番構成
// （CutProfileNames あり）で 204 になり、未知名・cut でない名前は 400 になることを固定する。
// cut 1 つのリストを「cut だけの選択」として弾く実装だと常に 400 になる。
func TestReencodeRecordingProfile_StatusCodes(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool, ctx)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{h264,cut}`)
	// 確認済みで一部だけ切る録画（作り直せる状態）。
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}

	post := func(profile string) int {
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/"+profile+"/reencode", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	if got := post("cut"); got != http.StatusNoContent {
		t.Errorf("reencode cut status = %d, want 204", got)
	}
	if got := post("nosuch"); got != http.StatusBadRequest {
		t.Errorf("reencode unknown status = %d, want 400", got)
	}
	if got := post("h264"); got != http.StatusBadRequest {
		t.Errorf("reencode non-cut status = %d, want 400", got)
	}
}

func TestReencodeRecordingProfile_EnqueuesEncodeRebuild(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool, ctx)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{h264,cut}`)
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reencode status = %d, want 204", resp.StatusCode)
	}
	var rebuilds int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'encode_rebuild'
		 AND (args->>'recording_id')::bigint = $1 AND args->>'profile' = 'cut'`, id,
	).Scan(&rebuilds); err != nil {
		t.Fatalf("counting encode_rebuild jobs: %v", err)
	}
	if rebuilds != 1 {
		t.Errorf("encode_rebuild jobs = %d, want 1", rebuilds)
	}
	var encodes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'encode'
		 AND (args->>'recording_id')::bigint = $1`, id,
	).Scan(&encodes); err != nil {
		t.Fatalf("counting encode jobs: %v", err)
	}
	if encodes != 0 {
		t.Errorf("encode jobs before worker handles rebuild = %d, want 0", encodes)
	}
}

// TestReencodeRecordingProfile_NeedsKeepRanges は作り直しが「確認済みで keep が空でない」
// 録画にしか 204 を返さないことを固定する。未確認・全区間カットで 204 を返すと、worker が
// 投入しないのでボタンが黙って効かず cutStale も残る。
func TestReencodeRecordingProfile_NeedsKeepRanges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	seed := func(owned bool, cutFrom, cutTo int64) int64 {
		id := insertIngestedRecordingFixture(t, pool, ctx)
		seedDesiredEncodeProfiles(t, pool, ctx, id, `{h264,cut}`)
		if owned {
			if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
				t.Fatal(err)
			}
		}
		if cutTo > 0 {
			if _, err := pool.Exec(ctx,
				`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), 'CM', true)`,
				id, cutFrom, cutTo); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	post := func(id int64) int {
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	// fixture の番組長は 1800000ms。ちょうどまで cut すると終端が手前のフレーム境界へ
	// 丸まって 1ms の本編が残る（KeepRanges が落とす）。番組長を越える終端も見る。
	if got := post(seed(false, 0, 0)); got != http.StatusConflict {
		t.Errorf("unconfirmed status = %d, want 409", got)
	}
	if got := post(seed(true, 0, 1800000)); got != http.StatusConflict {
		t.Errorf("cut to exactly the program length status = %d, want 409", got)
	}
	if got := post(seed(true, 0, chapters.QuantizeMs(1810000))); got != http.StatusConflict {
		t.Errorf("all-cut status = %d, want 409", got)
	}
	if got := post(seed(true, 600000, 900000)); got != http.StatusNoContent {
		t.Errorf("partially cut status = %d, want 204", got)
	}
}

// TestReencodeRecordingProfile_NotDesiredIs409 は desired に無い cut profile の作り直しが
// 409 で、encode_rebuild を積まないことを固定する（worker は公開直前に捨てるだけなので、
// 積むと ffmpeg を回した後に黙って無駄になる）。policy 行が無い録画も同じ。
func TestReencodeRecordingProfile_NotDesiredIs409(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	riverClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Pool:               pool,
		RiverClient:        riverClient,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	for name, desired := range map[string]string{"cut not desired": `{h264}`, "no policy row": ""} {
		id := insertIngestedRecordingFixture(t, pool, ctx)
		if desired != "" {
			seedDesiredEncodeProfiles(t, pool, ctx, id, desired)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409", name, resp.StatusCode)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'encode_rebuild'
			AND (args->>'recording_id')::bigint = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: encode_rebuild jobs = %d, want 0", name, n)
		}
	}
}
