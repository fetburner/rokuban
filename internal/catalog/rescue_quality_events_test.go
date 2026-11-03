package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueNullQualityEvents(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := newTestMediaDir(t)

	doc := &Document{
		Version:    Version,
		ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "null events",
			ProgramStartAt: fixedTime(), ProgramDurationMs: time.Hour.Milliseconds(), Status: "failed",
			QualityEvents: json.RawMessage(`null`), CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
	}
	genDir, err := Write(mediaDir, doc, 1, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := RescueFile(ctx, pgstoretest.Catalog(pool, []string{"default"}...), mediaDir, "test-store", filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatalf("RescueFile: %v", err)
	}

	var got string
	if err := pool.QueryRow(ctx, `SELECT quality_events::text FROM recordings WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("querying rescued quality_events: %v", err)
	}
	if got != "[]" {
		t.Errorf("rescued quality_events = %s, want []", got)
	}
}
