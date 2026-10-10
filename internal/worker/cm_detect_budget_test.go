package worker

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// failed で予算を使い切った録画が新しいロゴで再び desired になったとき、予算は 1 から
// やり直す（API の再試行が行を消して 1 から始めるのと同じ）。+1 のままだと再実行の
// 最初の失敗がすでに上限で、1 回しか試せずに failed へ落ちる。
func TestCMDetectionNewDesireAfterFailedStartsNewBudget(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	id := seedCMRecording(t, pool, t.TempDir(), 1301)
	for _, step := range []struct {
		count int32
		state string
	}{{1, "retrying"}, {2, "retrying"}, {3, "failed"}} {
		attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
		if attemptCount != step.count {
			t.Fatalf("attempt count = %d, want %d", attemptCount, step.count)
		}
		markCMDetectionTestFailure(t, ctx, q, id, attemptCount, step.state, nil, nil)
	}
	if _, err := pool.Exec(ctx, `UPDATE recording_cm_attempts SET attempted_at = now() - interval '1 hour' WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: []byte("lgd"), LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	if desired, err := q.IsCMDetectionDesired(ctx, id); err != nil || !desired {
		t.Fatalf("IsCMDetectionDesired = %v, %v; want true after a newly learned logo", desired, err)
	}
	attempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: id, MaxAttempts: cmDetectMaxTries,
	})
	if err != nil || !attempt.ShouldRun || attempt.AttemptCount != 1 {
		t.Fatalf("BeginCMDetectionAttempt after failed = %#v, %v; want running attempt 1", attempt, err)
	}
	// Work は attemptCount < cmDetectMaxTries の失敗を retrying にする（cm_detect.go）。
	markCMDetectionTestFailure(t, ctx, q, id, attempt.AttemptCount, "retrying", nil, nil)
	var state string
	var count int32
	if err := pool.QueryRow(ctx, `SELECT state, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &count); err != nil {
		t.Fatal(err)
	}
	if state != "retrying" || count != 1 {
		t.Errorf("after the first failure of the new run: state = %q, count = %d; want retrying, 1", state, count)
	}
}
