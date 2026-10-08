package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/tsscan"
	"github.com/fetburner/rokuban/internal/tsstat"
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

func runTSScan(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64) {
	t.Helper()
	worker := &tsscan.ScanWorker{Pool: pool, MediaDir: mediaDir}
	job := &river.Job[tsscan.ScanArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   tsscan.ScanArgs{RecordingID: recordingID},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("ScanWorker.Work: %v", err)
	}
}

// TestTSScan_StatisticsMatchIngestCommit fixes the temporary period when ingest
// and ts_scan both collect statistics from the same original.
func TestTSScan_StatisticsMatchIngestCommit(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
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
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating original directory: %v", err)
	}
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("writing ingest temporary file: %v", err)
	}
	counter := tsstat.NewCounter(&bytes.Buffer{})
	if n, err := counter.Write(data); err != nil || n != len(data) {
		t.Fatalf("counter.Write() = %d, %v; want %d, nil", n, err, len(data))
	}
	if err := (&IngestWorker{Pool: pool, MediaDir: mediaDir}).commit(
		context.Background(), recordingID, relPath, tempPath, fullPath, int64(len(data)), counter,
	); err != nil {
		t.Fatalf("IngestWorker.commit: %v", err)
	}

	var assetID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, recordingID,
	).Scan(&assetID); err != nil {
		t.Fatalf("querying original media asset: %v", err)
	}
	wantStats, wantPositions := readTSScanRows(t, pool, assetID)
	if len(wantPositions) != 1 || wantPositions[0].offset != 188 || wantPositions[0].elapsed == nil || *wantPositions[0].elapsed != 1000 {
		t.Fatalf("ingest positions = %#v, want one 1-second drop at byte 188", wantPositions)
	}

	runTSScan(t, pool, mediaDir, recordingID)
	gotStats, gotPositions := readTSScanRows(t, pool, assetID)
	if !reflect.DeepEqual(gotStats, wantStats) {
		t.Errorf("scan drop_stats = %#v, want ingest values %#v", gotStats, wantStats)
	}
	if !reflect.DeepEqual(gotPositions, wantPositions) {
		t.Errorf("scan drop_positions = %#v, want ingest values %#v", gotPositions, wantPositions)
	}
	if got := scannedSize(t, pool, assetID); got != int64(len(data)) {
		t.Errorf("scanned_size_bytes = %d, want %d", got, len(data))
	}

	// A duplicate or stale job for an already measured size is a no-op, even if
	// the file disappeared after the successful scan.
	if err := os.Remove(fullPath); err != nil {
		t.Fatalf("removing original after scan: %v", err)
	}
	runTSScan(t, pool, mediaDir, recordingID)
}

func TestIngestFollowupEnqueuesTSScanHint(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecordingWithEventID(t, pool, 12920)
	mediaDir := t.TempDir()
	seedOriginalAsset(t, pool, mediaDir, recordingID, "ingest-hint/original.m2ts", []byte("committed"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/recording/records/edge-1290" || r.URL.Query().Get("purge") != "true" {
			t.Errorf("DeleteRecord request = %s %s, want DELETE record with purge=true", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	w := &IngestWorker{Pool: pool, MediaDir: mediaDir}
	w.enqueueIngestFollowups(riverWorkContext(t, pool), mirakc.NewClient(srv.URL, nil), "edge-1290", recordingID,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := countTSScanJobs(t, pool, recordingID); got != 1 {
		t.Fatalf("TS scan jobs after ingest followup = %d, want 1", got)
	}
}

func TestTSScanArgsAndPeriodicRegistration(t *testing.T) {
	scanArgs := tsscan.ScanArgs{RecordingID: 123}
	if got, want := scanArgs.Kind(), "ts_scan"; got != want {
		t.Errorf("ScanArgs.Kind() = %q, want %q", got, want)
	}
	if got := scanArgs.InsertOpts().Queue; got != "ts_scan" {
		t.Errorf("ScanArgs queue = %q, want %q", got, "ts_scan")
	}
	reconcileArgs := tsscan.ReconcileArgs{AfterRecordingID: 456}
	if got, want := reconcileArgs.Kind(), "ts_scan_reconcile"; got != want {
		t.Errorf("ReconcileArgs.Kind() = %q, want %q", got, want)
	}
	if got := reconcileArgs.InsertOpts().Queue; got != "ts_scan" {
		t.Errorf("ReconcileArgs queue = %q, want %q", got, "ts_scan")
	}

	registered, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:    true,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig with periodic TS scan: %v", err)
	}
	if len(registered.PeriodicJobs) != 1 {
		t.Fatalf("PeriodicJobs = %d, want one TS scan reconcile job", len(registered.PeriodicJobs))
	}
	disabled, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:    false,
		TSScanReconcile: true,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig with periodic jobs disabled: %v", err)
	}
	if len(disabled.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs with periodic_jobs=false = %d, want 0", len(disabled.PeriodicJobs))
	}
	if jobs.RequiresSiteBinding([]string{jobs.TSScanQueue}) {
		t.Error("ts_scan must be site independent")
	}
}

func readTSScanRows(t *testing.T, pool *pgxpool.Pool, assetID int64) ([]tsScanStatRow, []tsScanPositionRow) {
	t.Helper()
	ctx := context.Background()
	var stats []tsScanStatRow
	rows, err := pool.Query(ctx, `
		SELECT pid, packets, drops, errors, scrambled, pid_type
		FROM drop_stats WHERE media_asset_id = $1 ORDER BY pid`, assetID)
	if err != nil {
		t.Fatalf("querying drop_stats: %v", err)
	}
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
	if err != nil {
		t.Fatalf("querying drop_positions: %v", err)
	}
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
	if err := pool.QueryRow(context.Background(),
		`SELECT scanned_size_bytes FROM media_asset_ts_scans WHERE media_asset_id = $1`, assetID,
	).Scan(&size); err != nil {
		t.Fatalf("querying scan size: %v", err)
	}
	return size
}

func countTSScanJobs(t *testing.T, pool *pgxpool.Pool, recordingID int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM river_job
		WHERE kind = 'ts_scan' AND (args->>'recording_id')::bigint = $1`, recordingID,
	).Scan(&count); err != nil {
		t.Fatalf("counting ts_scan jobs: %v", err)
	}
	return count
}
