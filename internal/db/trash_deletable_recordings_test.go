package db_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestTrashDeletableRecordings は、ごみ箱の削除資格を SQL 関数で直接検査する。
// PurgeRequestedImmediate_Deletes は deleted_at を 30 日の猶予内の「たった今」に置き、
// 即時削除の要求行単独で猶予を飛ばせることを確かめる。
// 述語の即時削除側を時刻比較に戻すと、対象は結果に含まれずこのケースが失敗する。
func TestTrashDeletableRecordings(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	past := cutoff.Add(-time.Hour)
	for _, tc := range []struct {
		name         string
		deletedAt    *time.Time
		purgeRequest bool
		want         bool
	}{
		{name: "TrashPastRetention_Deletes", deletedAt: &past, purgeRequest: false, want: true},
		{name: "TrashAtCutoff_Deletes", deletedAt: &cutoff, purgeRequest: false, want: true},
		{name: "TrashWithinRetention_NotDeleted", deletedAt: &now, purgeRequest: false, want: false},
		{name: "PurgeRequestedImmediate_Deletes", deletedAt: &now, purgeRequest: true, want: true},
		{name: "PurgeRequestedOutsideTrash_NotDeleted", deletedAt: nil, purgeRequest: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			target := dbtest.Recording(t, pool)
			control := dbtest.Recording(t, pool)
			testutil.MustExec(t, pool, "setting target deleted_at",
				"UPDATE recordings SET deleted_at = $1 WHERE id = $2", tc.deletedAt, target)
			if tc.purgeRequest {
				testutil.MustExec(t, pool, "requesting immediate purge for target",
					"INSERT INTO recording_purge_requests (recording_id) VALUES ($1)", target)
			}
			// 対照は猶予超過かつ要求あり。片方の条件を外しても対照は返る。
			// 猶予内・要求なしの対象は、対照の要求があっても返らないことを確かめる。
			testutil.MustExec(t, pool, "setting control deleted_at",
				"UPDATE recordings SET deleted_at = $1 WHERE id = $2", past, control)
			testutil.MustExec(t, pool, "requesting immediate purge for control",
				"INSERT INTO recording_purge_requests (recording_id) VALUES ($1)", control)

			rows, err := pool.Query(t.Context(),
				"SELECT recording_id FROM trash_deletable_recordings($1) ORDER BY recording_id", cutoff)
			testutil.RequireNoError(t, err, "querying trash_deletable_recordings")
			got, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			testutil.RequireNoError(t, err, "collecting trash_deletable_recordings")

			want := []int64{control}
			if tc.want {
				want = append(want, target)
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("trash_deletable_recordings returned %v, want %v (target %d, control %d)", got, want, target, control)
			}
		})
	}
}
