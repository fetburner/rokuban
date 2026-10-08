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
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/inplace"
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

// TestTSScan_InPlaceOriginalAndSizeChangeReplaceStatistics covers in-place imports
// and the same asset ID being reused after its file size changes.
func TestTSScan_InPlaceOriginalAndSizeChangeReplaceStatistics(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	relPath := "inplace/scan.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating original directory: %v", err)
	}
	firstBytes := append(
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000)...,
	)
	if err := os.WriteFile(fullPath, firstBytes, 0o600); err != nil {
		t.Fatalf("writing in-place original: %v", err)
	}
	input := inplace.Input{
		Recording: inplace.Recording{
			Source: "manual", Site: "default",
			NetworkID: -1290, ServiceID: -1290, EventID: -1290,
			ServiceName: "in-place scan", ChannelType: "GR", Channel: "unknown",
			Title: "in-place scan", ProgramStartAt: time.Unix(1_700_000_000, 0),
			ProgramDurationMs: 60000, Status: "finished",
		},
		Assets: []inplace.Asset{{Kind: db.AssetKindOriginal, RelPath: relPath}},
	}
	first, err := inplace.Register(ctx, pool, mediaDir, input)
	if err != nil {
		t.Fatalf("first inplace.Register: %v", err)
	}
	var statsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM drop_stats WHERE media_asset_id = $1`, first.AssetIDs[0]).Scan(&statsBefore); err != nil {
		t.Fatalf("counting in-place stats before scan: %v", err)
	}
	if statsBefore != 0 {
		t.Fatalf("in-place original already has %d drop_stats rows, want none", statsBefore)
	}
	runTSScan(t, pool, mediaDir, first.RecordingID)
	firstStats, _ := readTSScanRows(t, pool, first.AssetIDs[0])
	if len(firstStats) != 1 || firstStats[0].pid != 0x0100 || firstStats[0].drops != 1 {
		t.Fatalf("first scan stats = %#v, want one drop for PID 256", firstStats)
	}

	secondBytes := append([]byte{},
		pcrPacket(0x0200, 0, 900000)...,
	)
	secondBytes = append(secondBytes, pcrPacket(0x0200, 1, 990000)...)
	secondBytes = append(secondBytes, pcrPacket(0x0200, 2, 1080000)...)
	if err := os.WriteFile(fullPath, secondBytes, 0o600); err != nil {
		t.Fatalf("replacing in-place original: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE id = $1`, first.AssetIDs[0],
	); err != nil {
		t.Fatalf("marking original as removed before rescue: %v", err)
	}
	second, err := inplace.Register(ctx, pool, mediaDir, input)
	if err != nil {
		t.Fatalf("second inplace.Register: %v", err)
	}
	if second.RecordingID != first.RecordingID || second.AssetIDs[0] != first.AssetIDs[0] {
		t.Fatalf("in-place identity changed: first=%+v second=%+v", first, second)
	}
	runTSScan(t, pool, mediaDir, second.RecordingID)
	secondStats, secondPositions := readTSScanRows(t, pool, second.AssetIDs[0])
	if len(secondStats) != 1 || secondStats[0].pid != 0x0200 || secondStats[0].packets != 3 || secondStats[0].drops != 0 {
		t.Errorf("replacement stats = %#v, want only three packets for PID 512", secondStats)
	}
	if len(secondPositions) != 0 {
		t.Errorf("replacement positions = %#v, want none", secondPositions)
	}
	if got := scannedSize(t, pool, second.AssetIDs[0]); got != int64(len(secondBytes)) {
		t.Errorf("scanned_size_bytes = %d, want changed size %d", got, len(secondBytes))
	}
}

func TestTSScan_ReplacementRollsBackStatisticsAndMarkerTogether(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	recordingID := insertTestRecording(t, pool)
	mediaDir := t.TempDir()
	relPath := "rollback/original.m2ts"
	oldBytes := []byte("old")
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, relPath, oldBytes)
	if _, err := pool.Exec(ctx, `
		INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
		VALUES ($1, 700, 9, 2, 1, 3, 'old')`, assetID); err != nil {
		t.Fatalf("seeding old drop_stats: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms)
		VALUES ($1, 8, 700, 99)`, assetID); err != nil {
		t.Fatalf("seeding old drop_positions: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
		VALUES ($1, $2)`, assetID, len(oldBytes)); err != nil {
		t.Fatalf("seeding old scan marker: %v", err)
	}
	oldStats, oldPositions := readTSScanRows(t, pool, assetID)

	newBytes := append(
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000)...,
	)
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.WriteFile(fullPath, newBytes, 0o600); err != nil {
		t.Fatalf("replacing original: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET size_bytes = $2 WHERE id = $1`, assetID, len(newBytes)); err != nil {
		t.Fatalf("updating original size: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_ts_scan_position_insert() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected TS scan insert failure'; END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_ts_scan_position_insert
		BEFORE INSERT ON drop_positions
		FOR EACH ROW EXECUTE FUNCTION fail_ts_scan_position_insert()`); err != nil {
		t.Fatalf("creating failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_ts_scan_position_insert ON drop_positions`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS fail_ts_scan_position_insert()`)
	})

	worker := &tsscan.ScanWorker{Pool: pool, MediaDir: mediaDir}
	job := &river.Job[tsscan.ScanArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   tsscan.ScanArgs{RecordingID: recordingID},
	}
	if err := worker.Work(ctx, job); err == nil {
		t.Fatal("ScanWorker.Work() succeeded with failing drop_positions trigger, want error")
	}
	gotStats, gotPositions := readTSScanRows(t, pool, assetID)
	if !reflect.DeepEqual(gotStats, oldStats) {
		t.Errorf("drop_stats after rollback = %#v, want prior rows %#v", gotStats, oldStats)
	}
	if !reflect.DeepEqual(gotPositions, oldPositions) {
		t.Errorf("drop_positions after rollback = %#v, want prior rows %#v", gotPositions, oldPositions)
	}
	if got := scannedSize(t, pool, assetID); got != int64(len(oldBytes)) {
		t.Errorf("scanned_size_bytes after rollback = %d, want prior size %d", got, len(oldBytes))
	}
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

func TestTSScanReconcile_EnqueuesOnlyUnmeasuredCurrentOriginals(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	missingScanID := insertTestRecordingWithEventID(t, pool, 12901)
	staleScanID := insertTestRecordingWithEventID(t, pool, 12902)
	equalScanID := insertTestRecordingWithEventID(t, pool, 12903)
	trashedID := insertTestRecordingWithEventID(t, pool, 12904)
	missingFileID := insertTestRecordingWithEventID(t, pool, 12905)

	seedOriginalAsset(t, pool, mediaDir, missingScanID, "ts-scan/missing.m2ts", []byte("new"))
	staleAsset := seedOriginalAsset(t, pool, mediaDir, staleScanID, "ts-scan/stale.m2ts", []byte("changed"))
	equalAsset := seedOriginalAsset(t, pool, mediaDir, equalScanID, "ts-scan/equal.m2ts", []byte("equal"))
	trashedAsset := seedOriginalAsset(t, pool, mediaDir, trashedID, "ts-scan/trashed.m2ts", []byte("trash"))
	missingAssetID := seedOriginalAsset(t, pool, mediaDir, missingFileID, "ts-scan/missing-file.m2ts", []byte("gone"))
	if _, err := q.SoftDeleteRecording(ctx, trashedID); err != nil {
		t.Fatalf("soft deleting recording: %v", err)
	}
	for _, row := range []struct {
		assetID int64
		size    int64
	}{
		{assetID: staleAsset, size: 1},
		{assetID: equalAsset, size: int64(len("equal"))},
		{assetID: trashedAsset, size: 1},
		{assetID: missingAssetID, size: 1},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES ($1, $2)`,
			row.assetID, row.size,
		); err != nil {
			t.Fatalf("seeding scan record for %d: %v", row.assetID, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO missing_media_assets (media_asset_id) VALUES ($1)`, missingAssetID,
	); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	w := &tsscan.ReconcileWorker{Pool: pool}
	job := &river.Job[tsscan.ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   tsscan.ReconcileArgs{},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("ReconcileWorker.Work: %v", err)
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("second ReconcileWorker.Work: %v", err)
	}
	var got []int64
	rows, err := pool.Query(ctx, `
		SELECT (args->>'recording_id')::bigint
		FROM river_job
		WHERE kind = 'ts_scan'
		ORDER BY (args->>'recording_id')::bigint`)
	if err != nil {
		t.Fatalf("querying queued scan jobs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var recordingID int64
		if err := rows.Scan(&recordingID); err != nil {
			t.Fatalf("scanning queued recording ID: %v", err)
		}
		got = append(got, recordingID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating queued scan jobs: %v", err)
	}
	want := []int64{missingScanID, staleScanID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("queued scan recording IDs = %v, want %v (active unmeasured or stale only)", got, want)
	}
}

func TestTSScanReconcile_WindowRotatesPastUnchangedCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := insertTestRecordingWithEventID(t, pool, 12911)
	secondID := insertTestRecordingWithEventID(t, pool, 12912)
	seedOriginalAsset(t, pool, mediaDir, firstID, "ts-scan/window-first.m2ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "ts-scan/window-second.m2ts", []byte("second"))
	w := &tsscan.ReconcileWorker{Pool: pool, RowLimit: 1}
	job := &river.Job[tsscan.ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   tsscan.ReconcileArgs{},
	}
	ctx := riverWorkContext(t, pool)
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("first reconcile pass: %v", err)
	}
	if got := countTSScanJobs(t, pool, firstID); got != 1 {
		t.Fatalf("first recording jobs after first pass = %d, want 1", got)
	}
	if got := countTSScanJobs(t, pool, secondID); got != 0 {
		t.Fatalf("second recording jobs after first pass = %d, want 0", got)
	}
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("second reconcile pass: %v", err)
	}
	if got := countTSScanJobs(t, pool, secondID); got != 1 {
		t.Fatalf("second recording jobs after rotated pass = %d, want 1", got)
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
	reconcileArgs := tsscan.ReconcileArgs{}
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
