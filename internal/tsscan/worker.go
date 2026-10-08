package tsscan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/tsstat"
)

const (
	reconcileTimeout  = 5 * time.Minute
	reconcileRowLimit = 1000
	scanBatchSize     = 500

	// scanTimeout is how long one scan attempt may run before River cancels it, and
	// also the point after which River's rescuer treats a running job as dead.
	// 未検証: the longest original and the read throughput were not measured. If the
	// value is too short, a long original's scan times out and is retried forever.
	scanTimeout = 6 * time.Hour
)

// ScanWorker reads an active original and replaces its TS statistics atomically.
type ScanWorker struct {
	river.WorkerDefaults[ScanArgs]
	Pool     *pgxpool.Pool
	MediaDir string
}

// Timeout bounds one scan attempt so River's rescuer can recover a job whose
// process died. A scan is idempotent and restartable, so a timeout only costs a retry.
func (*ScanWorker) Timeout(*river.Job[ScanArgs]) time.Duration { return scanTimeout }

// Work reads an original from byte zero, then replaces its statistics and scan marker
// in one database transaction. A size change during the read leaves it eligible for
// the next reconcile pass.
func (w *ScanWorker) Work(ctx context.Context, job *river.Job[ScanArgs]) error {
	started := time.Now()
	result := "failure"
	defer func() {
		metrics.TSScanDuration.Observe(time.Since(started).Seconds())
		metrics.TSScanJobs.WithLabelValues(result).Inc()
	}()

	q := sqlcgen.New(w.Pool)
	asset, err := q.GetActiveOriginalForTSScan(ctx, job.Args.RecordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		result = "success"
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading active original for recording %d: %w", job.Args.RecordingID, err)
	}

	counter, size, err := scanFile(ctx, w.MediaDir, asset.RelPath, asset.SizeBytes)
	if err != nil {
		return fmt.Errorf("scanning original media asset %d: %w", asset.ID, err)
	}

	metrics.TSScanDroppedPackets.Add(float64(counter.TotalDrops()))
	metrics.TSScanErrorPackets.Add(float64(counter.TotalErrors()))
	metrics.TSScanScrambledPackets.Add(float64(counter.TotalScrambled()))

	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning TS scan transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txq := sqlcgen.New(tx)
	currentSize, err := txq.LockActiveOriginalForTSScan(ctx, asset.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		result = "success"
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking original media asset %d before replacing statistics: %w", asset.ID, err)
	}
	if currentSize != size {
		// In-place registration may have replaced the file while it was read. Leave
		// old observations alone; the current size still differs from the scan marker.
		result = "success"
		return nil
	}

	if err := txq.DeleteDropStatsForTSScan(ctx, asset.ID); err != nil {
		return fmt.Errorf("deleting old drop_stats for media asset %d: %w", asset.ID, err)
	}
	if err := txq.DeleteDropPositionsForTSScan(ctx, asset.ID); err != nil {
		return fmt.Errorf("deleting old drop_positions for media asset %d: %w", asset.ID, err)
	}
	if err := replaceStats(ctx, txq, asset.ID, counter); err != nil {
		return fmt.Errorf("writing TS statistics for media asset %d: %w", asset.ID, err)
	}
	if err := txq.UpsertMediaAssetTSScan(ctx, sqlcgen.UpsertMediaAssetTSScanParams{
		MediaAssetID:     asset.ID,
		ScannedSizeBytes: size,
	}); err != nil {
		return fmt.Errorf("upserting TS scan record for media asset %d: %w", asset.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing TS scan for media asset %d: %w", asset.ID, err)
	}
	result = "success"
	return nil
}

// ReconcileWorker enqueues missing or stale scans from the current database state.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	Pool     *pgxpool.Pool
	RowLimit int32
}

// Timeout bounds candidate selection and River inserts for one reconciliation pass.
func (*ReconcileWorker) Timeout(*river.Job[ReconcileArgs]) time.Duration {
	return reconcileTimeout
}

// Work enqueues one bounded window and persists the next page in a continuation job.
func (w *ReconcileWorker) Work(ctx context.Context, job *river.Job[ReconcileArgs]) error {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("TS scan reconcile: getting river client: %w", err)
	}
	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = reconcileRowLimit
	}
	rows, err := sqlcgen.New(w.Pool).ListMissingTSScanRecordings(ctx, sqlcgen.ListMissingTSScanRecordingsParams{
		AfterRecordingID: job.Args.AfterRecordingID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		return fmt.Errorf("listing originals that need TS scans: %w", err)
	}
	for _, recordingID := range rows {
		if err := EnqueueScan(ctx, client, recordingID); err != nil {
			slog.Error("ts_scan_reconcile: failed to enqueue scan", "recording_id", recordingID, "err", err)
		}
	}

	var continuationAfter int64
	if int32(len(rows)) >= rowLimit {
		continuationAfter = rows[len(rows)-1]
		if _, err := client.Insert(ctx, ReconcileArgs{AfterRecordingID: continuationAfter}, nil); err != nil {
			return fmt.Errorf("enqueuing TS scan reconcile continuation after recording %d: %w", continuationAfter, err)
		}
	}
	metrics.TSScanReconcileLastPass.SetToCurrentTime()
	if len(rows) > 0 {
		slog.Info("ts_scan_reconcile: pass complete", "candidates", len(rows),
			"row_limit", rowLimit, "continuation_after", continuationAfter)
	}
	return nil
}

// scanFile reads the whole original through a tsstat.Counter. It rejects a file whose
// size differs from wantSize before reading, so a persistent mismatch does not cost
// a full read on every retry and reconcile pass.
func scanFile(ctx context.Context, mediaDir, relPath string, wantSize int64) (*tsstat.Counter, int64, error) {
	path, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving original path: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening original: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stating original: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("original is not a regular file: %s", path)
	}

	if info.Size() != wantSize {
		return nil, 0, fmt.Errorf("original size differs from database: database=%d file=%d", wantSize, info.Size())
	}

	counter := tsstat.NewCounter(io.Discard)
	read, err := io.Copy(counter, contextReader{ctx: ctx, reader: f})
	if err != nil {
		return nil, read, fmt.Errorf("reading original: %w", err)
	}
	if read != info.Size() {
		return nil, read, fmt.Errorf("original size changed during scan: before=%d read=%d", info.Size(), read)
	}
	return counter, read, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func replaceStats(ctx context.Context, q *sqlcgen.Queries, assetID int64, counter *tsstat.Counter) error {
	stats := counter.Stats()
	pids := make([]int, 0, len(stats))
	for pid := range stats {
		pids = append(pids, pid)
	}
	slices.Sort(pids)

	statRows := make([]sqlcgen.InsertDropStatParams, 0, len(pids))
	positionRows := make([]sqlcgen.InsertDropPositionParams, 0)
	for _, pid := range pids {
		stat := stats[pid]
		var pidType *string
		if stat.Type != "" {
			t := stat.Type
			pidType = &t
		}
		statRows = append(statRows, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          int32(pid),
			Packets:      stat.Packets,
			Drops:        stat.Drops,
			Errors:       stat.Errors,
			Scrambled:    stat.Scrambled,
			PidType:      pidType,
		})
		for _, position := range stat.Positions {
			positionRows = append(positionRows, sqlcgen.InsertDropPositionParams{
				MediaAssetID: assetID,
				ByteOffset:   position.ByteOffset,
				Pid:          int32(pid),
				ElapsedMs:    position.ElapsedMs,
			})
		}
	}
	for start := 0; start < len(statRows); start += scanBatchSize {
		if err := execBatch(q.InsertDropStat(ctx, statRows[start:min(start+scanBatchSize, len(statRows))])); err != nil {
			return fmt.Errorf("inserting drop_stats batch: %w", err)
		}
	}
	for start := 0; start < len(positionRows); start += scanBatchSize {
		if err := execBatch(q.InsertDropPosition(ctx, positionRows[start:min(start+scanBatchSize, len(positionRows))])); err != nil {
			return fmt.Errorf("inserting drop_positions batch: %w", err)
		}
	}
	return nil
}

type batchResults interface {
	Exec(func(int, error))
	Close() error
}

func execBatch(batch batchResults) error {
	var firstErr error
	batch.Exec(func(i int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d: %w", i, err)
		}
	})
	if err := batch.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing batch: %w", err)
	}
	return firstErr
}
