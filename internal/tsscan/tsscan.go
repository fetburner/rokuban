// Package tsscan runs full-file TS scans after media assets have been committed.
package tsscan

import (
	"context"
	"fmt"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobs"
)

// ScanArgs identifies an original recording to scan.
type ScanArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind returns the River job kind for a single TS scan.
func (ScanArgs) Kind() string { return "ts_scan" }

// InsertOpts routes scans to the dedicated queue and merges pending work by recording.
func (ScanArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: jobs.TSScanQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: jobs.PendingJobStates(),
		},
	}
}

// ReconcileArgs requests one page of original assets that need a scan.
type ReconcileArgs struct {
	AfterRecordingID int64 `json:"after_recording_id,omitempty"`
}

// Kind returns the River job kind for the scan reconciliation pass.
func (ReconcileArgs) Kind() string { return "ts_scan_reconcile" }

// InsertOpts routes reconciliation to the scan queue and merges pending work for the same page.
func (ReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: jobs.TSScanQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: jobs.PendingJobStates(),
		},
	}
}

// EnqueueScan inserts a best-effort hint for the committed original of recordingID.
// The periodic reconcile pass remains the source of truth if this insert fails.
func EnqueueScan(ctx context.Context, client *river.Client[pgx5.Tx], recordingID int64) error {
	if _, err := client.Insert(ctx, ScanArgs{RecordingID: recordingID}, nil); err != nil {
		return fmt.Errorf("inserting TS scan for recording %d: %w", recordingID, err)
	}
	return nil
}
