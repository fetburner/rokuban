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

	"github.com/fetburner/rokuban/internal/jobs"
)

const (
	// encodeRecoveryStaleAfter は running encode の attempted_at から、プロセス死の
	// 候補として調べ始めるまでの時間。時刻だけでは死亡を確定せず、後段でジョブ
	// advisory lock を取得できた場合にだけ回収する。
	encodeRecoveryStaleAfter = time.Minute

	// encodeRecoveryMaxJobsPerSweep は 1 回の encode_reconcile で調べる候補数。
	// 回収は短いトランザクションを 1 件ずつ実行し、通常の gap-fill を長時間塞がない。
	encodeRecoveryMaxJobsPerSweep = 100

	encodeRecoveryReason = "encode process death detected: job advisory lock was not held"
)

// listStaleEncodeJobsQuery は encode の running 行から回収候補を抽出する。
// attempted_at は候補を絞るためだけに使い、死亡の確定は job-id advisory lock の
// 取得結果で行う。エンコードには ingest のような進捗行が無いため、最後の活動は
// River が試行を開始した attempted_at そのものである。
const listStaleEncodeJobsQuery = `
SELECT
    j.id,
    (j.args->>'recording_id')::bigint,
    j.args->>'profile',
    j.attempted_at,
    j.attempt
FROM river_job AS j
WHERE j.kind = 'encode'
  AND j.state = 'running'
  AND j.args->>'recording_id' IS NOT NULL
  AND j.args->>'profile' IS NOT NULL
  AND j.attempted_at < $1
ORDER BY j.id
LIMIT $2`

const discardRecoveredEncodeJobQuery = `
UPDATE river_job
SET state = 'discarded',
    finalized_at = $2,
    errors = array_append(
        COALESCE(errors, '{}'::jsonb[]),
        $3::jsonb
    ),
    metadata = COALESCE(metadata, '{}'::jsonb) || $4::jsonb
WHERE id = $1
  AND kind = 'encode'
  AND state = 'running'`

type staleEncodeJob struct {
	id           int64
	recordingID  int64
	profile      string
	lastActivity time.Time
	attempt      int
}

// recoverStaleEncodeJobsFunc は encode_reconcile が呼ぶフック。回収の失敗を注入して
// も gap-fill が続くことを、実 DB の回収テストとは独立に固定できる。
var recoverStaleEncodeJobsFunc = recoverStaleEncodeJobs

// recoverStaleEncodeJobs は running encode を候補として調べ、ジョブ ID の advisory
// lock を取得できたものだけをプロセス死と確定する。
//
// 候補ごとの失敗は他の候補の回収を止めない（1 件だけ warn して次へ進む）。
// 全滅した場合は呼び出し元へエラーを返す（errors.Join で束ねる）が、
// encode_reconcile 側はこれを gap-fill を止める理由にせず warn するだけにしている。
func recoverStaleEncodeJobs(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx]) error {
	staleBefore := time.Now().UTC().Add(-encodeRecoveryStaleAfter)

	rows, err := pool.Query(ctx, listStaleEncodeJobsQuery, staleBefore, encodeRecoveryMaxJobsPerSweep)
	if err != nil {
		return fmt.Errorf("listing stale running encode jobs: %w", err)
	}

	// rows がコネクションを保持したまま次の advisory lock を取りに行くと、
	// MaxConns=1 のプールで自分自身を待つ。先に候補をメモリへ読み切って閉じる。
	candidates := make([]staleEncodeJob, 0, encodeRecoveryMaxJobsPerSweep)
	for rows.Next() {
		var candidate staleEncodeJob
		if err := rows.Scan(
			&candidate.id,
			&candidate.recordingID,
			&candidate.profile,
			&candidate.lastActivity,
			&candidate.attempt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale running encode job: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading stale running encode jobs: %w", err)
	}
	rows.Close()

	var errs []error
	for _, candidate := range candidates {
		if err := recoverStaleEncodeJob(ctx, pool, riverClient, candidate); err != nil {
			slog.Warn("encode recovery: recovering stale candidate failed, continuing with remaining candidates",
				"old_job_id", candidate.id, "err", err)
			errs = append(errs, fmt.Errorf("job %d: %w", candidate.id, err))
		}
	}
	return errors.Join(errs...)
}

// recoverStaleEncodeJob は旧 running 行の終端化と新しい encode の投入を、同じ短い
// トランザクションで行う。job advisory lock はこのトランザクションをまたいで同じ
// セッションに保持するため、回収処理自身が二重実行されても安全である。
//
// recording_encode_attempts はここでは変更しない。回収直後から代替ジョブが実行を
// 開始するまで running が残るのは、ctx キャンセル時に running を残す既存規約と同じ
// であり、代替 EncodeWorker の markEncodeAttemptRunning が新しい試行として上書きする。
func recoverStaleEncodeJob(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx], candidate staleEncodeJob) error {
	lock, acquired, err := acquireEncodeJobLock(ctx, pool, candidate.id, defaultJobLockTimeout)
	if err != nil {
		return fmt.Errorf("acquiring advisory lock for stale encode job %d: %w", candidate.id, err)
	}
	if !acquired {
		slog.Debug("encode recovery skipped live job", "old_job_id", candidate.id, "last_activity", candidate.lastActivity)
		return nil
	}
	defer lock.release()
	// Work の長時間エンコードとは違い、recovery はこの lock 用 connection 自身で
	// transaction を実行する。heartbeat と pgx connection を同時利用しない。
	lock.stopHeartbeatLoop()

	recoveredAt := time.Now().UTC()
	errorJSON, err := json.Marshal(rivertype.AttemptError{
		At:      recoveredAt,
		Attempt: candidate.attempt,
		Error:   encodeRecoveryReason,
		Trace:   "",
	})
	if err != nil {
		return fmt.Errorf("marshaling stale encode recovery error: %w", err)
	}
	metadataJSON, err := json.Marshal(map[string]any{
		"encode_recovery": map[string]any{
			"reason":        encodeRecoveryReason,
			"last_activity": candidate.lastActivity,
			"recovered_at":  recoveredAt,
		},
	})
	if err != nil {
		return fmt.Errorf("marshaling stale encode recovery metadata: %w", err)
	}

	tx, err := lock.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning stale encode recovery transaction for job %d: %w", candidate.id, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, discardRecoveredEncodeJobQuery, candidate.id, recoveredAt, string(errorJSON), string(metadataJSON))
	if err != nil {
		return fmt.Errorf("discarding stale encode job %d: %w", candidate.id, err)
	}
	if tag.RowsAffected() == 0 {
		// 候補取得後に River が正常終了させた場合。state 条件が回収と完了の
		// 競合を止め、旧行を上書きして新しい job を作ることを防ぐ。
		return nil
	}

	inserted, err := riverClient.InsertTx(ctx, tx, jobs.EncodeJobArgs{
		RecordingID: candidate.recordingID,
		Profile:     candidate.profile,
	}, nil)
	if err != nil {
		return fmt.Errorf("inserting replacement encode job for %d: %w", candidate.id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing stale encode recovery for job %d: %w", candidate.id, err)
	}

	replacementID := int64(0)
	if inserted != nil && inserted.Job != nil {
		replacementID = inserted.Job.ID
	}
	slog.Info("encode: recovered stale running job",
		"old_job_id", candidate.id,
		"new_job_id", replacementID,
		"new_job_unique_skipped", inserted != nil && inserted.UniqueSkippedAsDuplicate,
		"recording_id", candidate.recordingID,
		"profile", candidate.profile,
		"last_activity", candidate.lastActivity,
	)
	return nil
}
