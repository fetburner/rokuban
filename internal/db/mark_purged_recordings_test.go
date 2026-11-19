package db_test

import (
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestMarkPurgedRecordings は、完全削除の確定（MarkPurgedRecordings の WHERE）を直接検査する。
// 録画が完全削除されたと確定するのは、ごみ箱の削除資格を満たし、かつ state <> 'deleted' の
// コピーを持つ media_assets が 1 件もないときだけである。deleting は unlink 待ちなので「消えた」に数えない。
// 対照は毎回、資産が 0 件で猶予超過の録画を置く。対照だけが返り、対象が返らないケースで
// 空虚な合格を防ぐ。
func TestMarkPurgedRecordings(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	past := cutoff.Add(-time.Hour)
	for _, tc := range []struct {
		name      string
		deletedAt time.Time
		// assetStates は対象録画の資産ごとのコピー状態。nil は資産 0 件。
		assetStates []string
		want        bool
	}{
		{name: "ZeroAssetRecording_Purged", deletedAt: past, assetStates: nil, want: true},
		{name: "AllAssetsDeleted_Purged", deletedAt: past, assetStates: []string{"deleted", "deleted"}, want: true},
		{name: "DeletingAssetStuck_NotPurged", deletedAt: past, assetStates: []string{"deleting"}, want: false},
		{name: "LastAssetStillDeleting_NotPurged", deletedAt: past, assetStates: []string{"deleted", "deleting"}, want: false},
		{name: "ActiveAsset_NotPurged", deletedAt: past, assetStates: []string{"active"}, want: false},
		{name: "ZeroAssetWithinRetention_NotPurged", deletedAt: now, assetStates: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			target := dbtest.Recording(t, pool)
			control := dbtest.Recording(t, pool)
			testutil.MustExec(t, pool, "setting target deleted_at",
				"UPDATE recordings SET deleted_at = $1 WHERE id = $2", tc.deletedAt, target)
			for i, state := range tc.assetStates {
				// 2 件目以降は原本を重ねられないので thumbnail にする（一意制約は kind と profile）。
				assetID := dbtest.MediaAsset(t, pool, target, func(p *dbtest.MediaAssetParams) {
					if i > 0 {
						p.Kind = "thumbnail"
					}
				})
				dbtest.SetMediaAssetState(t, pool, assetID, state)
			}
			testutil.MustExec(t, pool, "setting control deleted_at",
				"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, control)

			rows, err := sqlcgen.New(pool).MarkPurgedRecordings(t.Context(), cutoff)
			testutil.RequireNoError(t, err, "MarkPurgedRecordings")
			got := make([]int64, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.ID)
			}
			slices.Sort(got)

			want := []int64{control}
			if tc.want {
				want = append(want, target)
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("MarkPurgedRecordings purged %v, want %v (target %d, control %d)", got, want, target, control)
			}
		})
	}
}
