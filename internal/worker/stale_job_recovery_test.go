package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

type staleRecoveryTestCandidate struct {
	id int64
}

type staleRecoveryTestRows struct {
	ids     []int64
	index   int
	current int64
	closed  bool
}

func (r *staleRecoveryTestRows) Close() { r.closed = true }

func (r *staleRecoveryTestRows) Err() error { return nil }

func (r *staleRecoveryTestRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

func (r *staleRecoveryTestRows) FieldDescriptions() []pgconn.FieldDescription { return nil }

func (r *staleRecoveryTestRows) Next() bool {
	if r.closed || r.index >= len(r.ids) {
		r.Close()
		return false
	}
	r.current = r.ids[r.index]
	r.index++
	return true
}

func (r *staleRecoveryTestRows) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected stale recovery scan destinations")
	}
	id, ok := dest[0].(*int64)
	if !ok {
		return errors.New("stale recovery scan destination is not *int64")
	}
	*id = r.current
	return nil
}

func (r *staleRecoveryTestRows) Values() ([]any, error) { return nil, nil }

func (r *staleRecoveryTestRows) RawValues() [][]byte { return nil }

func (r *staleRecoveryTestRows) Conn() *pgx.Conn { return nil }

func (r *staleRecoveryTestRows) TypeMap() *pgtype.Map { return nil }

var _ pgx.Rows = (*staleRecoveryTestRows)(nil)

// TestRecoverStaleJobCandidatesStopsHeartbeatAndJoinsErrors は共有の回収順序を固定する。
// lock 取得より先に候補 rows を閉じ、回収 callback より先に heartbeat を止め、
// 1 件が失敗しても次の候補を飛ばさない。
func TestRecoverStaleJobCandidatesStopsHeartbeatAndJoinsErrors(t *testing.T) {
	rows := &staleRecoveryTestRows{ids: []int64{17, 23}}
	firstErr := errors.New("first candidate recovery failed")
	locks := make(map[int64]*jobLock, len(rows.ids))
	var recoveredIDs []int64

	err := recoverStaleJobCandidates(
		context.Background(),
		rows,
		"test",
		func(rows pgx.Rows) (staleRecoveryTestCandidate, error) {
			var candidate staleRecoveryTestCandidate
			err := rows.Scan(&candidate.id)
			return candidate, err
		},
		func(candidate staleRecoveryTestCandidate) int64 { return candidate.id },
		func(_ context.Context, id int64) (*jobLock, bool, error) {
			if !rows.closed {
				t.Error("advisory lock was acquired before candidate rows closed")
			}
			lock := &jobLock{stopHeartbeat: make(chan struct{}), heartbeatDone: make(chan struct{})}
			close(lock.heartbeatDone)
			locks[id] = lock
			return lock, true, nil
		},
		func(_ context.Context, _ *pgxpool.Conn, candidate staleRecoveryTestCandidate) error {
			if !rows.closed {
				t.Error("recovery callback ran before candidate rows closed")
			}
			if locks[candidate.id].stopHeartbeat != nil {
				t.Errorf("heartbeat for job %d is still running in recovery callback", candidate.id)
			}
			recoveredIDs = append(recoveredIDs, candidate.id)
			if candidate.id == 17 {
				return firstErr
			}
			return nil
		},
	)

	if !errors.Is(err, firstErr) {
		t.Fatalf("recoverStaleJobCandidates error = %v, want joined first candidate error", err)
	}
	if len(recoveredIDs) != 2 || recoveredIDs[0] != 17 || recoveredIDs[1] != 23 {
		t.Fatalf("recovered job IDs = %v, want [17 23]", recoveredIDs)
	}
}

// TestReplaceStaleRiverJobDoesNotReplaceCompletedJob は transaction 経路上の
// RowsAffected ガードを守る。候補取得後に完了したジョブから 2 本目のジョブを作らない。
func TestReplaceStaleRiverJobDoesNotReplaceCompletedJob(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	recordingID := seedRecordingWithOriginal(t, pool, t.TempDir(), "recovery/completed-before-recovery.m2ts", []string{"h264"}, []byte("payload"))
	oldJobID := insertStaleRunningEncodeJob(t, pool, recordingID, "h264")
	if _, err := pool.Exec(ctx, `
		UPDATE river_job
		SET state = 'completed', finalized_at = now()
		WHERE id = $1`, oldJobID); err != nil {
		t.Fatalf("completing stale encode job before recovery: %v", err)
	}

	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	lock, acquired, err := acquireEncodeJobLock(ctx, pool, oldJobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring completed job lock: %v", err)
	}
	if !acquired {
		t.Fatal("completed job advisory lock was not acquired")
	}
	t.Cleanup(lock.release)
	lock.stopHeartbeatLoop()

	inserted, err := replaceStaleRiverJob(
		ctx,
		lock.conn,
		client,
		"encode",
		oldJobID,
		1,
		time.Now().UTC().Add(-time.Minute),
		encodeRecoveryReason,
		"encode_recovery",
		discardRecoveredEncodeJobQuery,
		jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
		nil,
	)
	if err != nil {
		t.Fatalf("replaceStaleRiverJob: %v", err)
	}
	if inserted != nil {
		t.Fatal("completed job was treated as recovered")
	}

	job := testutil.MustGetRiverJob(t, ctx, testutil.NewRiverClient(t, pool), oldJobID)
	if job.State != rivertype.JobStateCompleted {
		t.Errorf("old job state = %q, want completed", job.State)
	}
	jobCount := 0
	for _, row := range testutil.MustListRiverJobsOfKind(t, ctx, pool, (jobs.EncodeJobArgs{}).Kind()) {
		args := testutil.MustDecodeRiverJobArgs[jobs.EncodeJobArgs](t, row)
		if args.RecordingID == recordingID {
			jobCount++
		}
	}
	if jobCount != 1 {
		t.Errorf("encode job count after completed-job recovery = %d, want 1", jobCount)
	}
}
