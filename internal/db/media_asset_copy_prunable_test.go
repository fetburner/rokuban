package db_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// prunableCutoff は overlap の境界（SQL 関数の p_overlap_cutoff）。固定値にして、created_at との比較を決定的にする。
var prunableCutoff = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// prunableDesiredStore は希望先の置き場。原本なので、派生物の置き場 p_derived_store は判定に使われない。
const prunableDesiredStore = "desired-store"

// prunableRules は原本を prunableDesiredStore へ置く規則（先頭の規則が勝つ）。
const prunableRules = `[{"store":"desired-store"}]`

// prunableSameStoreRules は原本を退く側と同じ置き場（dbtest.StoreName）へ置く規則。
func prunableSameStoreRules() string {
	return `[{"store":"` + dbtest.StoreName + `"}]`
}

// prunableFixture は、退く側のコピー（置き場 dbtest.StoreName）と、希望先の置き場の現在コピーを持つ。
type prunableFixture struct {
	assetID     int64
	recordingID int64
	retiringID  int64
	desiredID   int64
}

// seedPrunableComplete は、配置の腕で退いてよい原本を 1 件作る。
// 退く側は active・現在世代・録画が生きている。希望先は cutoff より前に作られた active の現在コピー。
// ケースはこの状態から条件を 1 つ崩す。
func seedPrunableComplete(t *testing.T, pool *pgxpool.Pool) prunableFixture {
	t.Helper()
	f := prunableFixture{recordingID: dbtest.Recording(t, pool)}
	f.assetID = dbtest.MediaAsset(t, pool, f.recordingID)
	testutil.MustExec(t, pool, "aging the retiring copy before the cutoff",
		`UPDATE media_asset_copies SET created_at = $2 WHERE media_asset_id = $1`, f.assetID, prunableCutoff.Add(-2*time.Hour))
	old := prunableCutoff.Add(-2 * time.Hour)
	dbtest.MediaAssetCopy(t, pool, f.assetID, func(p *dbtest.MediaAssetCopyParams) {
		p.Store = prunableDesiredStore
		p.RelPath = fmt.Sprintf("desired/%d.ts", f.assetID)
		p.CreatedAt = &old
	})
	f.retiringID = dbtest.MediaAssetCopyID(t, pool, f.assetID, dbtest.StoreName)
	f.desiredID = dbtest.MediaAssetCopyID(t, pool, f.assetID, prunableDesiredStore)
	return f
}

// setPrunableDesiredCreatedAt は希望先の現在コピーの created_at を変える。
func setPrunableDesiredCreatedAt(at time.Time) func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
	return func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
		testutil.MustExec(t, pool, "setting desired copy created_at",
			`UPDATE media_asset_copies SET created_at = $2 WHERE id = $1`, f.desiredID, at)
	}
}

// setPrunableRetiringState は退く側のコピーの state を変える。deleted_at は deleted のときだけ付ける。
func setPrunableRetiringState(state string) func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
	return func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
		testutil.MustExec(t, pool, "setting retiring copy state",
			`UPDATE media_asset_copies SET state = $2, deleted_at = CASE WHEN $2 = 'deleted' THEN now() ELSE NULL END WHERE id = $1`,
			f.retiringID, state)
	}
}

// mediaAssetCopyPrunable は SQL 関数 media_asset_copy_prunable の戻り値を返す。
func mediaAssetCopyPrunable(t *testing.T, pool *pgxpool.Pool, copyID int64, rules string) bool {
	t.Helper()
	var ok bool
	testutil.RequireNoError(t, pool.QueryRow(t.Context(),
		`SELECT media_asset_copy_prunable($1, $2::jsonb, $3, $4)`,
		copyID, rules, prunableDesiredStore, prunableCutoff).Scan(&ok), "querying media_asset_copy_prunable")
	return ok
}

// TestMediaAssetCopyPrunable は、配置の腕で退くコピーを削除してよいかの判定（SQL 関数 media_asset_copy_prunable）を、
// 条件 1 つずつの表で固定する。各ケースは完備なコピー（対象）から条件を 1 つ崩し、戻り値を見る。
// 同じ状態の対照（崩さない）を別に 1 件作り、それは毎回 true であることも確かめる。
// 対照が true にならなければ、関数が常に false を返す変異を、対象の false だけで見分けられないためである。
func TestMediaAssetCopyPrunable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rules  string
		mutate func(t *testing.T, pool *pgxpool.Pool, f prunableFixture)
		want   bool
	}{
		{name: "complete original is prunable", rules: prunableRules, want: true},
		{name: "desired copy created exactly at the cutoff is prunable", rules: prunableRules, want: true,
			mutate: setPrunableDesiredCreatedAt(prunableCutoff)},
		{name: "desired copy created after the cutoff is not prunable", rules: prunableRules,
			mutate: setPrunableDesiredCreatedAt(prunableCutoff.Add(time.Second))},
		{name: "desired copy marked missing is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "marking desired copy missing",
				`INSERT INTO missing_media_assets (media_asset_copy_id) VALUES ($1)`, f.desiredID)
		}},
		{name: "no desired copy is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "removing desired copy",
				`DELETE FROM media_asset_copies WHERE id = $1`, f.desiredID)
		}},
		{name: "desired copy only in another store is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "moving desired copy to another store",
				`UPDATE media_asset_copies SET store = 'other-store' WHERE id = $1`, f.desiredID)
		}},
		{name: "desired copy of a non-current generation is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "making desired copy stale",
				`UPDATE media_asset_copies SET generation = 1 WHERE id = $1`, f.desiredID)
		}},
		// 退く側を希望先と同じ置き場にすると、存在の判定は通る（退く側のコピー自身が数える）。
		// 残るのは置き場が違うことの判定だけになる。
		{name: "desired store equal to the retiring store is not prunable", rules: prunableSameStoreRules()},
		{name: "NULL desired store is not prunable", rules: `[]`},
		{name: "deleting retiring copy is prunable", rules: prunableRules, want: true,
			mutate: setPrunableRetiringState("deleting")},
		{name: "pending retiring copy is not prunable", rules: prunableRules,
			mutate: setPrunableRetiringState("pending")},
		{name: "deleted retiring copy is not prunable", rules: prunableRules,
			mutate: setPrunableRetiringState("deleted")},
		// 退く側が現在世代（原本は 0）でなければ、現在世代の希望先があっても退かない。原本の世代は 0 に固定されるので、新しい世代の行で表す。
		{name: "retiring copy of a non-current generation is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "making retiring copy an older generation",
				`UPDATE media_asset_copies SET generation = 1 WHERE id = $1`, f.retiringID)
		}},
		{name: "retiring copy of a trashed recording is not prunable", rules: prunableRules, mutate: func(t *testing.T, pool *pgxpool.Pool, f prunableFixture) {
			testutil.MustExec(t, pool, "trashing recording",
				`UPDATE recordings SET deleted_at = now() WHERE id = $1`, f.recordingID)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			f := seedPrunableComplete(t, pool)
			control := seedPrunableComplete(t, pool)
			if tc.mutate != nil {
				tc.mutate(t, pool, f)
			}
			if got := mediaAssetCopyPrunable(t, pool, control.retiringID, prunableRules); !got {
				t.Fatalf("control copy %d is not prunable, want prunable (the fixture itself is broken)", control.retiringID)
			}
			if got := mediaAssetCopyPrunable(t, pool, f.retiringID, tc.rules); got != tc.want {
				t.Fatalf("media_asset_copy_prunable(%d) = %t, want %t (control copy %d)", f.retiringID, got, tc.want, control.retiringID)
			}
		})
	}
}
