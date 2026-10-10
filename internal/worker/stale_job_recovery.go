package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recoverStaleJobCandidates は候補行をすべて読み切って閉じた後に advisory lock を取り、
// heartbeat を止めてから lock の connection を回収処理へ渡す。cm_detect の 2 種の回収でこの順序を
// 揃え、回収エラーを集めながら後続候補の処理を続ける。
//
// rows がコネクションを保持したまま次の advisory lock を取りに行くと、MaxConns=1 の
// プールで自分自身を待つ。そのため lock より先に候補をメモリへ読み切って閉じる。
// TestRecoverStaleJobCandidatesStopsHeartbeatAndJoinsErrors は close / heartbeat 停止の順と、
// 1 件の失敗後も次の候補を処理することを検証する。
func recoverStaleJobCandidates[T any](
	ctx context.Context,
	rows pgx5.Rows,
	kind string,
	scanCandidate func(pgx5.Rows) (T, error),
	jobID func(T) int64,
	acquireLock func(context.Context, int64) (*jobLock, bool, error),
	recoverLocked func(context.Context, *pgxpool.Conn, T) error,
) error {
	defer rows.Close()

	candidates := make([]T, 0)
	for rows.Next() {
		candidate, err := scanCandidate(rows)
		if err != nil {
			return fmt.Errorf("scanning stale %s job: %w", kind, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading stale %s jobs: %w", kind, err)
	}
	rows.Close()

	var errs []error
	for _, candidate := range candidates {
		id := jobID(candidate)
		lock, acquired, err := acquireLock(ctx, id)
		if err != nil {
			recoveryErr := fmt.Errorf("locking stale %s job %d: %w", kind, id, err)
			slog.Warn("stale job recovery failed; continuing with remaining candidates",
				"kind", kind, "job_id", id, "err", recoveryErr)
			errs = append(errs, recoveryErr)
			continue
		}
		if !acquired {
			slog.Debug("stale job recovery skipped live job", "kind", kind, "job_id", id)
			continue
		}

		err = func() error {
			defer lock.release()
			// Work の長時間処理とは違い、recovery はこの lock 用 connection 自身で
			// transaction を実行する。heartbeat と pgx connection を同時利用しない。
			lock.stopHeartbeatLoop()
			return recoverLocked(ctx, lock.conn, candidate)
		}()
		if err != nil {
			recoveryErr := fmt.Errorf("recovering stale %s job %d: %w", kind, id, err)
			slog.Warn("stale job recovery failed; continuing with remaining candidates",
				"kind", kind, "job_id", id, "err", recoveryErr)
			errs = append(errs, recoveryErr)
		}
	}
	return errors.Join(errs...)
}
