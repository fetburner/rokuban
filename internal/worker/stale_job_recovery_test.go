package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
