package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/medialock"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueCatalogMissingFiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	doc := &Document{Version: Version, ExportedAt: fixedTime(),
		Recordings: []Recording{{ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test", ProgramStartAt: fixedTime(), Status: "finished",
			QualityEvents: json.RawMessage("[]"), CreatedAt: fixedTime(), UpdatedAt: fixedTime()}},
	}
	for i, state := range []string{"active", "deleting", "deleted"} {
		a := MediaAsset{ID: int64(i + 1), RecordingID: 1, Kind: "encoded", RelPath: state + ".mp4", State: state, CreatedAt: fixedTime(), UpdatedAt: fixedTime()}
		a.Profile = &state
		if state == "deleted" {
			at := fixedTime()
			a.DeletedAt = &at
		}
		doc.MediaAssets = append(doc.MediaAssets, a)
	}
	profile := "present"
	doc.MediaAssets = append(doc.MediaAssets, MediaAsset{ID: 4, RecordingID: 1, Kind: "encoded", Profile: &profile, RelPath: "present.mp4", State: "active", CreatedAt: fixedTime(), UpdatedAt: fixedTime()})
	if err := os.WriteFile(filepath.Join(mediaDir, "present.mp4"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(mediaDir, doc, 7); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := RescueLatest(context.Background(), pool, mediaDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.MissingMediaFiles != 2 || result.MediaAssets != 4 {
			t.Fatalf("result = %+v", result)
		}
		var active, deleted int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE state <> 'deleted'), count(*) FILTER (WHERE state='deleted' AND deleted_at IS NOT NULL) FROM media_assets").Scan(&active, &deleted); err != nil {
			t.Fatal(err)
		}
		if active != 1 || deleted != 3 {
			t.Fatalf("live/deleted = %d/%d", active, deleted)
		}
	}
}

func TestRescueScanCandidateRemoved(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	const rel = "sites/default/show.m2ts"
	path := filepath.Join(mediaDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := statRescueCandidate
	defer func() { statRescueCandidate = original }()
	statRescueCandidate = func(p string) (os.FileInfo, error) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		removed := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lock, err := medialock.Lock(ctx, mediaDir, rel)
			if err != nil {
				removed <- err
				return
			}
			defer func() { _ = lock.Close() }()
			removed <- os.Remove(p)
		}()
		if err := <-removed; err != nil {
			return nil, err
		}
		return info, nil
	}
	result, err := RescueLatest(context.Background(), pool, mediaDir, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MediaAssets != 0 || result.MissingMediaFiles != 1 {
		t.Fatalf("result = %+v", result)
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE state='active'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active rows for removed file = %d", count)
	}
}
