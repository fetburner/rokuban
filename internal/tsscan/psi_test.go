package tsscan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Comcast/gots/v3"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/tsstat"
)

func psiSection(body []byte) []byte {
	sectionLength := len(body) - 3 + 4
	body[1] = (body[1] & 0xF0) | byte(sectionLength>>8)
	body[2] = byte(sectionLength)
	return append(body, gots.ComputeCRC(body)...)
}

func psiPacket(pid int, section []byte) []byte {
	if len(section) > 183 {
		panic("section does not fit in a single packet")
	}
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = 0x40 | byte((pid>>8)&0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10
	pkt[4] = 0x00
	copy(pkt[5:], section)
	for i := 5 + len(section); i < 188; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

func esPacket(pid, cc int) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte((pid >> 8) & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | byte(cc&0x0F)
	return pkt
}

func makeTSDataWithPSI() []byte {
	pat := psiSection([]byte{
		0x00, 0xB0, 0x00,
		0x00, 0x01,
		0xC1, 0x00, 0x00,
		0x00, 0x01,
		0xF0, 0x00,
	})
	pmt := psiSection([]byte{
		0x02, 0xB0, 0x00,
		0x00, 0x01,
		0xC1, 0x00, 0x00,
		0xE1, 0x00,
		0xF0, 0x00,
		0x02, 0xE1, 0x00, 0xF0, 0x00,
		0x0F, 0xE1, 0x10, 0xF0, 0x00,
	})

	data := append(psiPacket(0x0000, pat), psiPacket(0x1000, pmt)...)
	for cc := 0; cc < 8; cc++ {
		data = append(data, esPacket(0x0100, cc)...)
		data = append(data, esPacket(0x0110, cc)...)
		data = append(data, esPacket(0x0200, cc)...)
	}
	return data
}

func psiString(value string) *string { return &value }

func TestScanWorker_PersistsPIDTypesFromPSI(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := insertTestRecordingWithEventID(t, pool, 12910)

	data := makeTSDataWithPSI()
	mediaDir := t.TempDir()
	const relPath = "psi/recording.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating media directory: %v", err)
	}
	if err := os.WriteFile(fullPath, data, 0o600); err != nil {
		t.Fatalf("writing original: %v", err)
	}
	assetID, err := sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID, Kind: db.AssetKindOriginal, RelPath: relPath, SizeBytes: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("creating original media asset: %v", err)
	}

	runTSScan(t, pool, mediaDir, recordingID)

	rows, err := pool.Query(ctx, `
		SELECT pid, pid_type FROM drop_stats
		WHERE media_asset_id = $1 ORDER BY pid`, assetID)
	if err != nil {
		t.Fatalf("querying drop_stats: %v", err)
	}
	defer rows.Close()
	got := make(map[int32]*string)
	for rows.Next() {
		var pid int32
		var pidType *string
		if err := rows.Scan(&pid, &pidType); err != nil {
			t.Fatalf("scanning drop_stats row: %v", err)
		}
		got[pid] = pidType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating drop_stats: %v", err)
	}
	want := map[int32]*string{
		0x0000: psiString(tsstat.PIDTypePAT),
		0x1000: psiString(tsstat.PIDTypePMT),
		0x0100: psiString(tsstat.PIDTypeVideo),
		0x0110: psiString(tsstat.PIDTypeAudio),
		0x0200: nil,
	}
	if len(got) != len(want) {
		t.Fatalf("drop_stats rows = %d, want %d (%v)", len(got), len(want), got)
	}
	for pid, wantType := range want {
		gotType, ok := got[pid]
		if !ok {
			t.Errorf("PID 0x%04x has no drop_stats row", pid)
			continue
		}
		switch {
		case wantType == nil && gotType != nil:
			t.Errorf("PID 0x%04x pid_type = %q, want NULL", pid, *gotType)
		case wantType != nil && gotType == nil:
			t.Errorf("PID 0x%04x pid_type = NULL, want %q", pid, *wantType)
		case wantType != nil && *gotType != *wantType:
			t.Errorf("PID 0x%04x pid_type = %q, want %q", pid, *gotType, *wantType)
		}
	}
}
