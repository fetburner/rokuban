package db_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// untilEncodedFixture は、判定の基準になる録画 1 件と、その録画の資産の ID を持つ。
type untilEncodedFixture struct {
	recordingID int64
	originalID  int64
	encodedID   int64
	thumbID     int64
	tilesID     int64
}

// seedUntilEncodedComplete は、原本の削除条件をすべて満たす録画を 1 件作る。
// 内訳は until_encoded・プロファイル h264 の encoded、サムネイル、シークタイル、
// 現在サイズの TS scan、CM 検出なし（cm_detect=false、CM 結果なし）。
// ケースはこの状態から条件を 1 つ崩す。
func seedUntilEncodedComplete(t *testing.T, pool *pgxpool.Pool) untilEncodedFixture {
	t.Helper()
	profile := "h264"
	f := untilEncodedFixture{recordingID: dbtest.Recording(t, pool)}
	f.originalID = dbtest.MediaAsset(t, pool, f.recordingID)
	f.encodedID = dbtest.MediaAsset(t, pool, f.recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
	})
	f.thumbID = dbtest.MediaAsset(t, pool, f.recordingID, func(p *dbtest.MediaAssetParams) { p.Kind = "thumbnail" })
	f.tilesID = dbtest.MediaAsset(t, pool, f.recordingID, func(p *dbtest.MediaAssetParams) { p.Kind = "seek_tiles" })
	testutil.MustExec(t, pool, "marking original as scanned", `
		INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
		SELECT id, size_bytes FROM media_assets WHERE id = $1`, f.originalID)
	testutil.MustExec(t, pool, "setting processing policy", `
		INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', $2, false)`, f.recordingID, []string{profile})
	return f
}

// removeMediaAsset は資産のコピーと資産の行を消す。
func removeMediaAsset(t *testing.T, pool *pgxpool.Pool, assetID int64) {
	t.Helper()
	testutil.MustExec(t, pool, "removing media asset copies", `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID)
	testutil.MustExec(t, pool, "removing media asset", `DELETE FROM media_assets WHERE id = $1`, assetID)
}

// untilEncodedOriginals は until_encoded_deletable_originals の全件の asset_id を、昇順で返す。
// desiredStore が nil なら NULL を渡す。録画で絞らないので、データベース内の全録画が対象になる。
func untilEncodedOriginals(t *testing.T, pool *pgxpool.Pool, desiredStore *string) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT asset_id FROM until_encoded_deletable_originals($1) ORDER BY asset_id`, desiredStore)
	testutil.RequireNoError(t, err, "querying until_encoded_deletable_originals")
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	testutil.RequireNoError(t, err, "collecting until_encoded_deletable_originals")
	return ids
}

// TestUntilEncodedDeletableOriginals は、原本を until_encoded で削除してよいかの判定
// （SQL 関数 until_encoded_deletable_originals）を、条件 1 つずつの表で固定する。
// 各ケースは完備な録画（対象）から条件を 1 つ崩し、返る原本の全件を見る。
// 同じ状態の対照録画（崩さない）を別に 1 件作り、それは毎回返ることを確かめる。
// 対照が返らなければ、関数そのものが空を返す変異（対象だけ崩れて見える）を見分けられないので、
// 対象の不在だけで通る偽の合格を防ぐ。
func TestUntilEncodedDeletableOriginals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mutate    func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture)
		nullStore bool
		want      bool
	}{
		{name: "complete derivatives without cm detection qualify", want: true},
		{name: "deleted thumbnail does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			dbtest.SetMediaAssetState(t, pool, f.thumbID, "deleted")
		}},
		{name: "deleted seek tiles do not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			dbtest.SetMediaAssetState(t, pool, f.tilesID, "deleted")
		}},
		{name: "missing encoded profile does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			removeMediaAsset(t, pool, f.encodedID)
		}},
		{name: "encoded profile only in another store does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "moving encoded copy to another store",
				`UPDATE media_asset_copies SET store = 'other-store' WHERE media_asset_id = $1`, f.encodedID)
		}},
		{name: "encoded of another profile does not satisfy the desired profile", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "relabeling encoded profile",
				`UPDATE media_assets SET profile = 'hevc' WHERE id = $1`, f.encodedID)
		}},
		{name: "missing thumbnail does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			removeMediaAsset(t, pool, f.thumbID)
		}},
		{name: "missing seek tiles do not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			removeMediaAsset(t, pool, f.tilesID)
		}},
		{name: "empty encode profiles do not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			// 旧判定は「欠けたプロファイルが無い」と恒真になり、原本を消していた。
			// CHECK 制約は通常これを阻むので、外してから作る。
			testutil.MustExec(t, pool, "dropping processing policy check",
				`ALTER TABLE recording_processing_policy DROP CONSTRAINT recording_processing_policy_check`)
			testutil.MustExec(t, pool, "emptying encode profiles",
				`UPDATE recording_processing_policy SET encode_profiles = '{}' WHERE recording_id = $1`, f.recordingID)
			removeMediaAsset(t, pool, f.encodedID)
		}},
		{name: "missing TS scan marker does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "removing TS scan marker",
				`DELETE FROM media_asset_ts_scans WHERE media_asset_id = $1`, f.originalID)
		}},
		{name: "TS scan of a different size does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "making TS scan marker stale",
				`UPDATE media_asset_ts_scans SET scanned_size_bytes = scanned_size_bytes - 1 WHERE media_asset_id = $1`, f.originalID)
		}},
		{name: "pending cut rebuild does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requesting cut rebuild",
				`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
				 VALUES ($1, 'h264', '{[0,1000)}'::int8multirange)`, f.recordingID)
		}},
		{name: "cut frozen with a different keep does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requesting cut rebuild",
				`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
				 VALUES ($1, 'h264', '{[0,1000)}'::int8multirange)`, f.recordingID)
			testutil.MustExec(t, pool, "freezing a different keep",
				`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, '{[0,500)}'::int8multirange)`, f.encodedID)
		}},
		{name: "cut frozen with the requested keep qualifies", want: true, mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requesting cut rebuild",
				`INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
				 VALUES ($1, 'h264', '{[0,1000)}'::int8multirange)`, f.recordingID)
			testutil.MustExec(t, pool, "freezing the requested keep",
				`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, f.encodedID)
		}},
		{name: "keep_original always does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "switching to always",
				`UPDATE recording_processing_policy SET keep_original = 'always' WHERE recording_id = $1`, f.recordingID)
		}},
		{name: "trashed recording does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "trashing recording",
				`UPDATE recordings SET deleted_at = now() WHERE id = $1`, f.recordingID)
		}},
		{name: "NULL desired store does not qualify", nullStore: true},
		{name: "cm detection required and none recorded does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requiring cm detection",
				`UPDATE recording_processing_policy SET cm_detect = true WHERE recording_id = $1`, f.recordingID)
		}},
		{name: "cm detection recorded qualifies", want: true, mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requiring cm detection",
				`UPDATE recording_processing_policy SET cm_detect = true WHERE recording_id = $1`, f.recordingID)
			testutil.MustExec(t, pool, "recording cm detection",
				`INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, f.recordingID)
		}},
		{name: "cm attempt failed at match qualifies", want: true, mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requiring cm detection",
				`UPDATE recording_processing_policy SET cm_detect = true WHERE recording_id = $1`, f.recordingID)
			testutil.MustExec(t, pool, "recording failed cm attempt",
				`INSERT INTO recording_cm_attempts (recording_id, state, stage, policy_generation) VALUES ($1, 'failed', 'match', 0)`, f.recordingID)
		}},
		{name: "cm attempt failed at adopt does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requiring cm detection",
				`UPDATE recording_processing_policy SET cm_detect = true WHERE recording_id = $1`, f.recordingID)
			testutil.MustExec(t, pool, "recording failed cm attempt at adopt",
				`INSERT INTO recording_cm_attempts (recording_id, state, stage, policy_generation) VALUES ($1, 'failed', 'adopt', 0)`, f.recordingID)
		}},
		{name: "cm attempt still running does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f untilEncodedFixture) {
			testutil.MustExec(t, pool, "requiring cm detection",
				`UPDATE recording_processing_policy SET cm_detect = true WHERE recording_id = $1`, f.recordingID)
			testutil.MustExec(t, pool, "recording running cm attempt",
				`INSERT INTO recording_cm_attempts (recording_id, state, stage, policy_generation) VALUES ($1, 'running', 'match', 0)`, f.recordingID)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			f := seedUntilEncodedComplete(t, pool)
			control := seedUntilEncodedComplete(t, pool)
			if tc.mutate != nil {
				tc.mutate(t, pool, f)
			}
			var store *string
			if !tc.nullStore {
				s := dbtest.StoreName
				store = &s
			}
			got := untilEncodedOriginals(t, pool, store)
			// 対照は崩していないので、NULL の置き場以外では必ず返る。
			want := []int64{}
			if !tc.nullStore {
				want = append(want, control.originalID)
				if tc.want {
					want = append(want, f.originalID)
				}
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("until_encoded_deletable_originals returned %v, want %v (control recording %d, case recording %d)",
					got, want, control.recordingID, f.recordingID)
			}
		})
	}
}
