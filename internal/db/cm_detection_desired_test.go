package db_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// cmDesiredFixture は、CM 検出の対象になる録画 1 件と、その原本の ID を持つ。
type cmDesiredFixture struct {
	recordingID int64
	originalID  int64
}

// seedCMDesiredComplete は、cm_detection_desired に入る録画を 1 件作る。
// 内訳は cm_detect が有効（方針世代は dbtest の既定の 0）、Kind "original" で
// 現存（active）のコピーを持つ原本、CM 結果も試行も無い、の 3 点。
// ケースはこの状態から条件を 1 つ崩す。
func seedCMDesiredComplete(t *testing.T, pool *pgxpool.Pool) cmDesiredFixture {
	t.Helper()
	f := cmDesiredFixture{recordingID: dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = 32736
		p.ServiceID = 1024
	})}
	f.originalID = dbtest.MediaAsset(t, pool, f.recordingID, func(p *dbtest.MediaAssetParams) { p.Kind = "original" })
	dbtest.ProcessingPolicy(t, pool, f.recordingID, func(p *dbtest.ProcessingPolicyParams) { p.CMDetect = true })
	return f
}

// insertCMAttempt は録画 recordingID に失敗または再試行の試行を 1 件書く。方針世代は
// 録画の recording_processing_policy から読むので、世代の既定値に依らない。
// attemptedAgo が空なら now()、そうでなければ now() - attemptedAgo（SQL の interval）。
func insertCMAttempt(t *testing.T, pool *pgxpool.Pool, recordingID int64, state, attemptedAgo string) {
	t.Helper()
	at := "now()"
	if attemptedAgo != "" {
		at = "now() - interval '" + attemptedAgo + "'"
	}
	testutil.MustExec(t, pool, "recording cm attempt", `
		INSERT INTO recording_cm_attempts (recording_id, state, stage, policy_generation, attempted_at)
		SELECT recording_id, $2, NULL, cm_detect_generation, `+at+`
		FROM recording_processing_policy WHERE recording_id = $1`, recordingID, state)
}

// cmDesiredRecordings は cm_detection_desired の全件の recording_id を、昇順で返す。
func cmDesiredRecordings(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT recording_id FROM cm_detection_desired ORDER BY recording_id`)
	testutil.RequireNoError(t, err, "querying cm_detection_desired")
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	testutil.RequireNoError(t, err, "collecting cm_detection_desired")
	return ids
}

// TestCMDetectionDesired は、CM 検出の対象判定（SQL ビュー cm_detection_desired）を、
// 条件 1 つずつの表で固定する。各ケースは対象録画から条件を 1 つ崩す。
// 同じ状態の対照録画（崩さない）を別に 1 件作り、それは毎回入ることを確かめる。
// 対照が入らなければ、ビューが空を返す変異を対象の不在と見分けられないので、偽の合格になる。
func TestCMDetectionDesired(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture)
		want   bool
	}{
		{name: "recording without attempt is desired", want: true},
		{name: "cm detect disabled is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			testutil.MustExec(t, pool, "disabling cm detect",
				`UPDATE recording_processing_policy SET cm_detect = false WHERE recording_id = $1`, f.recordingID)
		}},
		{name: "trashed recording is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			testutil.MustExec(t, pool, "trashing recording",
				`UPDATE recordings SET deleted_at = now() WHERE id = $1`, f.recordingID)
		}},
		{name: "original with a missing copy is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			testutil.MustExec(t, pool, "marking original copy missing", `
				INSERT INTO missing_media_assets (media_asset_copy_id)
				SELECT copy.id FROM media_asset_copies copy WHERE copy.media_asset_id = $1`, f.originalID)
		}},
		{name: "original with a deleted copy is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			dbtest.SetMediaAssetState(t, pool, f.originalID, "deleted")
		}},
		{name: "recording with cm detections is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			testutil.MustExec(t, pool, "recording cm detections",
				`INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, '{[0,1000)}'::int8multirange)`, f.recordingID)
		}},
		{name: "retrying attempt is desired", want: true, mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			insertCMAttempt(t, pool, f.recordingID, "retrying", "")
		}},
		{name: "failed attempt without a learned logo is not desired", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			insertCMAttempt(t, pool, f.recordingID, "failed", "")
		}},
		{name: "failed attempt is desired once the logo is learned after it", want: true, mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			insertCMAttempt(t, pool, f.recordingID, "failed", "1 hour")
			testutil.MustExec(t, pool, "learning a logo now",
				`INSERT INTO cm_logos (network_id, service_id, lgd, learned_from, coded_width, coded_height)
				 VALUES (32736, 1024, 'lgd', $1, 1440, 1080)`, f.recordingID)
		}},
		{name: "logo for a different service does not wake a failed attempt", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			insertCMAttempt(t, pool, f.recordingID, "failed", "1 hour")
			testutil.MustExec(t, pool, "learning a logo now for another service on the same network",
				`INSERT INTO cm_logos (network_id, service_id, lgd, learned_from, coded_width, coded_height)
				 VALUES (32736, 1025, 'lgd', $1, 1440, 1080)`, f.recordingID)
		}},
		{name: "failed attempt stays undesired when the logo was learned before it", mutate: func(t *testing.T, pool *pgxpool.Pool, f cmDesiredFixture) {
			testutil.MustExec(t, pool, "learning a logo two hours ago",
				`INSERT INTO cm_logos (network_id, service_id, lgd, learned_at, learned_from, coded_width, coded_height)
				 VALUES (32736, 1024, 'lgd', now() - interval '2 hours', $1, 1440, 1080)`, f.recordingID)
			insertCMAttempt(t, pool, f.recordingID, "failed", "1 hour")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			f := seedCMDesiredComplete(t, pool)
			control := seedCMDesiredComplete(t, pool)
			if tc.mutate != nil {
				tc.mutate(t, pool, f)
			}
			want := []int64{control.recordingID}
			if tc.want {
				want = append(want, f.recordingID)
			}
			slices.Sort(want)
			if got := cmDesiredRecordings(t, pool); !slices.Equal(got, want) {
				t.Fatalf("cm_detection_desired = %v, want %v (target %d)", got, want, f.recordingID)
			}
		})
	}
}
