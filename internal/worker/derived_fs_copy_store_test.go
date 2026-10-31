package worker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

// callRecordingStore は Head / Delete の key を記録する objstore.Store。
type callRecordingStore struct {
	objstore.Store
	mu      sync.Mutex
	heads   []string
	deletes []string
}

func (s *callRecordingStore) Head(ctx context.Context, key string) (objstore.Object, error) {
	s.mu.Lock()
	s.heads = append(s.heads, key)
	s.mu.Unlock()
	return s.Store.Head(ctx, key)
}

func (s *callRecordingStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deletes = append(s.deletes, key)
	s.mu.Unlock()
	return s.Store.Delete(ctx, key)
}

func (s *callRecordingStore) calls() (heads, deletes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.heads...), append([]string(nil), s.deletes...)
}

// untouchableStore は、呼ばれたらテストを落とす置き場。S3 の派生物の置き場に見立てる。
type untouchableStore struct {
	objstore.Store
	t *testing.T
}

func (s untouchableStore) Head(_ context.Context, key string) (objstore.Object, error) {
	s.t.Errorf("derived object store was asked to Head %q; the key belongs to the filesystem store", key)
	return objstore.Object{}, objstore.ErrNotFound
}

func (s untouchableStore) Delete(_ context.Context, key string) error {
	s.t.Errorf("derived object store was asked to Delete %q; the key belongs to the filesystem store", key)
	return nil
}

func (s untouchableStore) List(_ context.Context, _ string) ([]objstore.Object, error) {
	return nil, nil
}

func (s *callRecordingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heads, s.deletes = nil, nil
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// derivedS3SwitchedWorker は、派生物の置き場を S3 相当の別の置き場に切り替えた後の FS の置き場の worker を返す。
// FS の置き場には旧い派生物のコピーが残る。
func derivedS3SwitchedWorker(t *testing.T, mediaDir string, fsObjects objstore.Store, pool store.MediaDelete, storage store.StorageObservation) *DeleteReconcileWorker {
	t.Helper()
	if err := mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName); err != nil {
		t.Fatalf("writing media store marker: %v", err)
	}
	return &DeleteReconcileWorker{
		MediaDelete: pool, Storage: storage,
		MediaDir: mediaDir, MediaStoreName: dbtest.StoreName,
		StorageObjects:         map[string]objstore.Store{dbtest.StoreName: fsObjects},
		DerivedObjects:         untouchableStore{Store: testFilesystemObjectStore(t, t.TempDir()), t: t},
		DerivedObjectStoreName: objectStoreName,
		TrashRetention:         30 * 24 * time.Hour,
	}
}

// 派生物の置き場が S3 に切り替わった後も、FS の置き場に残る encoded 本体と字幕の旧コピーは、
// FS の置き場の Store で消す（S3 の置き場へ問い合わせず、os.Remove にも戻さない）。
func TestDeleteReconcileWorker_StaleFilesystemDerivedCopyIsDeletedThroughItsFilesystemStore(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	const relPath = "encoded/old-copy.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, relPath, []byte("encoded"))
	const subtitleRelPath = "encoded/old-copy.vtt"
	if err := os.WriteFile(filepath.Join(mediaDir, filepath.FromSlash(subtitleRelPath)), []byte("WEBVTT\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markRecordingTrashed(t, pool, recordingID)

	recorder := &callRecordingStore{Store: testFilesystemObjectStore(t, mediaDir)}
	media := pgstore.NewMediaWithStores(pool, dbtest.StoreName, dbtest.StoreName, objectStoreName)
	w := derivedS3SwitchedWorker(t, mediaDir, recorder, media, media)
	w.RecordingPurge = pgstoretest.Recordings(pool)
	w.Breakers = pgstoretest.Breakers(pool)
	if err := runWorkerViaJobQFake(ctx, w, DeleteReconcileArgs{}); err != nil {
		t.Fatalf("Work(): %v", err)
	}

	if got := singleCopyState(t, pool, assetID); got != "deleted" {
		t.Errorf("filesystem derived copy state = %q, want deleted", got)
	}
	_, deletes := recorder.calls()
	for _, key := range []string{relPath, subtitleRelPath} {
		if !contains(deletes, key) {
			t.Errorf("filesystem store Delete calls = %v, want %q among them", deletes, key)
		}
		if fileExists(filepath.Join(mediaDir, filepath.FromSlash(key))) {
			t.Errorf("%q still exists after deletion", key)
		}
	}
}

// deleting のまま止まった旧い FS コピーの現存確認と、実体無しの監査も FS の置き場の Store の Head を通る。
func TestDeleteReconcileWorker_StaleFilesystemDerivedCopyChecksAreHeadsOnItsFilesystemStore(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	const relPath = "encoded/old-copy.mp4"
	assetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, relPath, []byte("encoded"))

	recorder := &callRecordingStore{Store: testFilesystemObjectStore(t, mediaDir)}
	media := pgstore.NewMediaWithStores(pool, dbtest.StoreName, dbtest.StoreName, objectStoreName)
	w := derivedS3SwitchedWorker(t, mediaDir, recorder, media, media)

	// 実体無しの監査: 走査結果（seenOnDisk）が空でも、Head で見つかるので欠落にならない。
	if _, err := w.reconcileMissingAssets(ctx, map[string]struct{}{"unrelated": {}}); err != nil {
		t.Fatalf("reconcileMissingAssets: %v", err)
	}
	var missing int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM missing_media_assets").Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Errorf("missing_media_assets = %d, want 0 (the filesystem store has the key)", missing)
	}
	if heads, _ := recorder.calls(); !contains(heads, relPath) {
		t.Errorf("audit Head calls = %v, want %q among them", heads, relPath)
	}

	recorder.reset()
	// 巻き戻し判定: deleting にして条件から外れた行は、FS の置き場の Head で現存を確かめて active に戻る。
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleting' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	if err := w.restoreUnqualifiedDeletingAssets(ctx, time.Now(), time.Now(), nil, objectStoreName); err != nil {
		t.Fatalf("restoreUnqualifiedDeletingAssets: %v", err)
	}
	if got := singleCopyState(t, pool, assetID); got != "active" {
		t.Errorf("copy state after revert = %q, want active", got)
	}
	if heads, _ := recorder.calls(); !contains(heads, relPath) {
		t.Errorf("revert-check Head calls = %v, want %q among them", heads, relPath)
	}

	// FS の置き場から実体が消えていれば、巻き戻しではなく deleted に確定する（Head の ErrNotFound を不在として扱う）。
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleting' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(mediaDir, filepath.FromSlash(relPath))); err != nil {
		t.Fatal(err)
	}
	if err := w.restoreUnqualifiedDeletingAssets(ctx, time.Now(), time.Now(), nil, objectStoreName); err != nil {
		t.Fatalf("restoreUnqualifiedDeletingAssets (absent): %v", err)
	}
	if got := singleCopyState(t, pool, assetID); got != "deleted" {
		t.Errorf("copy state after revert with the key absent = %q, want deleted", got)
	}
}

// fsstore の staging ファイルは fsstore 内部のファイルで、objstore の key ではない。
// 派生物の置き場が S3 でも、FS の置き場の古い staging は走査した root 上で消え、記録も消える。
func TestDeleteReconcileWorker_FilesystemStagingOrphanIsRemovedWithoutAskingTheDerivedStore(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	const relPath = mediapath.GeneratedAssetTempFilePrefix + "leftover"
	full := filepath.Join(mediaDir, relPath)
	if err := os.WriteFile(full, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO orphan_files (store, rel_path, first_seen) VALUES ($1, $2, $3)", dbtest.StoreName, relPath, old); err != nil {
		t.Fatal(err)
	}
	media := pgstore.NewMediaWithStores(pool, dbtest.StoreName, dbtest.StoreName, objectStoreName)
	w := derivedS3SwitchedWorker(t, mediaDir, testFilesystemObjectStore(t, mediaDir), media, media)
	w.RecordingPurge = pgstoretest.Recordings(pool)
	w.Breakers = pgstoretest.Breakers(pool)
	w.OrphanMTimeGrace = 7 * 24 * time.Hour
	w.OrphanAge = 14 * 24 * time.Hour
	if err := runWorkerViaJobQFake(ctx, w, DeleteReconcileArgs{}); err != nil {
		t.Fatalf("Work(): %v", err)
	}
	if _, err := os.Lstat(full); !os.IsNotExist(err) {
		t.Errorf("aged staging file stat error = %v, want not-exist", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("orphan_files rows for the staging file = %d, want 0", count)
	}
}

// putCommitPublisher は Prepare と PublishEncoded を成功させるだけの派生物の公開契約。
type putCommitPublisher struct {
	store.DerivedMediaPublisher
}

func (*putCommitPublisher) PreparePendingMediaAssetCopy(context.Context, store.PendingMediaAssetCopy) error {
	return nil
}

func (*putCommitPublisher) PublishEncoded(_ context.Context, _ store.EncodedPublishInput, before func() error) (bool, error) {
	return true, before()
}

// beforeEncodeObjectPut は本体の Put の前に 1 回だけ呼ぶ。字幕の Put と commit の前では呼ばない。
func TestPublishEncodedCallsBeforeEncodeObjectPutOnlyBeforeTheBodyPut(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "out.mp4")
	subtitle := filepath.Join(t.TempDir(), "out.vtt")
	for path, body := range map[string]string{scratch: "data", subtitle: "WEBVTT\n\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := &EncodeWorker{
		Objects: testFSObjectStore(mediaDir), ObjectPublisher: &putCommitPublisher{},
		MediaDir: mediaDir, MediaStoreName: "tokyo",
	}
	calls := 0
	previous := beforeEncodeObjectPut
	beforeEncodeObjectPut = func(string) { calls++ }
	t.Cleanup(func() { beforeEncodeObjectPut = previous })

	_, published, err := w.publishEncoded(context.Background(), encodePublishInput{
		relPath: "sites/tokyo/out.mp4", scratchOut: scratch, subtitleOut: subtitle, withSubtitles: true,
	})
	if err != nil || !published {
		t.Fatalf("publishEncoded() = published:%t err:%v, want published", published, err)
	}
	if calls != 1 {
		t.Errorf("beforeEncodeObjectPut calls = %d, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "sites", "tokyo", "out.vtt")); err != nil {
		t.Errorf("subtitle companion was not published: %v", err)
	}
}
