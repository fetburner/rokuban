package cmdetect

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// failed で予算を使い切った録画が新しいロゴで再び desired になったとき、予算は 1 から
// やり直す（API の再試行が行を消して 1 から始めるのと同じ）。+1 のままだと再実行の
// 最初の失敗がすでに上限で、1 回しか試せずに failed へ落ちる。
func TestCMDetectionNewDesireAfterFailedStartsNewBudget(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	det := pgstoretest.Analysis(pool)
	id := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(1301)))
	for _, step := range []struct {
		count int32
		state string
	}{{1, "retrying"}, {2, "retrying"}, {3, "failed"}} {
		attemptCount := startCMDetectionTestAttempt(t, ctx, det, id)
		if attemptCount != step.count {
			t.Fatalf("attempt count = %d, want %d", attemptCount, step.count)
		}
		markCMDetectionTestFailure(t, ctx, pool, det, id, attemptCount, step.state, nil, nil)
	}
	testutil.MustExec(t, pool, "aging the attempt", `UPDATE recording_cm_attempts SET attempted_at = now() - interval '1 hour' WHERE recording_id = $1`, id)
	dbtest.CMLogo(t, pool, func(p *dbtest.CMLogoParams) { p.LearnedFrom = &id })
	if desired, err := det.IsDetectionDesired(ctx, id); err != nil || !desired {
		t.Fatalf("IsDetectionDesired = %v, %v; want true after a newly learned logo", desired, err)
	}
	attempt, shouldRun, err := det.BeginAttempt(ctx, id, cmDetectMaxTries)
	if err != nil || !shouldRun || attempt.AttemptCount != 1 {
		t.Fatalf("BeginAttempt after failed = %#v, %v; want running attempt 1", attempt, err)
	}
	// Work は attemptCount < cmDetectMaxTries の失敗を retrying にする（cm_detect.go）。
	markCMDetectionTestFailure(t, ctx, pool, det, id, attempt.AttemptCount, "retrying", nil, nil)
	var state string
	var count int32
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &count), "reading attempt state")
	if state != "retrying" || count != 1 {
		t.Errorf("after the first failure of the new run: state = %q, count = %d; want retrying, 1", state, count)
	}
}
