package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
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
		Ready:              pool.Ping,
		RuleStore:          pgstore.NewRules(pool, []string{"default"}),
		ProgramSearch:      pgstore.NewEPG(pool),
		EncodePolicyStore:  pgstore.NewMedia(pool),
		ReservationStore:   pgstore.NewReservations(pool),
		MediaReader:        pgstore.NewMedia(pool),
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
		id := insertIngestedRecordingFixture(t, pool)
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
		Ready:              pool.Ping,
		RuleStore:          pgstore.NewRules(pool, []string{"default"}),
		ProgramSearch:      pgstore.NewEPG(pool),
		EncodePolicyStore:  pgstore.NewMedia(pool),
		ReservationStore:   pgstore.NewReservations(pool),
		MediaReader:        pgstore.NewMedia(pool),
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
		id := insertIngestedRecordingFixture(t, pool)
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
func insertIngestedRecordingFixture(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id := dbtest.Recording(t, pool, func(p *sqlcgen.CreateRecordingParams) {
		p.NetworkID = 21000
		p.ServiceID = 2100
		p.ServiceName = "テスト局"
		p.Title = "カット検証"
		p.ProgramStartAt = time.Now().Add(-24 * time.Hour)
	})
	dbtest.OriginalAsset(t, pool, id, func(p *sqlcgen.CreateMediaAssetParams) {
		p.RelPath = fmt.Sprintf("cut-validation/original-%d.m2ts", id)
		p.SizeBytes = 1024
	})
	return id
}

// TestAddEncodeProfiles_CutRuleAppliesToMergedSelection は事後追加の cut 規則が
// 「追加分だけ」ではなく「既存 ∪ 追加分」に当たることを両方向で固定する。
func TestAddEncodeProfiles_CutRuleAppliesToMergedSelection(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
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
		id := insertIngestedRecordingFixture(t, pool)
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
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool)
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

func TestReencodeRecordingProfile_AllowsTrashedRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	id := insertIngestedRecordingFixture(t, pool)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{h264,cut}`)
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("trashing recording: %v", err)
	}

	resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reencode status for trashed recording = %d, want 204", resp.StatusCode)
	}
	var requests int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1 AND profile = 'cut'`, id).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("rebuild requests for trashed recording = %d, want 1", requests)
	}
}

func TestReencodeRecordingProfile_RecordsRequestAndHintsReconcile(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		Enqueuer:           enqueuer,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{h264,cut}`)
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`, id); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		profile string
		state   string
	}{{profile: "cut", state: "failed"}, {profile: "h264", state: "running"}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO recording_encode_attempts (recording_id, profile, state, attempted_at, attempt_count)
			VALUES ($1, $2, $3, now(), 25)`, id, attempt.profile, attempt.state); err != nil {
			t.Fatalf("seeding %s attempt for %s: %v", attempt.state, attempt.profile, err)
		}
	}

	resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reencode status = %d, want 204", resp.StatusCode)
	}
	var requestedKeepJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(jsonb_agg(jsonb_build_object('startMs', lower(cut_range), 'endMs', upper(cut_range)) ORDER BY lower(cut_range)), '[]'::jsonb)
		FROM encoded_asset_rebuild_requests q
		CROSS JOIN LATERAL unnest(q.requested_keep) AS ranges(cut_range)
		WHERE q.recording_id = $1 AND q.profile = 'cut'`, id).Scan(&requestedKeepJSON); err != nil {
		t.Fatalf("loading committed rebuild request: %v", err)
	}
	var gotKeep []chapters.Range
	if err := json.Unmarshal(requestedKeepJSON, &gotKeep); err != nil {
		t.Fatalf("decoding committed rebuild keep: %v", err)
	}
	wantKeep := chapters.KeepRanges(chapters.Derive(true, []chapters.Span{{StartMs: 600000, EndMs: 900000, Label: "CM", Cut: true}}, nil, 1800000))
	if !chapters.SameRanges(gotKeep, wantKeep) {
		t.Fatalf("requested keep = %v, want current quantized keep %v", gotKeep, wantKeep)
	}
	var failedCutAttempts, runningH264Attempts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE profile = 'cut' AND state = 'failed'),
		       count(*) FILTER (WHERE profile = 'h264' AND state = 'running')
		FROM recording_encode_attempts WHERE recording_id = $1`, id).Scan(&failedCutAttempts, &runningH264Attempts); err != nil {
		t.Fatal(err)
	}
	if failedCutAttempts != 0 || runningH264Attempts != 1 {
		t.Errorf("failed cut attempts = %d and running h264 attempts = %d, want 0 and 1", failedCutAttempts, runningH264Attempts)
	}
	reconcileHints := enqueuer.CallsOfKind("encode_reconcile")
	if len(reconcileHints) != 1 {
		t.Fatalf("encode_reconcile hints = %d, want 1", len(reconcileHints))
	}
	if args, ok := reconcileHints[0].Args.(jobs.EncodeReconcileArgs); !ok || args.RecordingID != id {
		t.Fatalf("encode_reconcile hint = %#v, want targeted recording %d", reconcileHints[0].Args, id)
	}
	encodes := 0
	for _, call := range enqueuer.CallsOfKind("encode") {
		args, ok := call.Args.(jobs.EncodeJobArgs)
		if !ok {
			t.Fatalf("encode args = %T, want jobs.EncodeJobArgs", call.Args)
		}
		if args.RecordingID == id {
			encodes++
		}
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
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	seed := func(owned bool, cutFrom, cutTo int64) int64 {
		id := insertIngestedRecordingFixture(t, pool)
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
// 409 で、要求行を残さないことを固定する。policy 行が無い録画も同じ。
func TestReencodeRecordingProfile_NotDesiredIs409(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		Enqueuer:           enqueuer,
		EncodeProfileNames: []string{"h264", "cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)

	for name, desired := range map[string]string{"cut not desired": `{h264}`, "no policy row": ""} {
		id := insertIngestedRecordingFixture(t, pool)
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
		var requests int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM encoded_asset_rebuild_requests WHERE recording_id = $1`, id).Scan(&requests); err != nil {
			t.Fatalf("%s: counting rebuild requests: %v", name, err)
		}
		if requests != 0 {
			t.Errorf("%s: rebuild requests = %d, want 0", name, requests)
		}
	}
}

// TestEncodeQueue_ExhaustedRebuildRequestIsNotCountedUntilReencodeResetsBudget は、
// 試行上限に達した作り直し要求を待機列に数えず（worker が作らないため）、押し直し
// （API が failed 試行行を消す）の後は数えることを両方向で固定する。
func TestEncodeQueue_ExhaustedRebuildRequestIsNotCountedUntilReencodeResetsBudget(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: []string{"cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{cut}`)
	profile := "cut"
	dbtest.OriginalAsset(t, pool, id, func(p *sqlcgen.CreateMediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "queue-exhausted/cut.mp4"
		p.SizeBytes = 1000
	})
	for _, sql := range []string{
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(600000, 900000), 'CM', true)`,
		`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		 VALUES ($1, 'cut', '{[0,600000),[900000,1800000)}'::int8multirange)`,
		`INSERT INTO recording_encode_attempts (recording_id, profile, state, attempted_at, attempt_count)
		 VALUES ($1, 'cut', 'failed', now(), 25)`,
	} {
		if _, err := pool.Exec(ctx, sql, id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	queued := func() int64 {
		t.Helper()
		var got struct {
			Queued  int64 `json:"queued"`
			Running int64 `json:"running"`
		}
		resp, err := http.Get(srv.URL + "/api/encode-queue")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.Queued + got.Running
	}
	if got := queued(); got != 0 {
		t.Fatalf("queue with exhausted rebuild request = %d, want 0", got)
	}
	resp, err := http.Post(srv.URL+"/api/recordings/"+itoa(id)+"/encoded/cut/reencode", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reencode status = %d, want 204", resp.StatusCode)
	}
	if got := queued(); got != 1 {
		t.Fatalf("queue after reencode reset the budget = %d, want 1", got)
	}
}

// TestEncodeQueue_RebuildRequestWithoutActiveOriginalIsNotCounted は、原本が active でない録画の
// 作り直し要求を待機列に数えず、active なら数えることを固定する。
func TestEncodeQueue_RebuildRequestWithoutActiveOriginalIsNotCounted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(api.NewRouter(api.RouterConfig{
		Ready:              pool.Ping,
		EncodePolicyStore:  pgstore.NewMedia(pool),
		MediaReader:        pgstore.NewMedia(pool),
		EncodeProfileNames: []string{"cut"},
		CutProfileNames:    []string{"cut"},
	}))
	t.Cleanup(srv.Close)
	id := insertIngestedRecordingFixture(t, pool)
	seedDesiredEncodeProfiles(t, pool, ctx, id, `{cut}`)
	profile := "cut"
	dbtest.OriginalAsset(t, pool, id, func(p *sqlcgen.CreateMediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "queue-noorig/cut.mp4"
		p.SizeBytes = 1000
	})
	for _, sql := range []string{
		`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`,
		`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
		 VALUES ($1, 'cut', '{[0,1800000)}'::int8multirange)`,
		`UPDATE media_assets SET state = 'deleting' WHERE recording_id = $1 AND kind = 'original'`,
	} {
		if _, err := pool.Exec(ctx, sql, id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	queued := func() int64 {
		t.Helper()
		var got struct {
			Queued  int64 `json:"queued"`
			Running int64 `json:"running"`
		}
		resp, err := http.Get(srv.URL + "/api/encode-queue")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.Queued + got.Running
	}
	if got := queued(); got != 0 {
		t.Fatalf("queue without active original = %d, want 0", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET state = 'active' WHERE recording_id = $1 AND kind = 'original'`, id); err != nil {
		t.Fatal(err)
	}
	if got := queued(); got != 1 {
		t.Fatalf("queue with active original = %d, want 1", got)
	}
}
