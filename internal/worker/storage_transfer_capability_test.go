package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// storeWithoutResumable は Store の基本操作だけを委譲し、Resumable を隠す。
type storeWithoutResumable struct {
	objstore.Store
	putCalls int
}

func (s *storeWithoutResumable) Put(ctx context.Context, key string, src io.Reader, size int64, sha256 [32]byte, contentType string) error {
	s.putCalls++
	return s.Store.Put(ctx, key, src, size, sha256, contentType)
}

// TestStorageTransferWorkerRejectsDerivedDestinationWithoutResumableSupport は、再開できない宛先を転送失敗として数えず、
// Put より前にジョブエラーで拒否することを確かめる。
func TestStorageTransferWorkerRejectsDerivedDestinationWithoutResumableSupport(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	recordingID := dbtest.Recording(t, pool)
	relPath := "sites/default/transfer/non-resumable.jpg"
	content := []byte("derived bytes must not be written to a non-resumable target")
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindThumbnail, nil, relPath, content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	objects := transferTestObjectStores(t, roots)
	target := &storeWithoutResumable{Store: objects["archive"]}
	objects["archive"] = target
	worker := StorageTransferWorker{
		Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: roots,
		ObjectStores: objects, ScratchDir: t.TempDir(),
	}

	err = worker.Work(context.Background(), jobs.StorageTransferArgs{
		MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint,
	})
	if err == nil || !strings.Contains(err.Error(), `target store "archive" is not a resumable object store`) {
		t.Fatalf("Work error = %v, want a non-resumable target job error", err)
	}
	if target.putCalls != 0 {
		t.Fatalf("target Put calls = %d, want 0", target.putCalls)
	}
	destination := filepath.Join(roots["archive"], filepath.FromSlash(transferTestCopyRelPath(t, pool, assetID, "archive")))
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination stat error = %v, want the object to be absent", statErr)
	}
	copyID := transferTestCopyID(t, pool, assetID, "archive")
	var attempts int
	if err := pool.QueryRow(context.Background(), `
SELECT COALESCE((SELECT attempt_count FROM media_asset_transfer_attempts WHERE copy_id = $1), 0)`, copyID).Scan(&attempts); err != nil {
		t.Fatalf("reading transfer attempt count: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("recorded transfer attempts = %d, want 0 for a worker capability error", attempts)
	}
}
