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

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/tsstat"
)

const (
	reconcileRowLimit = 1000
)

// ScanWorker reads an active original and replaces its TS statistics atomically.
type ScanWorker struct {
	Media    store.TSScan
	MediaDir string
}

// Work reads an original from byte zero, then replaces its statistics and scan marker
// in one database transaction. ScanArgs.Spec().Timeout bounds the attempt so the queue
// adapter can recover it after process death. A size change during the read leaves the
// original eligible for the next reconcile pass.
func (w *ScanWorker) Work(ctx context.Context, args ScanArgs) error {
	started := time.Now()
	result := "failure"
	defer func() {
		metrics.TSScanDuration.Observe(time.Since(started).Seconds())
		metrics.TSScanJobs.WithLabelValues(result).Inc()
	}()

	asset, err := w.Media.GetActiveOriginalForTSScan(ctx, args.RecordingID)
	if errors.Is(err, store.ErrNotFound) {
		result = "success"
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading active original for recording %d: %w", args.RecordingID, err)
	}

	counter, size, err := scanFile(ctx, w.MediaDir, asset.RelPath, asset.SizeBytes)
	if err != nil {
		return fmt.Errorf("scanning original media asset %d: %w", asset.ID, err)
	}

	metrics.TSScanDroppedPackets.Add(float64(counter.TotalDrops()))
	metrics.TSScanErrorPackets.Add(float64(counter.TotalErrors()))
	metrics.TSScanScrambledPackets.Add(float64(counter.TotalScrambled()))

	stats, positions := scanResults(counter)
	if _, err := w.Media.ReplaceScan(ctx, asset.ID, size, stats, positions); err != nil {
		return fmt.Errorf("replacing TS scan for media asset %d: %w", asset.ID, err)
	}
	result = "success"
	return nil
}

// ReconcileWorker enqueues missing or stale scans from the current database state.
type ReconcileWorker struct {
	Enqueuer jobq.Enqueuer
	Media    store.TSScan
	RowLimit int32
}

// Work enqueues one bounded window and persists the next page in a continuation job.
func (w *ReconcileWorker) Work(ctx context.Context, args ReconcileArgs) error {
	if w.Enqueuer == nil {
		return fmt.Errorf("TS scan reconcile: job enqueuer is nil")
	}
	rowLimit := w.RowLimit
	if rowLimit <= 0 {
		rowLimit = reconcileRowLimit
	}
	rows, err := w.Media.ListMissingTSScanRecordings(ctx, args.AfterRecordingID, rowLimit)
	if err != nil {
		return fmt.Errorf("listing originals that need TS scans: %w", err)
	}
	for _, recordingID := range rows {
		if err := EnqueueScan(ctx, w.Enqueuer, recordingID); err != nil {
			slog.Error("ts_scan_reconcile: failed to enqueue scan", "recording_id", recordingID, "err", err)
		}
	}

	var continuationAfter int64
	if int32(len(rows)) >= rowLimit {
		continuationAfter = rows[len(rows)-1]
		if _, err := w.Enqueuer.Enqueue(ctx, ReconcileArgs{AfterRecordingID: continuationAfter}); err != nil {
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

func scanResults(counter *tsstat.Counter) ([]store.RecordingDropStat, []store.RecordingDropPosition) {
	stats := counter.Stats()
	pids := make([]int, 0, len(stats))
	for pid := range stats {
		pids = append(pids, pid)
	}
	slices.Sort(pids)

	statRows := make([]store.RecordingDropStat, 0, len(pids))
	positionRows := make([]store.RecordingDropPosition, 0)
	for _, pid := range pids {
		stat := stats[pid]
		var pidType *string
		if stat.Type != "" {
			t := stat.Type
			pidType = &t
		}
		statRows = append(statRows, store.RecordingDropStat{
			Pid: int32(pid), Packets: stat.Packets, Drops: stat.Drops,
			Errors: stat.Errors, Scrambled: stat.Scrambled, PidType: pidType,
		})
		for _, position := range stat.Positions {
			positionRows = append(positionRows, store.RecordingDropPosition{
				ByteOffset: position.ByteOffset, Pid: int32(pid), ElapsedMs: position.ElapsedMs,
			})
		}
	}
	return statRows, positionRows
}
