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
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestCutProfileSelection_RejectedOnEveryAPIPath は受け入れの「cut のみのルール /
// override / 追記 API が拒否される」を API の 3 経路ぶん固定する（4 経路目は
// ingest の凍結で、internal/worker のテストが押さえる）。
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
) VALUES ('manual', 'default', 21000, 2100, 99, 'テスト局', 'GR', '27',
          'カット検証', now() - interval '1 day', 1800000, 'finished')
RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("inserting recording fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', 'cut-validation/original.m2ts', 1024)`, id); err != nil {
		t.Fatalf("inserting original media_asset: %v", err)
	}
	return id
}
