package streamer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestDerivedServingUsesPreferredReadableCopyAcrossKinds(t *testing.T) {
	const preferredStore = "zz-s3"
	oldContent := []byte("from-old-store")
	newContent := []byte("from-new-store")
	assetCases := []struct {
		name     string
		kind     string
		profile  string
		ext      string
		endpoint func(int64) string
	}{
		{
			name: "encoded", kind: store.AssetKindEncoded, profile: "h264", ext: ".mp4",
			endpoint: func(id int64) string { return fmt.Sprintf("/api/media/recordings/%d/file?profile=h264", id) },
		},
		{
			name: "thumbnail", kind: store.AssetKindThumbnail, ext: ".jpg",
			endpoint: func(id int64) string { return fmt.Sprintf("/api/media/recordings/%d/thumbnail", id) },
		},
		{
			name: "seek tiles", kind: store.AssetKindSeekTiles, ext: ".jpg",
			endpoint: func(id int64) string { return fmt.Sprintf("/api/media/recordings/%d/seek-tiles", id) },
		},
	}
	for _, assetCase := range assetCases {
		t.Run(assetCase.name, func(t *testing.T) {
			for _, transferComplete := range []bool{false, true} {
				name := "before transfer: old FS copy is the only copy"
				if transferComplete {
					name = "overlap: prefer the new S3 copy over the earlier-sorting FS copy"
				}
				t.Run(name, func(t *testing.T) {
					pool := testutil.SetupDB(t)
					oldRoot, newRoot := t.TempDir(), t.TempDir()
					relPath := filepath.ToSlash(filepath.Join("2026", "07", "asset"+assetCase.ext))
					oldPath := filepath.Join(oldRoot, filepath.FromSlash(relPath))
					if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(oldPath, oldContent, 0o644); err != nil {
						t.Fatal(err)
					}
					oldObjects, err := fsstore.New(oldRoot)
					if err != nil {
						t.Fatal(err)
					}
					newObjects, err := fsstore.New(newRoot)
					if err != nil {
						t.Fatal(err)
					}
					recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = store.RecordingStatusFinished })
					assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
						p.Kind, p.RelPath, p.SizeBytes = assetCase.kind, relPath, int64(len(oldContent))
						if assetCase.profile != "" {
							p.Profile = &assetCase.profile
						}
					})
					if transferComplete {
						if err := newObjects.Put(t.Context(), relPath, bytes.NewReader(newContent), int64(len(newContent)), sha256.Sum256(newContent), "application/octet-stream"); err != nil {
							t.Fatal(err)
						}
						if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT media_asset_id, $2, rel_path, generation, 'active' FROM media_asset_copies WHERE media_asset_id = $1`, assetID, preferredStore); err != nil {
							t.Fatal(err)
						}
					}

					router := chi.NewRouter()
					New(pgstore.NewMediaForStreamer(pool, preferredStore, []string{dbtest.StoreName, preferredStore}), Config{
						MediaRoots: map[string]string{dbtest.StoreName: oldRoot},
						ObjectStores: map[string]objstore.Store{
							dbtest.StoreName: oldObjects,
							preferredStore:   newObjects,
						},
					}).Mount(router)
					httpServer := httptest.NewServer(router)
					t.Cleanup(httpServer.Close)

					resp, body := get(t, httpServer.URL+assetCase.endpoint(recordingID), nil)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status = %d, want 200", resp.StatusCode)
					}
					want := oldContent
					if transferComplete {
						want = newContent
					}
					if !bytes.Equal(body, want) {
						t.Fatalf("body = %q, want %q", body, want)
					}
				})
			}
		})
	}
}

func TestDerivedServingReturnsNotFoundWhenNoReadableCopyExists(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = store.RecordingStatusFinished })
	profile := "h264"
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind, p.Profile, p.RelPath, p.SizeBytes = store.AssetKindEncoded, &profile, "2026/07/unreadable.mp4", 1
	})

	router := chi.NewRouter()
	New(pgstore.NewMediaForStreamer(pool, "zz-s3", []string{"zz-s3"}), Config{}).Mount(router)
	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)

	resp, _ := get(t, fmt.Sprintf("%s/api/media/recordings/%d/file?profile=h264", httpServer.URL, recordingID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the only active copy is unreadable", resp.StatusCode)
	}
}
