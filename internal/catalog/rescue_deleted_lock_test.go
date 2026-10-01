package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/medialock"
	"github.com/fetburner/rokuban/internal/testutil"
)

// deleted の行は active 行を作らないのでロックしない。他者が path を握っていても待たない。
func TestRescueFile_DoesNotLockDeletedAssets(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	profile := "p"
	at := fixedTime()
	doc := &Document{Version: Version, ExportedAt: fixedTime(),
		Recordings: []Recording{{ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test", ProgramStartAt: fixedTime(), Status: "finished",
			QualityEvents: json.RawMessage("[]"), CreatedAt: fixedTime(), UpdatedAt: fixedTime()}},
		MediaAssets: []MediaAsset{{ID: 1, RecordingID: 1, Kind: "encoded", Profile: &profile, RelPath: "gone.mp4",
			State: "deleted", DeletedAt: &at, CreatedAt: fixedTime(), UpdatedAt: fixedTime()}},
	}
	genDir, err := Write(mediaDir, doc, 7)
	if err != nil {
		t.Fatal(err)
	}
	held, err := medialock.Lock(context.Background(), mediaDir, "gone.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := RescueFile(ctx, pool, mediaDir, filepath.Join(genDir, DocumentFilename)); err != nil {
		t.Fatalf("RescueFile blocked on a deleted asset's lock: %v", err)
	}
}
