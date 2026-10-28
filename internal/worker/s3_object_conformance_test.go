//go:build s3conformance

package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestS3DerivedObjectPublicationAndRecovery(t *testing.T) {
	ctx := t.Context()
	endpoint := os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")
	bucket := os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT and ROKUBAN_S3_CONFORMANCE_BUCKET are required")
	}
	region := os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	objects, err := s3store.New(ctx, s3store.Config{Endpoint: endpoint, Region: region, Bucket: bucket})
	if err != nil {
		t.Fatalf("creating S3 object store: %v", err)
	}
	pool := testutil.SetupDB(t)
	media := pgstore.NewMediaWithStores(pool, "s3main", dbtest.StoreName, "s3main")
	prefix := fmt.Sprintf("thumbnails/issue-1625-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range []string{prefix + "-active.jpg", prefix + "-pending.jpg", prefix + "-unknown.jpg"} {
			if err := objects.Delete(context.Background(), key); err != nil {
				t.Errorf("deleting S3 test key %q: %v", key, err)
			}
		}
	})

	t.Run("pending commits before PUT and active follows PUT", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		originalID := dbtest.MediaAsset(t, pool, recordingID)
		key := prefix + "-active.jpg"
		payload := []byte("published thumbnail bytes")
		scratchPath := filepath.Join(t.TempDir(), "thumbnail.jpg")
		if err := os.WriteFile(scratchPath, payload, 0o600); err != nil {
			t.Fatalf("writing thumbnail fixture: %v", err)
		}
		observing := &pendingObservedStore{Store: objects, t: t, pool: pool, storeName: "s3main"}
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key}
		size, published, err := publishGeneratedObject(ctx, observing, media, pending, scratchPath, "image/jpeg",
			func(size int64, publish func() error) (bool, error) {
				return media.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: key, SizeBytes: size, InputAssetID: originalID,
				}, publish)
			})
		if err != nil {
			t.Fatalf("publishing S3 thumbnail: %v", err)
		}
		if !observing.sawPending {
			t.Fatal("PUT did not observe a committed pending copy row")
		}
		if !published || size != int64(len(payload)) {
			t.Fatalf("publish result = (%d, %t), want (%d, true)", size, published, len(payload))
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, "s3main", key).Scan(&state); err != nil {
			t.Fatalf("reading published copy state: %v", err)
		}
		if state != "active" {
			t.Fatalf("published copy state = %q, want active", state)
		}
		object, err := objects.Head(ctx, key)
		if err != nil {
			t.Fatalf("heading published object: %v", err)
		}
		if object.Size != int64(len(payload)) || object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(payload)) {
			t.Fatalf("published object metadata = (size %d, sha256 %q), want (%d, %x)", object.Size, object.SHA256, len(payload), sha256.Sum256(payload))
		}
	})

	t.Run("uploaded pending copy is reclaimed after a worker crash", func(t *testing.T) {
		recordingID := dbtest.Recording(t, pool)
		key := prefix + "-pending.jpg"
		payload := []byte("uploaded before simulated process death")
		digest := sha256.Sum256(payload)
		pending := store.PendingMediaAssetCopy{RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key, SizeBytes: int64(len(payload))}
		if err := media.PreparePendingMediaAssetCopy(ctx, pending); err != nil {
			t.Fatalf("committing pending copy: %v", err)
		}
		if err := objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"); err != nil {
			t.Fatalf("uploading object before simulated process death: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET created_at = now() - interval '30 days' WHERE store = $1 AND rel_path = $2`, "s3main", key); err != nil {
			t.Fatalf("aging pending copy: %v", err)
		}
		cleanup := &DeleteReconcileWorker{DerivedObjects: objects, DerivedCopyMaintenance: media}
		if err := cleanup.reclaimPendingObjectCopies(ctx, time.Now().Add(-14*24*time.Hour)); err != nil {
			t.Fatalf("reclaiming stale pending copy: %v", err)
		}
		if _, err := objects.Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
			t.Fatalf("reclaimed S3 key HEAD error = %v, want ErrNotFound", err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, "s3main", key).Scan(&state); err != nil {
			t.Fatalf("reading reclaimed copy state: %v", err)
		}
		if state != "deleted" {
			t.Fatalf("reclaimed copy state = %q, want deleted", state)
		}
	})

	t.Run("DB-less S3 keys are reported and retained", func(t *testing.T) {
		key := prefix + "-unknown.jpg"
		payload := []byte("unknown keys are not automatically deleted")
		digest := sha256.Sum256(payload)
		if err := objects.Put(ctx, key, strings.NewReader(string(payload)), int64(len(payload)), digest, "image/jpeg"); err != nil {
			t.Fatalf("uploading DB-less object: %v", err)
		}
		cleanup := &DeleteReconcileWorker{DerivedObjects: objects, DerivedStorage: media, DerivedObjectStoreName: "s3main"}
		if err := cleanup.reconcileObjectStore(ctx, -time.Second, 0); err != nil {
			t.Fatalf("reconciling unknown S3 key: %v", err)
		}
		if _, err := objects.Head(ctx, key); err != nil {
			t.Fatalf("DB-less S3 key was removed instead of retained: %v", err)
		}
		aged, err := media.ListAgedOrphanFiles(ctx, 0)
		if err != nil {
			t.Fatalf("listing reported DB-less key: %v", err)
		}
		if !containsString(aged, key) {
			t.Fatalf("DB-less key %q is absent from aged orphan report %v", key, aged)
		}
	})
}

type pendingObservedStore struct {
	objstore.Store
	t          *testing.T
	pool       *pgxpool.Pool
	storeName  string
	sawPending bool
}

func (s *pendingObservedStore) Put(ctx context.Context, key string, src io.Reader, size int64, checksum [32]byte, contentType string) error {
	s.t.Helper()
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = $1 AND rel_path = $2`, s.storeName, key).Scan(&state); err != nil {
		return fmt.Errorf("checking copy state immediately before PUT: %w", err)
	}
	if state != "pending" {
		return fmt.Errorf("copy state immediately before PUT = %q, want pending", state)
	}
	s.sawPending = true
	return s.Store.Put(ctx, key, src, size, checksum, contentType)
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
