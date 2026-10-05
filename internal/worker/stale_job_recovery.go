package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// recoverStaleJobCandidates は候補行をすべて読み切って閉じた後に advisory lock を取り、
// heartbeat を止めてから lock の connection を回収処理へ渡す。4 種の回収でこの順序を
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

// replaceStaleRiverJob は stale running River job の discard と代替ジョブの投入を、
// job lock を持つ connection 上の 1 transaction で行う。beforeInsert は ingest 固有の
// 書き込みを同じ transaction に加える。metadata は kind+"_recovery" のキーに記録する。
// discard が 0 行なら代替を投入せず nil を返す。
// TestReplaceStaleRiverJobDoesNotReplaceCompletedJob は完了済みジョブが差し替わらないことを検証する。
func replaceStaleRiverJob(
	ctx context.Context,
	conn *pgxpool.Conn,
	riverClient *river.Client[pgx5.Tx],
	kind string,
	jobID int64,
	attempt int,
	lastActivity time.Time,
	recoveryReason string,
	discardQuery string,
	args river.JobArgs,
	beforeInsert func(pgx5.Tx) error,
) (*rivertype.JobInsertResult, error) {
	recoveredAt := time.Now().UTC()
	errorJSON, err := json.Marshal(rivertype.AttemptError{
		At:      recoveredAt,
		Attempt: attempt,
		Error:   recoveryReason,
		Trace:   "",
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling stale %s recovery error: %w", kind, err)
	}
	metadataJSON, err := json.Marshal(map[string]any{
		kind + "_recovery": map[string]any{
			"reason":        recoveryReason,
			"last_activity": lastActivity,
			"recovered_at":  recoveredAt,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling stale %s recovery metadata: %w", kind, err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning stale %s recovery transaction for job %d: %w", kind, jobID, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, discardQuery, jobID, recoveredAt, string(errorJSON), string(metadataJSON))
	if err != nil {
		return nil, fmt.Errorf("discarding stale %s job %d: %w", kind, jobID, err)
	}
	if tag.RowsAffected() == 0 {
		// 候補取得後に River が正常終了させた場合。state 条件が回収と完了の
		// 競合を止め、旧行を上書きして新しい job を作ることを防ぐ。
		return nil, nil
	}

	if beforeInsert != nil {
		if err := beforeInsert(tx); err != nil {
			return nil, err
		}
	}
	inserted, err := riverClient.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("inserting replacement %s job for %d: %w", kind, jobID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing stale %s recovery for job %d: %w", kind, jobID, err)
	}
	return inserted, nil
}
