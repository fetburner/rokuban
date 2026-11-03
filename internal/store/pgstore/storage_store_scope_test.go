package pgstore

import (
	"testing"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestStorageSyncAllowsSameRootAcrossStores(t *testing.T) {
	pool := testutil.SetupDB(t)
	for _, name := range []string{"tokyo", "takamatsu"} {
		if err := NewMedia(pool, name, "").UpsertStorageSync(t.Context(), store.StorageObservationInput{
			Root: "media", Path: "/mnt/" + name, TotalBytes: 100, UsedBytes: 10, AvailableBytes: 90,
		}); err != nil {
			t.Fatalf("UpsertStorageSync(%s, media): %v", name, err)
		}
	}
}

func TestOrphanFileAllowsSamePathAcrossStores(t *testing.T) {
	pool := testutil.SetupDB(t)
	for _, name := range []string{"tokyo", "takamatsu"} {
		if err := NewMedia(pool, name, "").UpsertOrphanFile(t.Context(), "2026/07/orphan.m2ts"); err != nil {
			t.Fatalf("UpsertOrphanFile(%s): %v", name, err)
		}
	}
}
