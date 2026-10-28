package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

func TestPublishEncodedDoesNotResurrectPendingCopyClaimedForDeletion(t *testing.T) {
	pool := setupTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	seedEncodePolicy(t, pool, recordingID, []string{profile})
	media := pgstore.NewMediaWithStores(pool, "s3main", dbtest.StoreName, "s3main")
	q := sqlcgen.New(pool)
	if _, err := q.CreateRecordingEncodeAttemptRunning(ctx, sqlcgen.CreateRecordingEncodeAttemptRunningParams{
		RecordingID: recordingID, Profile: profile,
	}); err != nil {
		t.Fatalf("seeding running encode attempt: %v", err)
	}
	key := fmt.Sprintf("encoded/race-%d.mp4", recordingID)
	if err := media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
		RecordingID: recordingID, Kind: store.AssetKindEncoded, Profile: &profile, RelPath: key, SizeBytes: 1,
	}); err != nil {
		t.Fatalf("preparing pending encoded copy: %v", err)
	}
	copyRow, err := q.GetMediaAssetCopyByPath(ctx, sqlcgen.GetMediaAssetCopyByPathParams{Store: "s3main", RelPath: key})
	if err != nil {
		t.Fatalf("loading pending encoded copy: %v", err)
	}

	cleanupTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning pending-copy cleanup claim: %v", err)
	}
	defer func() { _ = cleanupTx.Rollback(context.Background()) }()
	claimed, err := sqlcgen.New(cleanupTx).ClaimAgedPendingMediaAssetCopyByID(ctx, sqlcgen.ClaimAgedPendingMediaAssetCopyByIDParams{
		ID: copyRow.ID, CreatedBefore: time.Now().Add(time.Minute),
	})
	if err != nil || claimed != 1 {
		t.Fatalf("claiming pending copy for cleanup = (%d, %v), want (1, nil)", claimed, err)
	}

	type publishResult struct {
		published bool
		err       error
		calls     int
	}
	resultCh := make(chan publishResult, 1)
	go func() {
		calls := 0
		published, publishErr := media.PublishEncoded(ctx, store.EncodedPublishInput{
			RecordingID: recordingID, Profile: profile, Attempt: 1, RelPath: key,
			SizeBytes: 1, ObservedRelPath: "",
		}, func() error { calls++; return nil })
		resultCh <- publishResult{published: published, err: publishErr, calls: calls}
	}()

	if err := waitForBlockedMediaAssetCopyUpdate(ctx, pool); err != nil {
		t.Fatalf("publisher did not reach pending-to-active update while cleanup held the row: %v", err)
	}
	if err := cleanupTx.Commit(ctx); err != nil {
		t.Fatalf("committing pending-copy cleanup claim: %v", err)
	}
	var result publishResult
	select {
	case result = <-resultCh:
	case <-ctx.Done():
		t.Fatalf("waiting for encoded publisher after cleanup commit: %v", ctx.Err())
	}
	if result.err == nil || result.published || result.calls != 0 {
		t.Fatalf("publish result = (%t, %v, callback calls %d), want failed CAS without publication", result.published, result.err, result.calls)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, copyRow.ID).Scan(&state); err != nil {
		t.Fatalf("reading reclaimed copy state: %v", err)
	}
	if state != "deleting" {
		t.Fatalf("copy state after failed publication = %q, want deleting", state)
	}
}

func waitForBlockedMediaAssetCopyUpdate(ctx context.Context, pool *pgxpool.Pool) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid()
				  AND datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%UPDATE media_asset_copies%'
			)`).Scan(&blocked); err != nil {
			return fmt.Errorf("checking blocked copy update: %w", err)
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
