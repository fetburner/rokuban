package ingest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/tsscan"
)

type tsScanStatRow struct {
	pid       int32
	packets   int64
	drops     int64
	errors    int64
	scrambled int64
	pidType   *string
}

type tsScanPositionRow struct {
	offset  int64
	pid     int32
	elapsed *int64
}

func pcrPacket(pid, cc int, base uint64) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte((pid >> 8) & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x30 | byte(cc&0x0F)
	pkt[4] = 7
	pkt[5] = 0x10
	pkt[6] = byte(base >> 25)
	pkt[7] = byte(base >> 17)
	pkt[8] = byte(base >> 9)
	pkt[9] = byte(base >> 1)
	pkt[10] = byte(base<<7) | 0x7E
	return pkt
}

func runTSScan(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64) {
	t.Helper()
	worker := &tsscan.ScanWorker{ScanStores: tsscan.ScanStores{Media: pgstoretest.Media(pool)}, MediaDir: mediaDir}
	job := tsscan.ScanArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), worker, job), "ScanWorker.Work")
}

// TestTSScan_StatisticsAreWrittenAfterIngestCommit verifies ingest leaves TS
// statistics absent until the asynchronous scan records them.
func TestTSScan_StatisticsAreWrittenAfterIngestCommit(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool)
	data := append(
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000)...,
	)
	tei := pcrPacket(0x0110, 0, 1080000)
	tei[1] |= 0x80
	data = append(data, tei...)
	scrambled := pcrPacket(0x0120, 0, 1170000)
	scrambled[3] |= 0x40
	data = append(data, scrambled...)

	mediaDir := t.TempDir()
	relPath := "scan-parity/original.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755), "creating original directory")
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	testutil.RequireNoError(t, os.WriteFile(tempPath, data, 0o600), "writing ingest temporary file")
	seedIngestTempForCommit(t, pool, recordingID, mediaDir, tempPath)
	testutil.RequireNoError(t, (&IngestWorker{Publisher: pgstoretest.Media(pool), MediaDir: mediaDir, Stores: Stores{Precheck: pgstoretest.Media(pool)}}).commit(
		context.Background(), recordingID, relPath, tempPath, fullPath, int64(len(data)), nil, uuid.Nil, 0,
	), "IngestWorker.commit")

	var assetID int64
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, recordingID,
	).Scan(&assetID), "querying original media asset")
	gotStats, gotPositions := readTSScanRows(t, pool, assetID)
	if len(gotStats) != 0 || len(gotPositions) != 0 {
		t.Fatalf("drop rows after ingest commit = stats:%#v positions:%#v, want none before scan", gotStats, gotPositions)
	}
	var scanRows int
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1`, assetID,
	).Scan(&scanRows), "querying scan marker after ingest commit")
	if scanRows != 0 {
		t.Fatalf("scan markers after ingest commit = %d, want 0", scanRows)
	}

	runTSScan(t, pool, mediaDir, recordingID)
	gotStats, gotPositions = readTSScanRows(t, pool, assetID)
	// 期待値はフィクスチャから決まるリテラル（実装と同じ tsstat.Counter では作らない）。
	// PID 0x100 は cc 0 → 2 で 1 件欠落し、欠落は 2 パケット目（byte 188）、
	// 直前の PCR との差 90000 tick = 1000ms。
	wantStats := []tsScanStatRow{
		{pid: 0x100, packets: 2, drops: 1},
		{pid: 0x110, packets: 1, errors: 1},
		{pid: 0x120, packets: 1, scrambled: 1},
	}
	wantElapsed := int64(1000)
	wantPositions := []tsScanPositionRow{{offset: 188, pid: 0x100, elapsed: &wantElapsed}}
	if !reflect.DeepEqual(gotStats, wantStats) {
		t.Errorf("scan drop_stats = %#v, want values %#v", gotStats, wantStats)
	}
	if !reflect.DeepEqual(gotPositions, wantPositions) {
		t.Errorf("scan drop_positions = %#v, want values %#v", gotPositions, wantPositions)
	}
	if got := scannedSize(t, pool, assetID); got != int64(len(data)) {
		t.Errorf("scanned_size_bytes = %d, want %d", got, len(data))
	}

	// A duplicate or stale job for an already measured size is a no-op, even if
	// the file disappeared after the successful scan.
	testutil.RequireNoError(t, os.Remove(fullPath), "removing original after scan")
	runTSScan(t, pool, mediaDir, recordingID)
}

func TestIngestFollowupEnqueuesTSScanAndCMDetectionHints(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = 12920 })
	mediaDir := t.TempDir()
	seedOriginalAsset(t, pool, mediaDir, recordingID, "ingest-hint/original.m2ts", []byte("committed"))
	dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) {
		p.KeepOriginal = "until_encoded"
		p.EncodeProfiles = []string{"h264"}
		p.CMDetect = true
	})
	testutil.MustExec(t, pool, "setting recording duration", `
		UPDATE recordings
		SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
		WHERE id = $1`, recordingID)
	fake := mirakctest.New(t)
	fake.SetRecords([]mirakc.Record{{ID: "edge-1290"}})

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	w := &IngestWorker{Publisher: pgstoretest.Media(pool), Enqueuer: enqueuer, MediaDir: mediaDir, MediaStoreName: dbtest.StoreName, CMDetectionStore: pgstoretest.Analysis(pool), Stores: Stores{Precheck: pgstoretest.Media(pool)}}
	w.enqueueIngestFollowups(context.Background(), mirakc.NewClient(fake.URL(), nil), "edge-1290", recordingID,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	var deleteFound bool
	for _, call := range fake.Calls() {
		if call.Method == "DELETE" && call.Path == "/api/recording/records/edge-1290" && call.Query == "purge=true" {
			deleteFound = true
		}
	}
	if !deleteFound {
		t.Errorf("DeleteRecord calls = %#v, want DELETE record with purge=true", fake.Calls())
	}
	if got := countTSScanJobs(t, enqueuer, recordingID); got != 1 {
		t.Fatalf("TS scan jobs after ingest followup = %d, want 1", got)
	}
	cmJobs := enqueuedArgs[jobs.CMDetectJobArgs](enqueuer, jobs.CMDetectJobArgs{}.Kind())
	cmJobFound := false
	for _, args := range cmJobs {
		if args.RecordingID != recordingID {
			continue
		}
		if args.Store != dbtest.StoreName {
			t.Errorf("recording %d CM detection store = %q, want %q", recordingID, args.Store, dbtest.StoreName)
		}
		if args.RecordingDurationMs != 45*60*1000 {
			t.Errorf("recording %d job duration = %dms, want 2700000ms", recordingID, args.RecordingDurationMs)
		}
		cmJobFound = true
		break
	}
	if !cmJobFound {
		t.Fatalf("no CM detection job for recording %d", recordingID)
	}
	thumbs := countEnqueuedForRecording(t, enqueuer, "thumbnail_reconcile", recordingID)
	if thumbs != 1 {
		t.Errorf("thumbnail_reconcile jobs after ingest followup = %d, want 1", thumbs)
	}
}

func readTSScanRows(t *testing.T, pool *pgxpool.Pool, assetID int64) ([]tsScanStatRow, []tsScanPositionRow) {
	t.Helper()
	ctx := context.Background()
	var stats []tsScanStatRow
	rows, err := pool.Query(ctx, `
		SELECT pid, packets, drops, errors, scrambled, pid_type
		FROM drop_stats WHERE media_asset_id = $1 ORDER BY pid`, assetID)
	testutil.RequireNoError(t, err, "querying drop_stats")
	for rows.Next() {
		var row tsScanStatRow
		if err := rows.Scan(&row.pid, &row.packets, &row.drops, &row.errors, &row.scrambled, &row.pidType); err != nil {
			rows.Close()
			t.Fatalf("scanning drop_stats: %v", err)
		}
		stats = append(stats, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterating drop_stats: %v", err)
	}
	rows.Close()

	var positions []tsScanPositionRow
	rows, err = pool.Query(ctx, `
		SELECT byte_offset, pid, elapsed_ms
		FROM drop_positions WHERE media_asset_id = $1 ORDER BY byte_offset`, assetID)
	testutil.RequireNoError(t, err, "querying drop_positions")
	for rows.Next() {
		var row tsScanPositionRow
		if err := rows.Scan(&row.offset, &row.pid, &row.elapsed); err != nil {
			rows.Close()
			t.Fatalf("scanning drop_positions: %v", err)
		}
		positions = append(positions, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterating drop_positions: %v", err)
	}
	rows.Close()
	return stats, positions
}

func scannedSize(t *testing.T, pool *pgxpool.Pool, assetID int64) int64 {
	t.Helper()
	var size int64
	testutil.RequireNoError(t, pool.QueryRow(context.Background(),
		`SELECT scanned_size_bytes FROM media_asset_ts_scans WHERE media_asset_id = $1`, assetID,
	).Scan(&size), "querying scan size")
	return size
}

func countTSScanJobs(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64) int {
	return countEnqueuedForRecording(t, enqueuer, (tsscan.ScanArgs{}).Kind(), recordingID)
}
