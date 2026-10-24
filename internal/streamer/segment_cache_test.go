package streamer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSegmentCacheLRUAndProtectedLeases(t *testing.T) {
	cache, err := newSegmentCache(t.TempDir(), 6)
	if err != nil {
		t.Fatal(err)
	}
	a := SegmentKey{"recording", "generation", "profile", "video", 1}
	b := SegmentKey{"recording", "generation", "profile", "video", 2}
	c := SegmentKey{"recording", "generation", "profile", "video", 3}

	leaseA, _, err := cache.publish(writeSegmentFile(t, cache.root, "aaa"), a)
	if err != nil {
		t.Fatal(err)
	}
	leaseA.release()
	leaseB, _, err := cache.publish(writeSegmentFile(t, cache.root, "bbb"), b)
	if err != nil {
		t.Fatal(err)
	}
	leaseB.release()

	// Touch a so b is the least-recently-used unprotected entry.
	leaseA, ok, err := cache.acquire(a)
	if err != nil || !ok {
		t.Fatalf("acquire a = (%v, %v), want a lease", leaseA, err)
	}
	leaseA.release()

	leaseC, _, err := cache.publish(writeSegmentFile(t, cache.root, "ccc"), c)
	if err != nil {
		t.Fatal(err)
	}
	leaseC.release()
	if cache.usedBytes() != 6 {
		t.Fatalf("used bytes = %d, want 6", cache.usedBytes())
	}
	if _, ok, err := cache.acquire(b); err != nil || ok {
		t.Fatalf("least-recently-used b still cached: ok=%v err=%v", ok, err)
	}
	for _, key := range []SegmentKey{a, c} {
		lease, ok, err := cache.acquire(key)
		if err != nil || !ok {
			t.Fatalf("recent segment %#v missing: ok=%v err=%v", key, ok, err)
		}
		lease.release()
	}

	// A live lease is not evictable, so insertion waits instead of unlinking it.
	protectedCache, err := newSegmentCache(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	protected, _, err := protectedCache.publish(writeSegmentFile(t, protectedCache.root, "one"), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := protectedCache.publish(writeSegmentFile(t, protectedCache.root, "two"), b); !errors.Is(err, errSegmentCacheFull) {
		t.Fatalf("publish at protected capacity = %v, want errSegmentCacheFull", err)
	}
	protected.release()
	next, _, err := protectedCache.publish(writeSegmentFile(t, protectedCache.root, "two"), b)
	if err != nil {
		t.Fatalf("publish after lease release: %v", err)
	}
	next.release()
	if _, ok, err := protectedCache.acquire(a); err != nil || ok {
		t.Fatalf("released a should be evicted: ok=%v err=%v", ok, err)
	}
}

func TestSegmentCacheHardLinkFirstWriterWins(t *testing.T) {
	cache, err := newSegmentCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	key := SegmentKey{"recording", "generation", "profile", "audio-main", 7}
	first := writeSegmentFile(t, cache.root, "first")
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	lease, inserted, err := cache.publish(first, key)
	if err != nil || !inserted {
		t.Fatalf("first publish = (inserted %v, err %v), want inserted", inserted, err)
	}
	lease.release()
	cachedInfo, err := os.Stat(cache.pathFor(key))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, cachedInfo) {
		t.Fatal("published cache path does not share the scratch file inode; want an os.Link publication")
	}

	second := writeSegmentFile(t, cache.root, "other")
	lease, inserted, err = cache.publish(second, key)
	if err != nil || inserted {
		t.Fatalf("second publish = (inserted %v, err %v), want existing winner", inserted, err)
	}
	got, err := io.ReadAll(lease.file)
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	if string(got) != "first" {
		t.Fatalf("winning bytes = %q, want first", got)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("losing scratch file stat error = %v, want not exist", err)
	}
}

func TestSegmentCachePathCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	cache, err := newSegmentCache(root, 64)
	if err != nil {
		t.Fatal(err)
	}
	key := SegmentKey{"../../recording", "..", "profile/../../", "audio", 0}
	path := cache.pathFor(key)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		t.Fatalf("cache path escapes root: %s", path)
	}
}

func writeSegmentFile(t *testing.T, root, contents string) string {
	t.Helper()
	path := filepath.Join(root, "tmp-"+t.Name()+"-"+contents)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSegmentCacheCanUnlinkAnOpenDeliveryFile(t *testing.T) {
	cache, err := newSegmentCache(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	a := SegmentKey{"recording", "generation", "profile", "video", 10}
	b := SegmentKey{"recording", "generation", "profile", "video", 11}
	published, _, err := cache.publish(writeSegmentFile(t, cache.root, "four"), a)
	if err != nil {
		t.Fatal(err)
	}
	published.release()

	delivery, ok, err := cache.acquire(a)
	if err != nil || !ok {
		t.Fatalf("acquire delivery = (ok %v, err %v)", ok, err)
	}
	replacement, _, err := cache.publish(writeSegmentFile(t, cache.root, "next"), b)
	if err != nil {
		t.Fatalf("new segment should evict the open delivery path: %v", err)
	}
	replacement.release()

	got, err := io.ReadAll(delivery.file)
	if err != nil {
		t.Fatal(err)
	}
	delivery.release()
	if string(got) != "four" {
		t.Fatalf("open delivery bytes = %q, want four", got)
	}
	if _, ok, err := cache.acquire(a); err != nil || ok {
		t.Fatalf("evicted path still indexed: ok=%v err=%v", ok, err)
	}
}
