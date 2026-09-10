package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

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
