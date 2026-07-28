package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Comcast/gots/v3"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/tsstat"
)

// PAT / PMT を含む合成 TS を作るためのヘルパー。
// internal/tsstat 側のテストヘルパーはパッケージ外から使えないので最小限を持つ。

func psiSection(body []byte) []byte {
	sectionLength := len(body) - 3 + 4 // + CRC_32
	body[1] = (body[1] & 0xF0) | byte(sectionLength>>8)
	body[2] = byte(sectionLength)
	return append(body, gots.ComputeCRC(body)...)
}

// psiPacket はセクション 1 つを 1 パケットに詰める（188 バイトに収まる長さ限定）。
func psiPacket(pid int, section []byte) []byte {
	if len(section) > 183 {
		panic("section does not fit in a single packet")
	}
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = 0x40 | byte((pid>>8)&0x1F) // payload_unit_start_indicator
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 // payload only, CC 0
	pkt[4] = 0x00 // pointer_field
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

// makeTSDataWithPSI は PAT / PMT と ES を含む TS を返す。
// PMT は映像 0x0100 と音声 0x0110 を宣言し、0x0200 はどこにも宣言しない。
func makeTSDataWithPSI() []byte {
	pat := psiSection([]byte{
		0x00, 0xB0, 0x00,
		0x00, 0x01, // transport_stream_id
		0xC1, 0x00, 0x00,
		0x00, 0x01, // program_number 1
		0xF0, 0x00, // PMT PID 0x1000
	})
	pmt := psiSection([]byte{
		0x02, 0xB0, 0x00,
		0x00, 0x01, // program_number
		0xC1, 0x00, 0x00,
		0xE1, 0x00, // PCR_PID 0x0100
		0xF0, 0x00, // program_info_length 0
		0x02, 0xE1, 0x00, 0xF0, 0x00, // MPEG-2 Video, PID 0x0100
		0x0F, 0xE1, 0x10, 0xF0, 0x00, // AAC, PID 0x0110
	})

	data := append(psiPacket(0x0000, pat), psiPacket(0x1000, pmt)...)
	for cc := 0; cc < 8; cc++ {
		data = append(data, esPacket(0x0100, cc)...)
		data = append(data, esPacket(0x0110, cc)...)
		data = append(data, esPacket(0x0200, cc)...)
	}
	return data
}

// ingest が採取した PID 種別が drop_stats.pid_type に入ることを確認する。
// 分類できない PID は NULL のままで、行そのものは作られる。
func TestIngestWorker_DropStatPIDType(t *testing.T) {
	tsData := makeTSDataWithPSI()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tsData)

		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{
					Options: mirakc.Options{ContentPath: strPtr("psi/recording.m2ts")},
				},
				Content: mirakc.ContentInfo{Path: "/recording/psi/recording.m2ts"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)

		case r.Method == http.MethodDelete:
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	w := &IngestWorker{
		MirakcClient: mirakc.NewClient(srv.URL, nil),
		MediaDir:     t.TempDir(),
		StallTimeout: 5 * time.Second,
		Pool:         pool,
	}

	recordingID := insertTestRecording(t, pool)
	insertTestRecordSync(t, pool, recordingID, "rec-psi")

	job := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-psi"},
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT pid, pid_type FROM drop_stats d
		   JOIN media_assets a ON a.id = d.media_asset_id
		  WHERE a.recording_id = $1`, recordingID)
	if err != nil {
		t.Fatalf("querying drop_stats: %v", err)
	}
	defer rows.Close()

	got := map[int]*string{}
	for rows.Next() {
		var pid int
		var pidType *string
		if err := rows.Scan(&pid, &pidType); err != nil {
			t.Fatalf("scanning drop_stats: %v", err)
		}
		got[pid] = pidType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating drop_stats: %v", err)
	}

	want := map[int]*string{
		0x0000: strPtr(tsstat.PIDTypePAT),
		0x1000: strPtr(tsstat.PIDTypePMT),
		0x0100: strPtr(tsstat.PIDTypeVideo),
		0x0110: strPtr(tsstat.PIDTypeAudio),
		0x0200: nil, // PMT に無い PID は種別なし。行は作る
	}
	if len(got) != len(want) {
		t.Errorf("drop_stats rows = %d, want %d (%v)", len(got), len(want), got)
	}
	for pid, wantType := range want {
		gotType, ok := got[pid]
		if !ok {
			t.Errorf("PID 0x%04x の行が無い", pid)
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
