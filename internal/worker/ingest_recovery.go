package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

const (
	// ingestRecoveryStaleAfter は running ingest の最後の活動から、プロセス死の
	// 候補として調べ始めるまでの時間。時刻だけでは死亡を確定せず、後段でジョブ
	// advisory lock を取得できた場合にだけ回収する。
	ingestRecoveryStaleAfter = time.Minute

	// ingestRecoveryMaxJobsPerSweep は 1 回の record_sweep で調べる候補数。回収は
	// 短いトランザクションを 1 件ずつ実行し、通常の record sweep を長時間塞がない。
	ingestRecoveryMaxJobsPerSweep = 100

	ingestRecoveryReason = "ingest process death detected: job advisory lock was not held"
)

// listStaleIngestJobsQuery の「最後の活動」は GREATEST(p.observed_at,
// j.attempted_at) --- COALESCE ではない。DeleteRecordingIngestProgress
// （internal/worker/ingest.go の commit / already-committed 経路）は成功した
// attempt でしか呼ばれないため、失敗して再試行した attempt は古い
// recording_ingest_progress 行を残したまま attempted_at だけを更新する。
// （cancel / fail を観測した終端経路も同じ関数を呼ぶが、そこではジョブが
// state='running' でなくなるのでこのクエリの候補にはならない。）
// River のバックオフ（attempt^4 秒）で attempt 3 以降は間隔が
// ingestRecoveryStaleAfter（1 分）を超えるので、COALESCE(p.observed_at,
// j.attempted_at) のまま新しい attempted_at を無視すると、再試行が
// state='running' になった直後（acquireIngestJobLock に到達する前）を
// 「進捗が 1 分以上前から古い」候補として拾ってしまう。GREATEST は NULL を
// 無視するので、進捗行が無いケース（COALESCE の元の fallback）も同じ式のまま成立する。
const listStaleIngestJobsQuery = `
SELECT
    j.id,
    j.args->>'record_id',
    rs.recording_id,
    GREATEST(p.observed_at, j.attempted_at) AS last_activity,
    j.attempt
FROM river_job AS j
LEFT JOIN record_sync AS rs
    ON rs.site = $1
   AND rs.record_id = j.args->>'record_id'
LEFT JOIN recording_ingest_progress AS p
    ON p.recording_id = rs.recording_id
WHERE j.kind = 'ingest'
  AND j.queue = $2
  AND j.state = 'running'
  AND j.args->>'site' = $1
  AND j.args->>'record_id' IS NOT NULL
  AND GREATEST(p.observed_at, j.attempted_at) < $3
ORDER BY j.id
LIMIT $4`

const discardRecoveredIngestJobQuery = `
UPDATE river_job
SET state = 'discarded',
    finalized_at = $2,
    errors = array_append(
        COALESCE(errors, '{}'::jsonb[]),
        $3::jsonb
    ),
    metadata = COALESCE(metadata, '{}'::jsonb) || $4::jsonb
WHERE id = $1
  AND kind = 'ingest'
  AND state = 'running'`

type staleIngestJob struct {
	id           int64
	recordID     string
	recordingID  *int64
	lastActivity time.Time
	attempt      int
}

// recoverStaleIngestJobs は site の running ingest を候補として調べ、ジョブ ID の
// advisory lock を取得できたものだけをプロセス死と確定する。進捗行がまだ作られて
// いない（ファイル作成より前に死んだ）ケースも attempted_at を fallback にして含める。
//
// 候補ごとの失敗は他の候補の回収を止めない（1 件だけ warn して次へ進む）。
// 全滅した場合は呼び出し元へエラーを返す（errors.Join で束ねる）が、record_sweep
// 側はこれを wt.Sweep（真実の再取得）を止める理由にせず warn するだけにしている
// （record_sweep.go 参照）。
func recoverStaleIngestJobs(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx], site string) error {
	staleBefore := time.Now().UTC().Add(-ingestRecoveryStaleAfter)
	queue := jobs.PhysicalQueueName(jobs.IngestQueue, site)

	rows, err := pool.Query(ctx, listStaleIngestJobsQuery, site, queue, staleBefore, ingestRecoveryMaxJobsPerSweep)
	if err != nil {
		return fmt.Errorf("listing stale running ingest jobs: %w", err)
	}

	return recoverStaleJobCandidates(
		ctx,
		rows,
		"ingest",
		func(rows pgx5.Rows) (staleIngestJob, error) {
			var candidate staleIngestJob
			err := rows.Scan(&candidate.id, &candidate.recordID, &candidate.recordingID, &candidate.lastActivity, &candidate.attempt)
			return candidate, err
		},
		func(candidate staleIngestJob) int64 { return candidate.id },
		func(ctx context.Context, id int64) (*jobLock, bool, error) {
			return acquireIngestJobLock(ctx, pool, id, defaultJobLockTimeout)
		},
		func(ctx context.Context, conn *pgxpool.Conn, candidate staleIngestJob) error {
			return recoverStaleIngestJob(ctx, conn, riverClient, site, candidate)
		},
	)
}

// recoverStaleIngestJob は旧 running 行の終端化と新しい ingest の投入を、同じ短い
// トランザクションで行う。ジョブ advisory lock はこのトランザクションをまたいで
// 同じセッションに保持するため、回収処理自身が二重実行されても安全である。
//
// 直後に走る watcher.Sweep の再投入（同じ Work の中で、この関数の後に呼ばれる）に
// 任せず、ここで明示的に InsertTx するのは、mirakc がその record をもう返さない
// （record 削除済み）ケースや Sweep 自体が失敗するケースでも、回収だけで
// 「旧行の終端化」と「代替の存在」を同じ tx で確定させるため。Sweep が同じ
// (site, record_id) を InsertTx しても UniqueOpts（pendingJobStates）がここで
// 作った available 行に合流するだけで、二重に ingest が走ることはない
// （TestRecordSweepRecovery_ReplacesStaleRunningIngest 参照）。
func recoverStaleIngestJob(ctx context.Context, conn *pgxpool.Conn, riverClient *river.Client[pgx5.Tx], site string, candidate staleIngestJob) error {
	var beforeInsert func(pgx5.Tx) error
	if candidate.recordingID != nil {
		// Clear the failed attempt's progress in the same transaction so the API
		// does not show stale progress until the replacement ingest finishes.
		// TestRecordSweepRecovery_ReplacesStaleRunningIngest verifies this write.
		beforeInsert = func(tx pgx5.Tx) error {
			if err := sqlcgen.New(tx).DeleteRecordingIngestProgress(ctx, *candidate.recordingID); err != nil {
				return fmt.Errorf("clearing stale ingest progress for recording %d: %w", *candidate.recordingID, err)
			}
			return nil
		}
	}
	inserted, recovered, err := replaceStaleRiverJob(
		ctx,
		conn,
		riverClient,
		"ingest",
		candidate.id,
		candidate.attempt,
		candidate.lastActivity,
		ingestRecoveryReason,
		"ingest_recovery",
		discardRecoveredIngestJobQuery,
		jobs.IngestJobArgs{Site: site, RecordID: candidate.recordID},
		beforeInsert,
	)
	if err != nil || !recovered {
		return err
	}

	replacementID := int64(0)
	if inserted != nil && inserted.Job != nil {
		replacementID = inserted.Job.ID
	}
	slog.Info("ingest: recovered stale running job",
		"site", site,
		"old_job_id", candidate.id,
		"new_job_id", replacementID,
		"new_job_unique_skipped", inserted != nil && inserted.UniqueSkippedAsDuplicate,
		"last_activity", candidate.lastActivity,
	)
	return nil
}
