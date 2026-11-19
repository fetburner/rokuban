package db_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// encodableFixture は、判定の基準になる録画 1 件と、その原本・encoded の資産の ID を持つ。
type encodableFixture struct {
	recordingID int64
	originalID  int64
	encodedID   int64
}

// seedEncodable は、原本の active コピーと encoded の active コピーを持つ録画を 1 件作る。
// ケースはこの状態から条件を 1 つ崩す。
func seedEncodable(t *testing.T, pool *pgxpool.Pool) encodableFixture {
	t.Helper()
	profile := "h264"
	f := encodableFixture{recordingID: dbtest.Recording(t, pool)}
	f.originalID = dbtest.MediaAsset(t, pool, f.recordingID)
	f.encodedID = dbtest.MediaAsset(t, pool, f.recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
	})
	return f
}

// encodableRecordings は encodable_recordings の全件の recording_id を、昇順で返す。
// 録画で絞らないので、データベース内の全録画が対象になる。
func encodableRecordings(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT recording_id FROM encodable_recordings ORDER BY recording_id`)
	testutil.RequireNoError(t, err, "querying encodable_recordings")
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	testutil.RequireNoError(t, err, "collecting encodable_recordings")
	return ids
}

// TestEncodableRecordings は、エンコードしてよい録画の判定（SQL ビュー encodable_recordings）を、
// 条件 1 つずつの表で固定する。各ケースは完備な録画（対象）から条件を 1 つ崩し、
// 返る録画の全件を見る。崩さない対照録画を別に 1 件作り、それは毎回返ることを確かめる。
// 対照が返らなければ、ビューが空を返す変異を見分けられないので、対象の不在だけで通る偽の合格を防ぐ。
func TestEncodableRecordings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, pool *pgxpool.Pool, f encodableFixture)
		want   bool
	}{
		{name: "active original copy qualifies", want: true},
		{name: "trashed recording does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f encodableFixture) {
			testutil.MustExec(t, pool, "trashing recording",
				`UPDATE recordings SET deleted_at = now() WHERE id = $1`, f.recordingID)
		}},
		{name: "original copy in deleting state does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f encodableFixture) {
			dbtest.SetMediaAssetState(t, pool, f.originalID, "deleting")
		}},
		{name: "original without any copy does not qualify even with an active encoded copy", mutate: func(t *testing.T, pool *pgxpool.Pool, f encodableFixture) {
			testutil.MustExec(t, pool, "removing original copies",
				`DELETE FROM media_asset_copies WHERE media_asset_id = $1`, f.originalID)
		}},
		{name: "active encoded copy without an original does not qualify", mutate: func(t *testing.T, pool *pgxpool.Pool, f encodableFixture) {
			removeMediaAsset(t, pool, f.originalID)
		}},
		{name: "recording without its own original does not qualify while another recording has one", mutate: func(t *testing.T, pool *pgxpool.Pool, f encodableFixture) {
			removeMediaAsset(t, pool, f.originalID)
			removeMediaAsset(t, pool, f.encodedID)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			f := seedEncodable(t, pool)
			control := seedEncodable(t, pool)
			if tc.mutate != nil {
				tc.mutate(t, pool, f)
			}
			got := encodableRecordings(t, pool)
			// 対照は崩していないので必ず返る。対象は want のときだけ返る。
			want := []int64{control.recordingID}
			if tc.want {
				want = append(want, f.recordingID)
				slices.Sort(want)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("encodable_recordings = %v, want %v (target %d, control %d)", got, want, f.recordingID, control.recordingID)
			}
		})
	}
}
