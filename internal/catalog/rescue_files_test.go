package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueCatalogMissingFiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := newTestMediaDir(t)
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
	if _, err := Write(mediaDir, doc, 7, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := RescueLatest(context.Background(), pgstoretest.Catalog(pool, []string{"default"}...), pgstoretest.Media(pool), mediaDir, "test-store", nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.MissingMediaFiles != 2 || result.MediaAssets != 4 {
			t.Fatalf("result = %+v", result)
		}
		var active, deleted int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE state <> 'deleted'), count(*) FILTER (WHERE state='deleted' AND deleted_at IS NOT NULL) FROM media_asset_copies").Scan(&active, &deleted); err != nil {
			t.Fatal(err)
		}
		if active != 1 || deleted != 3 {
			t.Fatalf("live/deleted = %d/%d", active, deleted)
		}
	}
}

func TestRescueCatalogMissingOldGenerationKeepsCurrentCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := newTestMediaDir(t)
	oldPath := "old.g1.mp4"
	currentPath := "current.g2.mp4"
	if err := os.WriteFile(filepath.Join(mediaDir, oldPath), []byte("old media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, currentPath), []byte("current media"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := "h264"
	currentGeneration, oldGeneration := int64(2), int64(1)
	doc := &Document{
		Version: Version, ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test",
			ProgramStartAt: fixedTime(), Status: "finished", QualityEvents: json.RawMessage("[]"),
			CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
		MediaAssets: []MediaAsset{
			{
				ID: 10, RecordingID: 1, Kind: "encoded", Profile: &profile,
				RelPath: oldPath, SizeBytes: 80, State: "active",
				CreatedAt: fixedTime().Add(-time.Hour), UpdatedAt: fixedTime(),
				DerivedGeneration: &currentGeneration, Generation: &oldGeneration, Store: "test-store",
			},
			{
				ID: 10, RecordingID: 1, Kind: "encoded", Profile: &profile,
				RelPath: currentPath, SizeBytes: 100, State: "active",
				CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
				DerivedGeneration: &currentGeneration, Generation: &currentGeneration, Store: "test-store",
			},
		},
	}
	if _, err := Write(mediaDir, doc, 7, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mediaDir, oldPath)); err != nil {
		t.Fatal(err)
	}

	result, err := RescueLatest(context.Background(), pgstoretest.Catalog(pool, []string{"default"}...), pgstoretest.Media(pool), mediaDir, "test-store", nil)
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.MissingMediaFiles != 1 {
		t.Fatalf("MissingMediaFiles = %d, want 1 missing physical copy", result.MissingMediaFiles)
	}
	var oldState, currentState string
	var hasActiveCopy bool
	if err := pool.QueryRow(context.Background(), `
SELECT old_copy.state, current_copy.state, EXISTS (SELECT 1 FROM active_media_asset_copies(asset.id, NULL))
FROM media_assets asset
	JOIN media_asset_copies old_copy ON old_copy.media_asset_id = asset.id AND old_copy.rel_path = $2
	JOIN media_asset_copies current_copy ON current_copy.media_asset_id = asset.id AND current_copy.rel_path = $3
	WHERE asset.id = $1`, 10, oldPath, currentPath).Scan(&oldState, &currentState, &hasActiveCopy); err != nil {
		t.Fatalf("reading restored generation copies: %v", err)
	}
	if oldState != "deleted" || currentState != "active" || !hasActiveCopy {
		t.Fatalf("old/current copy states and current active copy = %q/%q/%v, want deleted/active/true", oldState, currentState, hasActiveCopy)
	}
}

func TestRescueCatalogMarksPreviouslyRestoredCopyDeletedWhenFileDisappears(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := newTestMediaDir(t)
	const relPath = "current.g2.mp4"
	if err := os.WriteFile(filepath.Join(mediaDir, relPath), []byte("current media"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := "h264"
	generation := int64(2)
	doc := &Document{
		Version: Version, ExportedAt: fixedTime(),
		Recordings: []Recording{{
			ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test",
			ProgramStartAt: fixedTime(), Status: "finished", QualityEvents: json.RawMessage("[]"),
			CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
		}},
		MediaAssets: []MediaAsset{{
			ID: 11, RecordingID: 1, Kind: "encoded", Profile: &profile,
			RelPath: relPath, SizeBytes: 100, State: "active",
			CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
			DerivedGeneration: &generation, Generation: &generation, Store: "test-store",
		}},
	}
	if _, err := Write(mediaDir, doc, 7, nil); err != nil {
		t.Fatal(err)
	}
	catalogStore := pgstoretest.Catalog(pool, []string{"default"}...)
	mediaStore := pgstoretest.Media(pool)
	if _, err := RescueLatest(context.Background(), catalogStore, mediaStore, mediaDir, "test-store", nil); err != nil {
		t.Fatalf("initial RescueLatest: %v", err)
	}
	if err := os.Remove(filepath.Join(mediaDir, relPath)); err != nil {
		t.Fatal(err)
	}
	result, err := RescueLatest(context.Background(), catalogStore, mediaStore, mediaDir, "test-store", nil)
	if err != nil {
		t.Fatalf("RescueLatest after file removal: %v", err)
	}
	if result.MissingMediaFiles != 1 {
		t.Fatalf("MissingMediaFiles = %d, want 1", result.MissingMediaFiles)
	}
	var activeCopies, deletedCopies int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FILTER (WHERE copy.state = 'active'),
       count(*) FILTER (WHERE copy.state = 'deleted')
FROM media_asset_copies copy
WHERE copy.media_asset_id = 11`).Scan(&activeCopies, &deletedCopies); err != nil {
		t.Fatalf("reading copy state after second rescue: %v", err)
	}
	if activeCopies != 0 || deletedCopies != 1 {
		t.Fatalf("active/deleted copies = %d/%d, want 0/1", activeCopies, deletedCopies)
	}
}

func TestRescueScanCandidateRemoved(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := newTestMediaDir(t)
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
		if err := os.Remove(p); err != nil {
			return nil, err
		}
		return info, nil
	}
	result, err := RescueLatest(context.Background(), pgstoretest.Catalog(pool, []string{"default"}...), pgstoretest.Media(pool), mediaDir, "test-store", []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	if result.MediaAssets != 0 || result.MissingMediaFiles != 1 {
		t.Fatalf("result = %+v", result)
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_asset_copies WHERE state='active'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active rows for removed file = %d", count)
	}
}
