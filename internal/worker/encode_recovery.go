package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

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

	return recoverStaleJobCandidates(
		ctx,
		rows,
		"encode",
		func(rows pgx5.Rows) (staleEncodeJob, error) {
			var candidate staleEncodeJob
			err := rows.Scan(
				&candidate.id,
				&candidate.recordingID,
				&candidate.profile,
				&candidate.lastActivity,
				&candidate.attempt,
			)
			return candidate, err
		},
		func(candidate staleEncodeJob) int64 { return candidate.id },
		func(ctx context.Context, id int64) (*jobLock, bool, error) {
			return acquireEncodeJobLock(ctx, pool, id, defaultJobLockTimeout)
		},
		func(ctx context.Context, conn *pgxpool.Conn, candidate staleEncodeJob) error {
			return recoverStaleEncodeJob(ctx, conn, riverClient, candidate)
		},
	)
}

// recoverStaleEncodeJob は旧 running 行の終端化と新しい encode の投入を、同じ短い
// トランザクションで行う。job advisory lock はこのトランザクションをまたいで同じ
// セッションに保持するため、回収処理自身が二重実行されても安全である。
//
// recording_encode_attempts はここでは変更しない。回収直後から代替ジョブが実行を
// 開始するまで running が残るのは、ctx キャンセル時に running を残す既存規約と同じ
// であり、代替 EncodeWorker の markEncodeAttemptRunning が新しい試行として上書きする。
func recoverStaleEncodeJob(ctx context.Context, conn *pgxpool.Conn, riverClient *river.Client[pgx5.Tx], candidate staleEncodeJob) error {
	inserted, recovered, err := replaceStaleRiverJob(
		ctx,
		conn,
		riverClient,
		"encode",
		candidate.id,
		candidate.attempt,
		candidate.lastActivity,
		encodeRecoveryReason,
		"encode_recovery",
		discardRecoveredEncodeJobQuery,
		jobs.EncodeJobArgs{
			RecordingID: candidate.recordingID,
			Profile:     candidate.profile,
		},
		nil,
	)
	if err != nil || !recovered {
		return err
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
