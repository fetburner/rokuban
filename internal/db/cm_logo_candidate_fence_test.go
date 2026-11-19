package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestCMLogoCandidateAttemptFence は、試行の結果を書く MarkCMLogoCandidateReady / MarkCMLogoCandidateFailure が、
// 現在の running 行の attempted_at と一致しない試行の結果を書かないことを検査する。
// 行は現在の試行（newAttempt）の 1 本だけを置き、候補が作り直された後に古い試行（oldAttempt）の結果が届いた場面を表す。
// 古い試行のケース（stale）は 0 行しか更新せず、行は running のまま attempted_at も変わらない。
// 現在の試行のケース（current）は 1 行を更新する。これは対照で、クエリが何もしない誤りで stale のケースが空虚に通らないようにする。
// `attempted_at` の一致条件を外すと stale の 2 ケースが失敗する（行が ready / failed になる）。current の 2 ケースは通り続ける。
// 作り直しの経緯（削除して入れ直す）と discard の扱いは、store 契約の TestCMStationContract が検査する。
func TestCMLogoCandidateAttemptFence(t *testing.T) {
	t.Parallel()
	ready := func(q *sqlcgen.Queries, areaAt, attempt time.Time) (int64, error) {
		return q.MarkCMLogoCandidateReady(context.Background(), sqlcgen.MarkCMLogoCandidateReadyParams{
			NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
			AttemptedAt: attempt, Lgd: []byte("late logo"),
		})
	}
	failure := func(q *sqlcgen.Queries, areaAt, attempt time.Time) (int64, error) {
		stage, message := "logo", "late failure"
		return q.MarkCMLogoCandidateFailure(context.Background(), sqlcgen.MarkCMLogoCandidateFailureParams{
			NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
			AttemptedAt: attempt, Stage: &stage, Error: &message,
		})
	}
	for _, tc := range []struct {
		name      string
		call      func(q *sqlcgen.Queries, areaAt, attempt time.Time) (int64, error)
		stale     bool   // true なら現在の行より古い試行で書く
		wantRows  int64  // 更新される行数
		wantState string // 書いた後の行の state
	}{
		{name: "ReadyStaleAttempt_NoWrite", call: ready, stale: true, wantRows: 0, wantState: "running"},
		{name: "ReadyCurrentAttempt_Writes", call: ready, stale: false, wantRows: 1, wantState: "ready"},
		{name: "FailureStaleAttempt_NoWrite", call: failure, stale: true, wantRows: 0, wantState: "running"},
		{name: "FailureCurrentAttempt_Writes", call: failure, stale: false, wantRows: 1, wantState: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			q := sqlcgen.New(pool)
			recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.NetworkID = 32736
				p.ServiceID = 1024
			})
			testutil.RequireNoError(t, q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
				NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
			}), "UpsertCMLogoArea")
			area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
			testutil.RequireNoError(t, err, "GetCMLogoArea")

			// 現在の行の試行と、それより古い試行。固定値で 1 秒ずらす。
			newAttempt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			oldAttempt := newAttempt.Add(-time.Second)
			testutil.MustExec(t, pool, "inserting running cm_logo_candidates", `
				INSERT INTO cm_logo_candidates (
					network_id, service_id, state,
					x, y, w, h, coded_width, coded_height, recording_id, observed_area_updated_at, attempted_at
				) VALUES (32736, 1024, 'running', 1180, 24, 240, 96, 1440, 1080, $1, $2, $3)`,
				recordingID, area.UpdatedAt, newAttempt)

			attempt := newAttempt
			if tc.stale {
				attempt = oldAttempt
			}
			rows, err := tc.call(q, area.UpdatedAt, attempt)
			testutil.RequireNoError(t, err, "mark candidate")
			if rows != tc.wantRows {
				t.Errorf("updated rows = %d, want %d", rows, tc.wantRows)
			}
			candidate, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024})
			testutil.RequireNoError(t, err, "GetCMLogoCandidate")
			if candidate.State != tc.wantState {
				t.Errorf("candidate state = %q, want %q", candidate.State, tc.wantState)
			}
			if !candidate.AttemptedAt.Equal(newAttempt) {
				t.Errorf("candidate attempted_at = %s, want %s", candidate.AttemptedAt, newAttempt)
			}
		})
	}
}
