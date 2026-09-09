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

	// rows がコネクションを保持したまま次の advisory lock を取りに行くと、
	// MaxConns=1 のプールで自分自身を待つ。先に候補をメモリへ読み切って閉じる。
	candidates := make([]staleIngestJob, 0, ingestRecoveryMaxJobsPerSweep)
	for rows.Next() {
		var candidate staleIngestJob
		if err := rows.Scan(&candidate.id, &candidate.recordID, &candidate.recordingID, &candidate.lastActivity, &candidate.attempt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale running ingest job: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("reading stale running ingest jobs: %w", err)
	}
	rows.Close()

	var errs []error
	for _, candidate := range candidates {
		if err := recoverStaleIngestJob(ctx, pool, riverClient, site, candidate); err != nil {
			slog.Warn("ingest recovery: recovering stale candidate failed, continuing with remaining candidates",
				"site", site, "old_job_id", candidate.id, "err", err)
			errs = append(errs, fmt.Errorf("job %d: %w", candidate.id, err))
		}
	}
	return errors.Join(errs...)
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
func recoverStaleIngestJob(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx], site string, candidate staleIngestJob) error {
	lock, acquired, err := acquireIngestJobLock(ctx, pool, candidate.id, defaultRelPathLockTimeout)
	if err != nil {
		return fmt.Errorf("acquiring advisory lock for stale ingest job %d: %w", candidate.id, err)
	}
	if !acquired {
		slog.Debug("ingest recovery skipped live job", "site", site, "old_job_id", candidate.id, "last_activity", candidate.lastActivity)
		return nil
	}
	defer lock.release()

	recoveredAt := time.Now().UTC()
	errorJSON, err := json.Marshal(rivertype.AttemptError{
		At:      recoveredAt,
		Attempt: candidate.attempt,
		Error:   ingestRecoveryReason,
		Trace:   "",
	})
	if err != nil {
		return fmt.Errorf("marshaling stale ingest recovery error: %w", err)
	}
	metadataJSON, err := json.Marshal(map[string]any{
		"ingest_recovery": map[string]any{
			"reason":        ingestRecoveryReason,
			"last_activity": candidate.lastActivity,
			"recovered_at":  recoveredAt,
		},
	})
	if err != nil {
		return fmt.Errorf("marshaling stale ingest recovery metadata: %w", err)
	}

	tx, err := lock.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning stale ingest recovery transaction for job %d: %w", candidate.id, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, discardRecoveredIngestJobQuery, candidate.id, recoveredAt, string(errorJSON), string(metadataJSON))
	if err != nil {
		return fmt.Errorf("discarding stale ingest job %d: %w", candidate.id, err)
	}
	if tag.RowsAffected() == 0 {
		// 候補取得後に River が正常終了させた場合。state 条件が回収と完了の
		// 競合を止め、旧行を上書きして新しい job を作ることを防ぐ。
		return nil
	}

	// 死んだ attempt が残した recording_ingest_progress 行を同じ tx で消す。
	// 消さずに放置すると、代替 ingest が commit するまで（次の record_sweep の
	// 間隔 + 再転送の全時間）API の進捗表示が古い値のまま止まって見える
	// （internal/worker/ingest.go の commit がこの行を消すのと同じ理由）。
	// record_sync 行が無い（候補抽出時点で recording_id が引けない）場合は
	// 消す対象が無いのでスキップする。
	if candidate.recordingID != nil {
		if err := sqlcgen.New(tx).DeleteRecordingIngestProgress(ctx, *candidate.recordingID); err != nil {
			return fmt.Errorf("clearing stale ingest progress for recording %d: %w", *candidate.recordingID, err)
		}
	}

	inserted, err := riverClient.InsertTx(ctx, tx, jobs.IngestJobArgs{Site: site, RecordID: candidate.recordID}, nil)
	if err != nil {
		return fmt.Errorf("inserting replacement ingest job for %d: %w", candidate.id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing stale ingest recovery for job %d: %w", candidate.id, err)
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
