package db_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestCMLogoCandidateDesired は、ロゴ候補の定期パスの希望（cm_logo_candidate_desired）を、定期パスが読む ListMissingCMLogoCandidates 経由で検査する。
// 候補の行は状態によらず存在するだけで希望から外れる。failed も例外ではなく、失敗した候補を定期パスが作り直さない。
// 行が無い枠の希望（先頭のケース）を対照にして、ビューが常に空を返す誤りで他のケースが空虚に通らないようにする。
// `candidate.network_id IS NULL` を外すと running・ready・failed の 3 ケースが失敗し、failed だけを通すと failed のケースが失敗する。
func TestCMLogoCandidateDesired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state string // 空なら候補の行を作らない
		want  bool
	}{
		{name: "NoCandidateRow_Desired", state: "", want: true},
		{name: "RunningCandidate_NotDesired", state: "running", want: false},
		{name: "ReadyCandidate_NotDesired", state: "ready", want: false},
		{name: "FailedCandidate_NotDesired", state: "failed", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.NetworkID = 32736
				p.ServiceID = 1024
			})
			dbtest.MediaAsset(t, pool, recordingID) // 原本と、その既定の置き場の active なコピー
			testutil.RequireNoError(t, sqlcgen.New(pool).UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
				NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
			}), "UpsertCMLogoArea")
			if tc.state != "" {
				testutil.MustExec(t, pool, "inserting cm_logo_candidates", `
					INSERT INTO cm_logo_candidates (
						network_id, service_id, state, stage, error,
						x, y, w, h, coded_width, coded_height, recording_id, observed_area_updated_at
					)
					SELECT 32736, 1024, $1,
					       CASE WHEN $1 = 'failed' THEN 'logo' END,
					       CASE WHEN $1 = 'failed' THEN 'failed' END,
					       1180, 24, 240, 96, 1440, 1080, $2, a.updated_at
					FROM cm_logo_areas a
					WHERE a.network_id = 32736 AND a.service_id = 1024`, tc.state, recordingID)
			}

			rows, err := sqlcgen.New(pool).ListMissingCMLogoCandidates(ctx, 100)
			testutil.RequireNoError(t, err, "ListMissingCMLogoCandidates")
			desired := false
			for _, row := range rows {
				if row.NetworkID == 32736 && row.ServiceID == 1024 {
					desired = true
				}
			}
			if desired != tc.want {
				t.Errorf("desired = %v, want %v (candidate state %q)", desired, tc.want, tc.state)
			}
		})
	}
}
