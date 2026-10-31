// Package tsscan runs full-file TS scans after media assets have been committed.
package tsscan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

// ScanArgs identifies an original recording to scan.
type ScanArgs struct {
	RecordingID int64 `json:"recording_id"`
	// Store は原本のコピーがある置き場。投入側が選び、物理キュー `ts_scan_{store}` を決める。
	Store string `json:"store"`
}

// QueueStore returns the store whose queue runs this scan.
func (a ScanArgs) QueueStore() string { return a.Store }

// Kind returns the job kind for a single TS scan.
func (ScanArgs) Kind() string { return "ts_scan" }

// Spec returns the scan queue, six-hour timeout, default retry limit, and coalescing hint.
// The timeout is the attempt's context deadline. Unverified: the
// longest original and the read throughput were not measured. If six hours is too short,
// a long original's scan times out and is retried forever. A scan is idempotent and
// restartable, so a timeout only costs a retry.
func (ScanArgs) Spec() jobq.Spec {
	return jobq.Spec{
		Queue: jobs.TSScanQueue,
		Timeout: func(jobq.Args) time.Duration {
			return 6 * time.Hour
		},
		Coalesce: true,
		Priority: 1,
	}
}

// ReconcileArgs requests one page of original assets that need a scan.
type ReconcileArgs struct {
	AfterRecordingID int64 `json:"after_recording_id,omitempty"`
}

// Kind returns the job kind for the scan reconciliation pass.
func (ReconcileArgs) Kind() string { return "ts_scan_reconcile" }

// Spec returns the scan queue, five-minute timeout, and coalescing hint.
// Failed passes are not retried; the next periodic enqueue retries them.
func (ReconcileArgs) Spec() jobq.Spec {
	return jobq.Spec{
		Queue: jobs.CleanupQueue,
		Timeout: func(jobq.Args) time.Duration {
			return 5 * time.Minute
		},
		MaxAttempts: 1,
		Coalesce:    true,
		Priority:    1,
	}
}

var (
	_ jobq.Args = ScanArgs{}
	_ jobq.Args = ReconcileArgs{}
)

// EnqueueScan inserts a best-effort hint for the committed original of recordingID.
// The periodic reconcile pass remains the source of truth if this insert fails.
func EnqueueScan(ctx context.Context, enqueuer jobq.Enqueuer, recordingID int64, storeName string) error {
	if enqueuer == nil {
		return errors.New("TS scan enqueuer is nil")
	}
	if _, err := enqueuer.Enqueue(ctx, ScanArgs{RecordingID: recordingID, Store: storeName}); err != nil {
		return fmt.Errorf("inserting TS scan for recording %d: %w", recordingID, err)
	}
	return nil
}
