package testutil

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// assertJSONEqual は 2 つの JSON 値をデコードして意味的に比較する。jsonb 列は
// 格納時にキー順を正規化するため、テキストのままでは一致しないことがある。
func assertJSONEqual(t *testing.T, label string, got, want json.RawMessage) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: unmarshaling got JSON %s: %v", label, got, err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("%s: unmarshaling want JSON %s: %v", label, want, err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("%s = %s, want %s", label, got, want)
	}
}

// TestSetupDBPoolerCompat_ComplexQueries は SetupDBPoolerCompat が返すプール
// （db.NewPool 経由・PoolerCompat: true・QueryExecModeExec）上で、jsonb 列の
// 読み書きと配列パラメータ（`= ANY($1::bigint[])`）を使う既存クエリが壊れずに
// 動くことを確認する。
//
// issue #90 のレビュー指摘: 受け入れ基準「pooler 想定モードで既存テストスイートが
// 通る」は、レビュアーが手動で確認しただけで自動テストが無かった（既存の SetupDB は
// pgxpool.New を直接呼び、db.NewPool / buildPoolConfig の pooler_compat 経路を
// 経由しないため）。CI で全パッケージのテストを exec モードで二重に走らせる必要は
// ない（pooler_compat は api ロール専用のオプトイン設定）ので、ここでは
// 「拡張プロトコル固有の機能に依存していないか」が疑わしい代表的なクエリ
// （jsonb 列、配列パラメータ）だけを exec モードのプールで叩く回帰テストとして残す。
func TestSetupDBPoolerCompat_ComplexQueries(t *testing.T) {
	pool := SetupDBPoolerCompat(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	// SetupDBPoolerCompat が本当に pooler_compat（QueryExecModeExec）を有効にした
	// プールを返しているかを確認する。これが無いと、以下のクエリはこの返り値に
	// 関わらず通常モードでも通ってしまうため、実装の配線が抜けても検知できない。
	if mode := pool.Config().ConnConfig.DefaultQueryExecMode; mode != pgx.QueryExecModeExec {
		t.Fatalf("pool DefaultQueryExecMode = %v, want QueryExecModeExec (SetupDBPoolerCompat wiring broken)", mode)
	}

	site := "home"
	now := time.Now().Truncate(time.Microsecond)

	t.Run("basic SELECT (no extended-protocol-specific feature)", func(t *testing.T) {
		programID := int64(1001)
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site:       site,
			ProgramID:  programID,
			Title:      "テスト番組",
			StartAt:    now,
			DurationMs: 1800000,
		}); err != nil {
			t.Fatalf("UpsertProgramSnapshot: %v", err)
		}

		got, err := q.GetProgramSnapshot(ctx, sqlcgen.GetProgramSnapshotParams{Site: site, ProgramID: programID})
		if err != nil {
			t.Fatalf("GetProgramSnapshot: %v", err)
		}
		if got.Title != "テスト番組" {
			t.Errorf("Title = %q, want %q", got.Title, "テスト番組")
		}
	})

	t.Run("jsonb column round-trip", func(t *testing.T) {
		programID := int64(1002)
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site:       site,
			ProgramID:  programID,
			Title:      "jsonb テスト番組",
			StartAt:    now,
			DurationMs: 1800000,
		}); err != nil {
			t.Fatalf("UpsertProgramSnapshot: %v", err)
		}

		overrides := json.RawMessage(`{"encodeProfiles":["h264","h265"],"keepOriginal":"always"}`)
		upserted, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
			Site:      site,
			ProgramID: programID,
			Overrides: overrides,
		})
		if err != nil {
			t.Fatalf("UpsertProgramOverrides: %v", err)
		}
		// jsonb は格納時にキー順を正規化する（テキストとして一致するとは限らない）ため、
		// デコードして意味的に比較する。
		assertJSONEqual(t, "UpsertProgramOverrides", upserted.Overrides, overrides)

		got, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID})
		if err != nil {
			t.Fatalf("GetProgramOverrides: %v", err)
		}
		assertJSONEqual(t, "GetProgramOverrides", got.Overrides, overrides)
	})

	t.Run("array parameter (= ANY($1::bigint[]))", func(t *testing.T) {
		ids := []int64{2001, 2002, 2003}
		for _, id := range ids {
			if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
				Site:       site,
				ProgramID:  id,
				Title:      "配列パラメータテスト",
				StartAt:    now,
				DurationMs: 1800000,
			}); err != nil {
				t.Fatalf("UpsertProgramSnapshot(%d): %v", id, err)
			}
		}
		// 存在しない ID を混ぜて、絞り込みそのものが機能していることも確認する。
		queried := append(append([]int64{}, ids...), 9999999)

		got, err := q.ListProgramSnapshotProgramIDsBySiteAndProgramIDs(ctx,
			sqlcgen.ListProgramSnapshotProgramIDsBySiteAndProgramIDsParams{
				Site:       site,
				ProgramIds: queried,
			})
		if err != nil {
			t.Fatalf("ListProgramSnapshotProgramIDsBySiteAndProgramIDs: %v", err)
		}
		if len(got) != len(ids) {
			t.Fatalf("got %d program IDs, want %d (got=%v)", len(got), len(ids), got)
		}
		gotSet := make(map[int64]bool, len(got))
		for _, id := range got {
			gotSet[id] = true
		}
		for _, id := range ids {
			if !gotSet[id] {
				t.Errorf("expected program_id %d in result, got %v", id, got)
			}
		}
	})
}
