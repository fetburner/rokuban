package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRescue_SkipsLegacyNeverScheduledPseudoRow は issue #318 より前に export
// された catalog に残る欠測擬似行（recordings.status='failed' + quality_events の
// recording.never-scheduled マーカー）を rescue が recordings に戻さないことを
// 確認する。欠測は never_scheduled_events 表へ移設され、recordings は観測
// された試行だけを持つ脊椎になった。擬似行を復元すると「試行でない行」が
// ライブラリに failed として復活してしまう（issue #318 item 5）。
//
// 同じダンプに含まれる本物の録画は従来どおり復元されることも合わせて確認する
// （スキップが marker 付きの行だけに効いていること）。
func TestRescue_SkipsLegacyNeverScheduledPseudoRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	programStartAt := time.Date(2026, 8, 1, 21, 0, 0, 0, time.UTC)
	programEndedAt := programStartAt.Add(30 * time.Minute)
	marker, err := json.Marshal([]map[string]any{{
		"at":     programEndedAt,
		"event":  "recording.never-scheduled",
		"reason": json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("marshalling quality_events: %v", err)
	}

	doc := &Document{
		Version:    Version,
		ExportedAt: programStartAt,
		Recordings: []Recording{
			{
				// 旧世代の欠測擬似行（marker 付き）。rescue でスキップされるはず。
				ID:                1,
				Source:            "rule",
				Site:              "default",
				NetworkID:         32736,
				ServiceID:         1024,
				EventID:           300,
				ServiceName:       "NHK総合",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "欠測になった番組",
				IsFree:            true,
				ProgramStartAt:    programStartAt,
				ProgramDurationMs: 1800000,
				Status:            "failed",
				QualityEvents:     marker,
				CreatedAt:         programStartAt,
				UpdatedAt:         programStartAt,
			},
			{
				// 本物の録画（marker 無し）。復元されるはず。
				ID:                2,
				Source:            "rule",
				Site:              "default",
				NetworkID:         32736,
				ServiceID:         1024,
				EventID:           301,
				ServiceName:       "NHK総合",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "録れた番組",
				IsFree:            true,
				ProgramStartAt:    programStartAt,
				ProgramDurationMs: 1800000,
				Status:            "finished",
				QualityEvents:     json.RawMessage(`[]`),
				CreatedAt:         programStartAt,
				UpdatedAt:         programStartAt,
			},
		},
	}

	mediaDir := t.TempDir()
	genDir, err := Write(mediaDir, doc, 7)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(genDir, DocumentFilename)

	result, err := RescueFile(ctx, pool, path)
	if err != nil {
		t.Fatalf("RescueFile: %v", err)
	}
	if result.Recordings != 1 {
		t.Errorf("rescued recordings = %d, want 1 (欠測擬似行はスキップ、本物だけ復元)", result.Recordings)
	}

	// 欠測擬似行（event_id=300）は recordings に無い。
	var pseudoExists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM recordings WHERE site='default' AND network_id=32736 AND service_id=1024 AND event_id=300)`,
	).Scan(&pseudoExists); err != nil {
		t.Fatalf("checking pseudo-row: %v", err)
	}
	if pseudoExists {
		t.Error("欠測擬似行が recordings に復元された, want スキップ（recordings は試行だけを持つ）")
	}

	// 本物の録画（event_id=301）は復元されている。
	var realExists bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM recordings WHERE site='default' AND network_id=32736 AND service_id=1024 AND event_id=301)`,
	).Scan(&realExists); err != nil {
		t.Fatalf("checking real recording: %v", err)
	}
	if !realExists {
		t.Error("本物の録画が復元されていない（スキップが marker 付きの行だけに効いていない）")
	}
}
