package tsscan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/inplace"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/testutil"
)

func workContext(t *testing.T, pool *pgxpool.Pool) context.Context {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}
	return rivertest.WorkContext(context.Background(), client)
}

func insertTestRecordingWithEventID(t *testing.T, pool *pgxpool.Pool, eventID int32) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source: "manual", Site: "default", NetworkID: 32736, ServiceID: 1024, EventID: eventID,
		ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test",
		ProgramStartAt: time.Now(), ProgramDurationMs: 1800000, Status: "finished",
	})
	if err != nil {
		t.Fatalf("inserting test recording: %v", err)
	}
	return id
}

func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID, Kind: db.AssetKindOriginal, RelPath: relPath, SizeBytes: int64(len(content)),
	})
	if err != nil {
		t.Fatalf("seeding original media_asset: %v", err)
	}
	return id
}

// pcrPacket builds a minimal TS packet carrying a PCR (90,000 ticks = 1 second).
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

// TestScanWorker_SizeMismatchFailsWithoutWriting: the DB size differs from the file.
func TestScanWorker_SizeMismatchFailsWithoutWriting(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := insertTestRecordingWithEventID(t, pool, 12930)
	mediaDir := t.TempDir()
	data := append(pcrPacket(0x0100, 0, 900000), pcrPacket(0x0100, 2, 990000)...)
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "mismatch/original.m2ts", data)
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET size_bytes = $2 WHERE id = $1`, assetID, len(data)+1); err != nil {
		t.Fatalf("changing DB size: %v", err)
	}
	job := &river.Job[ScanArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ScanArgs{RecordingID: recordingID},
	}
	if err := (&ScanWorker{Pool: pool, MediaDir: mediaDir}).Work(context.Background(), job); err == nil {
		t.Fatal("ScanWorker.Work() succeeded with DB size != file size, want error")
	}
	stats, positions := readTSScanRows(t, pool, assetID)
	if len(stats) != 0 || len(positions) != 0 {
		t.Errorf("rows written on mismatch: stats=%v positions=%v", stats, positions)
	}
	var markers int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_asset_ts_scans WHERE media_asset_id = $1`, assetID).Scan(&markers); err != nil {
		t.Fatalf("counting markers: %v", err)
	}
	if markers != 0 {
		t.Errorf("scan markers on mismatch = %d, want 0", markers)
	}
}

// TestReconcileWorker_RecordsLastPass: a completed pass sets the freshness gauge.
func TestReconcileWorker_RecordsLastPass(t *testing.T) {
	pool := testutil.SetupDB(t)
	metrics.TSScanReconcileLastPass.Set(0)
	job := &river.Job[ReconcileArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25}}
	if err := (&ReconcileWorker{Pool: pool}).Work(workContext(t, pool), job); err != nil {
		t.Fatalf("ReconcileWorker.Work: %v", err)
	}
	if got := promtestutil.ToFloat64(metrics.TSScanReconcileLastPass); got < float64(time.Now().Add(-time.Minute).Unix()) {
		t.Errorf("last pass gauge = %v, want a current timestamp", got)
	}
}

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
	worker := &ScanWorker{Pool: pool, MediaDir: mediaDir}
	job := &river.Job[ScanArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ScanArgs{RecordingID: recordingID},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("ScanWorker.Work: %v", err)
	}
}

// TestTSScan_InPlaceOriginalAndSizeChangeReplaceStatistics covers in-place imports
// and the same asset ID being reused after its file size changes.
func TestTSScan_InPlaceOriginalAndSizeChangeReplaceStatistics(t *testing.T) {
	pool := testutil.SetupDB(t)
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
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	recordingID := insertTestRecordingWithEventID(t, pool, 12940)
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

	worker := &ScanWorker{Pool: pool, MediaDir: mediaDir}
	job := &river.Job[ScanArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ScanArgs{RecordingID: recordingID},
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

func TestTSScanReconcile_EnqueuesOnlyUnmeasuredCurrentOriginals(t *testing.T) {
	pool := testutil.SetupDB(t)
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

	w := &ReconcileWorker{Pool: pool}
	job := &river.Job[ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ReconcileArgs{},
	}
	if err := w.Work(workContext(t, pool), job); err != nil {
		t.Fatalf("ReconcileWorker.Work: %v", err)
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

func TestTSScanReconcile_ContinuationSurvivesFreshWorker(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := insertTestRecordingWithEventID(t, pool, 12911)
	secondID := insertTestRecordingWithEventID(t, pool, 12912)
	seedOriginalAsset(t, pool, mediaDir, firstID, "ts-scan/window-first.m2ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "ts-scan/window-second.m2ts", []byte("second"))
	firstWorker := &ReconcileWorker{Pool: pool, RowLimit: 1}
	firstJob := &river.Job[ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ReconcileArgs{},
	}
	ctx := workContext(t, pool)
	if err := firstWorker.Work(ctx, firstJob); err != nil {
		t.Fatalf("first reconcile pass: %v", err)
	}
	if got := countTSScanJobs(t, pool, firstID); got != 1 {
		t.Fatalf("first recording jobs after first pass = %d, want 1", got)
	}
	if got := countTSScanJobs(t, pool, secondID); got != 0 {
		t.Fatalf("second recording jobs after first pass = %d, want 0", got)
	}
	var continuationAfter int64
	if err := pool.QueryRow(ctx, `
		SELECT (args->>'after_recording_id')::bigint
		FROM river_job
		WHERE kind = 'ts_scan_reconcile'
		  AND state IN ('available', 'retryable', 'running')
		ORDER BY id DESC
		LIMIT 1`).Scan(&continuationAfter); err != nil {
		t.Fatalf("reading reconcile continuation: %v", err)
	}
	if continuationAfter != firstID {
		t.Fatalf("continuation cursor = %d, want first recording ID %d", continuationAfter, firstID)
	}

	// KEDA starts a fresh --once process for each queue item. The next page must
	// come from the continuation args, not state held by the previous worker.
	secondWorker := &ReconcileWorker{Pool: pool, RowLimit: 1}
	secondJob := &river.Job[ReconcileArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 25},
		Args:   ReconcileArgs{AfterRecordingID: continuationAfter},
	}
	if err := secondWorker.Work(ctx, secondJob); err != nil {
		t.Fatalf("second reconcile page: %v", err)
	}
	if got := countTSScanJobs(t, pool, secondID); got != 1 {
		t.Fatalf("second recording jobs after continuation page = %d, want 1", got)
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
