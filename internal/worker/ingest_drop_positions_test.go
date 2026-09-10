package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/tsstat"
)

// pcrPacket は adaptation field の PCR を持つ最小限の TS パケットを作る。
// PCR base は 90 kHz の時計なので、テストでは 90,000 tick を 1 秒として使う。
func pcrPacket(pid, cc int, base uint64) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte((pid >> 8) & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x30 | byte(cc&0x0F) // adaptation field + payload
	pkt[4] = 7                    // flags 1 byte + PCR 6 bytes
	pkt[5] = 0x10                 // PCR_flag
	pkt[6] = byte(base >> 25)
	pkt[7] = byte(base >> 17)
	pkt[8] = byte(base >> 9)
	pkt[9] = byte(base >> 1)
	pkt[10] = byte(base<<7) | 0x7E // base[0], reserved bits, extension[8]
	// PCR extension is zero; the remaining payload is already zero-filled.
	return pkt
}

type ingestBatchTracer struct {
	batchSizes []int
}

func (t *ingestBatchTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	t.batchSizes = append(t.batchSizes, len(data.Batch.QueuedQueries))
	return ctx
}

func (*ingestBatchTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*ingestBatchTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*ingestBatchTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (*ingestBatchTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func setupIngestBatchTracePool(t *testing.T) (*pgxpool.Pool, *ingestBatchTracer) {
	t.Helper()

	base := setupTestPool(t)
	config := base.Config()
	base.Close()

	tracer := &ingestBatchTracer{}
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("creating traced test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tracer
}

func TestIngestWorker_CommitDropPositions(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	recordingID := insertTestRecording(t, pool)
	data := append(
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000)...,
	)
	counter := tsstat.NewCounter(&bytes.Buffer{})
	if n, err := counter.Write(data); err != nil || n != len(data) {
		t.Fatalf("counter.Write() = %d, %v; want %d, nil", n, err, len(data))
	}
	mediaDir := t.TempDir()
	relPath := "test/drop-position.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating media directory: %v", err)
	}
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("creating ingest temporary file: %v", err)
	}

	if err := (&IngestWorker{Pool: pool}).commit(
		context.Background(), recordingID, relPath, tempPath, fullPath, int64(len(data)), counter,
	); err != nil {
		t.Fatalf("commit() error: %v", err)
	}

	var assetID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, recordingID,
	).Scan(&assetID); err != nil {
		t.Fatalf("querying original media asset: %v", err)
	}

	var drops int64
	if err := pool.QueryRow(context.Background(),
		`SELECT drops FROM drop_stats WHERE media_asset_id = $1 AND pid = $2`, assetID, 0x0100,
	).Scan(&drops); err != nil {
		t.Fatalf("querying drop_stats: %v", err)
	}
	if drops != 1 {
		t.Errorf("drops = %d, want 1", drops)
	}

	var offset, pid int64
	var elapsed *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT byte_offset, pid, elapsed_ms FROM drop_positions WHERE media_asset_id = $1`, assetID,
	).Scan(&offset, &pid, &elapsed); err != nil {
		t.Fatalf("querying drop_positions: %v", err)
	}
	if offset != 188 || pid != 0x0100 {
		t.Errorf("position = (offset=%d, pid=%d), want (188, 256)", offset, pid)
	}
	if elapsed == nil || *elapsed != 1000 {
		t.Errorf("elapsed_ms = %v, want 1000", elapsed)
	}
}

func TestIngestWorker_CommitDropStatsAndPositionsUsesTwoBatches(t *testing.T) {
	pool, tracer := setupIngestBatchTracePool(t)
	recordingID := insertTestRecording(t, pool)

	data := make([]byte, 0, 4*188)
	for _, packet := range [][]byte{
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000),
		pcrPacket(0x0110, 0, 1080000),
		pcrPacket(0x0110, 2, 1170000),
	} {
		data = append(data, packet...)
	}
	counter := tsstat.NewCounter(&bytes.Buffer{})
	if n, err := counter.Write(data); err != nil || n != len(data) {
		t.Fatalf("counter.Write() = %d, %v; want %d, nil", n, err, len(data))
	}

	mediaDir := t.TempDir()
	fullPath := filepath.Join(mediaDir, "test", "drop-batch.m2ts")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating media directory: %v", err)
	}
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("creating ingest temporary file: %v", err)
	}

	if err := (&IngestWorker{Pool: pool}).commit(
		context.Background(), recordingID, "test/drop-batch.m2ts", tempPath, fullPath, int64(len(data)), counter,
	); err != nil {
		t.Fatalf("commit() error: %v", err)
	}

	if got, want := tracer.batchSizes, []int{2, 2}; !equalInts(got, want) {
		t.Errorf("SendBatch sizes = %v, want %v", got, want)
	}
}

func TestIngestWorker_CommitSkipsEmptyDropBatches(t *testing.T) {
	pool, tracer := setupIngestBatchTracePool(t)
	recordingID := insertTestRecording(t, pool)

	mediaDir := t.TempDir()
	fullPath := filepath.Join(mediaDir, "test", "stats-only-drop-batch.m2ts")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating media directory: %v", err)
	}
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	data := append(
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 1, 990000)...,
	)
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("creating ingest temporary file: %v", err)
	}
	counter := tsstat.NewCounter(&bytes.Buffer{})
	if n, err := counter.Write(data); err != nil || n != len(data) {
		t.Fatalf("counter.Write() = %d, %v; want %d, nil", n, err, len(data))
	}

	if err := (&IngestWorker{Pool: pool}).commit(
		context.Background(), recordingID, "test/stats-only-drop-batch.m2ts", tempPath, fullPath,
		int64(len(data)), counter,
	); err != nil {
		t.Fatalf("commit() error: %v", err)
	}

	// stats は 1 件だが positions は 0 件なので、stats batch だけが送信される。
	if got, want := tracer.batchSizes, []int{1}; !equalInts(got, want) {
		t.Fatalf("stats-only SendBatch sizes = %v, want %v", got, want)
	}

	emptyRecordingID := insertTestRecording(t, pool)
	emptyFullPath := filepath.Join(mediaDir, "test", "empty-drop-batch.m2ts")
	emptyTempPath := filepath.Join(filepath.Dir(emptyFullPath), ".rokuban-ingest-test")
	if err := os.WriteFile(emptyTempPath, nil, 0o600); err != nil {
		t.Fatalf("creating empty ingest temporary file: %v", err)
	}
	if err := (&IngestWorker{Pool: pool}).commit(
		context.Background(), emptyRecordingID, "test/empty-drop-batch.m2ts", emptyTempPath, emptyFullPath, 0,
		tsstat.NewCounter(&bytes.Buffer{}),
	); err != nil {
		t.Fatalf("empty commit() error: %v", err)
	}

	if got, want := tracer.batchSizes, []int{1}; !equalInts(got, want) {
		t.Errorf("empty SendBatch sizes = %v, want unchanged %v", got, want)
	}
}

func TestIngestWorker_CommitDropBatchFailureRollsBack(t *testing.T) {
	pool := setupTestPool(t)
	const functionName = "ingest_drop_batch_failure"
	const triggerName = "ingest_drop_batch_failure_trigger"

	_, err := pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+triggerName+" ON drop_positions")
	if err != nil {
		t.Fatalf("removing stale test trigger: %v", err)
	}
	_, err = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	if err != nil {
		t.Fatalf("removing stale test function: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+triggerName+" ON drop_positions")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	})
	_, err = pool.Exec(context.Background(), `
		CREATE FUNCTION ingest_drop_batch_failure() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.byte_offset = 376 THEN
				RAISE EXCEPTION 'injected drop position batch failure';
			END IF;
			RETURN NEW;
		END;
		$$`)
	if err != nil {
		t.Fatalf("creating test trigger function: %v", err)
	}
	_, err = pool.Exec(context.Background(), `
		CREATE TRIGGER ingest_drop_batch_failure_trigger
		BEFORE INSERT ON drop_positions
		FOR EACH ROW EXECUTE FUNCTION ingest_drop_batch_failure()`)
	if err != nil {
		t.Fatalf("creating test trigger: %v", err)
	}

	recordingID := insertTestRecording(t, pool)
	data := make([]byte, 0, 3*188)
	for _, packet := range [][]byte{
		pcrPacket(0x0100, 0, 900000),
		pcrPacket(0x0100, 2, 990000),
		pcrPacket(0x0100, 4, 1080000),
	} {
		data = append(data, packet...)
	}
	counter := tsstat.NewCounter(&bytes.Buffer{})
	if n, err := counter.Write(data); err != nil || n != len(data) {
		t.Fatalf("counter.Write() = %d, %v; want %d, nil", n, err, len(data))
	}

	mediaDir := t.TempDir()
	fullPath := filepath.Join(mediaDir, "test", "drop-batch-failure.m2ts")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating media directory: %v", err)
	}
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-test")
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatalf("creating ingest temporary file: %v", err)
	}

	err = (&IngestWorker{Pool: pool}).commit(
		context.Background(), recordingID, "test/drop-batch-failure.m2ts", tempPath, fullPath,
		int64(len(data)), counter,
	)
	if err == nil {
		t.Fatal("commit() succeeded despite a failed drop position batch item")
	}
	if !strings.Contains(err.Error(), "injected drop position batch failure") {
		t.Errorf("commit() error = %v, want injected batch failure", err)
	}

	var assetCount int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID,
	).Scan(&assetCount); err != nil {
		t.Fatalf("counting rolled back media assets: %v", err)
	}
	if assetCount != 0 {
		t.Errorf("media_assets rows = %d, want 0 after batch failure", assetCount)
	}
	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		t.Errorf("canonical file stat error = %v, want file to remain unpublished", err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
