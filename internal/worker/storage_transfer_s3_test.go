package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// s3TransferTestConfig は FS の置き場 test-store と、派生物の置き場 cloud（S3）の構成を返す。
// cloud は Roots に入れない（root も目印も無い）。転送は test-store の worker が cloud へ書く。
func s3TransferTestConfig() config.StorageConfig {
	return config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"cloud":      {Kind: "s3", Bucket: "rokuban-test"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "cloud",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
}

// s3DerivedFixture は、test-store にだけある encoded 派生物を、cloud へ転送する準備をする。
// cloud の objstore は fakeResumableStore（fsstore の埋め込み）で、中身は cloudRoot に書かれる。
type s3DerivedFixture struct {
	pool      *pgxpool.Pool
	root      string
	cloudRoot string
	cfg       config.StorageConfig
	worker    StorageTransferWorker
	args      jobs.StorageTransferArgs
	assetID   int64
	relPath   string
	content   []byte
}

func newS3DerivedFixture(t *testing.T) s3DerivedFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store")
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	relPath := "sites/default/transfer/s3.mp4"
	content := bytes.Repeat([]byte("s3-derived-bytes-"), 20000)
	assetID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, relPath, content)
	sum := sha256.Sum256(content)
	if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET sha256 = $1 WHERE id = $2", sum[:], assetID); err != nil {
		t.Fatalf("setting logical checksum: %v", err)
	}
	cfg := s3TransferTestConfig()
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })

	sourceFS, err := fsstore.New(roots["test-store"])
	if err != nil {
		t.Fatal(err)
	}
	cloudRoot := t.TempDir()
	cloudFS, err := fsstore.New(cloudRoot)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]objstore.Store{
		"test-store": &fakeResumableStore{Store: sourceFS},
		"cloud":      &fakeResumableStore{Store: cloudFS},
	}
	return s3DerivedFixture{
		pool: pool, root: roots["test-store"], cloudRoot: cloudRoot, cfg: cfg,
		worker: StorageTransferWorker{
			Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg, Roots: map[string]string{"test-store": roots["test-store"]},
			ObjectStores: objects, ScratchDir: t.TempDir(),
		},
		args:    jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "cloud", SourceStore: "test-store", PolicyFingerprint: fingerprint},
		assetID: assetID, relPath: relPath, content: content,
	}
}

// cloudCopyState は cloud のコピーの状態を返す。行が無ければ空文字。
func (f s3DerivedFixture) cloudCopyState(t *testing.T) string {
	t.Helper()
	var state string
	err := f.pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'cloud'`, f.assetID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading cloud copy state: %v", err)
	}
	return state
}

// driveToEnd は Work を繰り返し、cloud のコピーが active になるまで進める。途中の snooze は slice の期限だけを許し、その回数を返す。
func (f s3DerivedFixture) driveToEnd(t *testing.T) int {
	t.Helper()
	snoozes := 0
	for range 300 {
		err := f.worker.Work(context.Background(), f.args)
		if err == nil {
			if state := f.cloudCopyState(t); state != "active" {
				t.Fatalf("Work finished without error but the cloud copy is %q", state)
			}
			return snoozes
		}
		requireSliceSnooze(t, err, "driving the S3 transfer to the end")
		snoozes++
	}
	t.Fatal("S3 transfer did not finish in 300 Work calls")
	return snoozes
}

// TestStorageTransferWorkerSendsDerivedCopyToS3FromFSSource は、S3 の派生物への転送が、転送元の FS 置き場の worker で
// slice をまたいで完了し、cloud のコピーが active になることを確かめる。cloud には root も目印も無い。
func TestStorageTransferWorkerSendsDerivedCopyToS3FromFSSource(t *testing.T) {
	f := newS3DerivedFixture(t)
	snoozes := f.driveToEnd(t)
	if snoozes == 0 {
		t.Fatal("transfer finished in one slice; the test needs several slices to exercise resumption")
	}
	got, err := os.ReadFile(filepath.Join(f.cloudRoot, filepath.FromSlash(f.relPath)))
	if err != nil || !bytes.Equal(got, f.content) {
		t.Fatalf("cloud object = %d bytes, %v; want the %d source bytes", len(got), err, len(f.content))
	}
}

// TestStorageTransferWorkerRequiresSourceMountForS3Destination は、S3 宛ての転送を転送元の FS 置き場がマウントされていない
// worker が受け取ったら、FS 宛ての未マウントと同じメトリクスと（破棄でない）エラーで止まることを確かめる。
func TestStorageTransferWorkerRequiresSourceMountForS3Destination(t *testing.T) {
	f := newS3DerivedFixture(t)
	f.worker.Roots = map[string]string{}
	before := promtestutil.ToFloat64(metrics.StorageTransferMountMismatches.WithLabelValues("test-store"))
	err := f.worker.Work(context.Background(), f.args)
	if err == nil {
		t.Fatal("Work() error = nil, want the source store to be reported as not mounted")
	}
	if errors.Is(err, jobq.ErrDiscard) {
		t.Fatalf("Work() error = %v, want a retryable error (not discarded)", err)
	}
	if got := promtestutil.ToFloat64(metrics.StorageTransferMountMismatches.WithLabelValues("test-store")); got != before+1 {
		t.Fatalf("mount mismatch counter = %v, want %v", got, before+1)
	}
	if state := f.cloudCopyState(t); state != "" {
		t.Fatalf("cloud copy state = %q, want no reservation when the source is not mounted", state)
	}
}

// TestStorageTransferReconcileRoutesS3CopiesToSourceQueue は、reconcile が FS 宛ての候補を宛先のキューに、
// S3 宛ての候補を転送元のキューに積むことを確かめる。原本は FS 宛てにしかならない。
func TestStorageTransferReconcileRoutesS3CopiesToSourceQueue(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "test-store", "archive")
	originalID := transferTestOriginal(t, pool, roots["test-store"], []byte("original to archive"))
	recordingID := dbtest.Recording(t, pool)
	profile := "web"
	encodedID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/routed.mp4", []byte("encoded to cloud"))
	cfg := s3TransferTestConfig()
	cfg.Stores["archive"] = config.StoreConfig{Kind: "fs"}
	cfg.Originals = []config.OriginalStoreRule{{Store: "archive"}}
	enqueuer := newRecordingEnqueuer()
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMedia(pool, "test-store", ""), Config: cfg}
	if err := reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}); err != nil {
		t.Fatalf("reconcile Work: %v", err)
	}
	var fsJobs, s3Jobs []jobs.StorageTransferArgs
	for _, a := range enqueuer.Enqueued("storage_transfer") {
		got, ok := a.(jobs.StorageTransferArgs)
		if !ok {
			t.Fatalf("enqueued arg type = %T, want jobs.StorageTransferArgs", a)
		}
		if got.Store == "cloud" {
			s3Jobs = append(s3Jobs, got)
		} else {
			fsJobs = append(fsJobs, got)
		}
	}
	if len(fsJobs) != 1 || fsJobs[0].MediaAssetID != originalID || fsJobs[0].Store != "archive" || fsJobs[0].SourceStore != "" {
		t.Fatalf("FS-destination jobs = %+v, want one copy of asset %d to archive with no source store", fsJobs, originalID)
	}
	if queue, err := jobs.PhysicalQueueFor(fsJobs[0]); err != nil || queue != "transfer_archive" {
		t.Fatalf("FS-destination queue = %q, %v; want transfer_archive", queue, err)
	}
	if len(s3Jobs) != 1 || s3Jobs[0].MediaAssetID != encodedID || s3Jobs[0].SourceStore != "test-store" {
		t.Fatalf("S3-destination jobs = %+v, want one copy of encoded asset %d sourced from test-store", s3Jobs, encodedID)
	}
	if queue, err := jobs.PhysicalQueueFor(s3Jobs[0]); err != nil || queue != "transfer_test-store" {
		t.Fatalf("S3-destination queue = %q, %v; want transfer_test-store", queue, err)
	}
}

// TestStorageTransferS3PendingReservationIsNotObsolete は、S3 の派生物の pending 予約を、FS の置き場だけを見る
// 掃除の判定が消さないことを確かめる。派生物の宛先は p_derived_store で判定される。
func TestStorageTransferS3PendingReservationIsNotObsolete(t *testing.T) {
	f := newS3DerivedFixture(t)
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT a.id, 'cloud', a.id::text || '.mp4', COALESCE(a.derived_generation, 0), 'pending'
FROM media_assets a WHERE a.id = $1`, f.assetID); err != nil {
		t.Fatalf("inserting pending S3 reservation: %v", err)
	}
	rules := mustOriginalRules(t, f.cfg)
	cloud := pgstore.NewMedia(f.pool, "cloud", "")
	obsolete, err := cloud.ListObsoletePendingMediaAssetTransferCopies(context.Background(), rules, f.cfg.FSStoreNames(), f.cfg.Derived, 10)
	if err != nil {
		t.Fatalf("listing obsolete pending copies: %v", err)
	}
	if len(obsolete) != 0 {
		t.Fatalf("obsolete pending copies = %+v, want none for a current S3 derived reservation", obsolete)
	}
}

// headSHAStore は fsstore の Head に SHA-256 を載せ、Get の回数を数える。S3 がメタデータに SHA-256 を持つ置き場を真似る。
type headSHAStore struct {
	*fsstore.Store
	sha  string
	gets int
}

func (s *headSHAStore) Head(ctx context.Context, key string) (objstore.Object, error) {
	obj, err := s.Store.Head(ctx, key)
	if err != nil {
		return obj, err
	}
	obj.SHA256 = s.sha
	return obj, nil
}

func (s *headSHAStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.gets++
	return s.Store.Get(ctx, key)
}

// TestExistingTransferredObjectUsesHeadSHA256 は、既存の宛先キーの照合が Head の SHA-256 があれば中身を読まず、
// 無ければ読んで測ることを確かめる。
func TestExistingTransferredObjectUsesHeadSHA256(t *testing.T) {
	ctx := context.Background()
	content := []byte("wanted")
	digest := sha256.Sum256(content)
	tests := []struct {
		name      string
		headSHA   string
		wantExist bool
		wantErr   error
		wantGets  int
	}{
		{name: "trusts the head SHA-256 without reading", headSHA: hex.EncodeToString(digest[:]), wantExist: true, wantGets: 0},
		{name: "reads the object when the head has no SHA-256", headSHA: "", wantExist: true, wantGets: 1},
		{name: "rejects a head SHA-256 that differs", headSHA: hex.EncodeToString(make([]byte, sha256.Size)), wantErr: objstore.ErrAlreadyExists, wantGets: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, err := fsstore.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.Put(ctx, "encoded/head.mp4", strings.NewReader(string(content)), int64(len(content)), digest, "video/mp4"); err != nil {
				t.Fatalf("seeding object: %v", err)
			}
			objects := &headSHAStore{Store: fs, sha: tt.headSHA}
			exists, err := existingTransferredObjectMatches(ctx, objects, "encoded/head.mp4", digest[:], int64(len(content)), false)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("existingTransferredObjectMatches error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("existingTransferredObjectMatches: %v", err)
			}
			if exists != tt.wantExist {
				t.Errorf("exists = %t, want %t", exists, tt.wantExist)
			}
			if objects.gets != tt.wantGets {
				t.Errorf("Get calls = %d, want %d", objects.gets, tt.wantGets)
			}
		})
	}
}

// TestStorageTransferS3CopyLetsDeleteReconcileRetireFSCopy は、FS 1 つ + S3 の派生物の構成で、reconcile → 転送 → 配置の削除
// の順に進んだとき、S3 のコピーが active になった後に FS の旧いコピーが消えることを確かめる。
func TestStorageTransferS3CopyLetsDeleteReconcileRetireFSCopy(t *testing.T) {
	f := newS3DerivedFixture(t)
	f.worker.Config.Transfer.Bandwidth = nil
	enqueuer := newRecordingEnqueuer()
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMedia(f.pool, "test-store", ""), Config: f.cfg}
	if err := reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}); err != nil {
		t.Fatalf("reconcile Work: %v", err)
	}
	queued := enqueuer.Enqueued("storage_transfer")
	if len(queued) != 1 {
		t.Fatalf("enqueued storage_transfer jobs = %d, want 1", len(queued))
	}
	job, ok := queued[0].(jobs.StorageTransferArgs)
	if !ok || job.Store != "cloud" || job.SourceStore != "test-store" {
		t.Fatalf("enqueued job = %#v, want a cloud copy sourced from test-store", queued[0])
	}
	f.args = job
	f.driveToEnd(t)

	// S3 のコピーが overlap の期間を過ぎてから、FS の旧いコピーの配置の削除を走らせる。
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'cloud'`, f.assetID); err != nil {
		t.Fatal(err)
	}
	media := pgstore.NewMedia(f.pool, "test-store", "")
	deleter := DeleteReconcileWorker{
		MediaDelete: media, Storage: media, StorageTransfer: media, StorageTransferPolicy: f.cfg,
		RecordingPurge: pgstore.NewRecordings(f.pool, "test-store"), Breakers: pgstore.NewBreakers(f.pool),
		MediaDir: f.root, StorageObjects: testStorageObjects(t, "test-store", f.root), MediaStoreName: "test-store", TransferOverlap: time.Hour,
		DerivedObjects: f.worker.ObjectStores["cloud"],
	}
	if err := deleter.Work(context.Background(), jobs.DeleteReconcileArgs{}); err != nil {
		t.Fatalf("DeleteReconcileWorker.Work: %v", err)
	}
	var fsState string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store'`, f.assetID).Scan(&fsState); err != nil {
		t.Fatal(err)
	}
	if fsState != "deleted" {
		t.Fatalf("FS derived copy state = %q, want deleted once the S3 copy is active", fsState)
	}
	if _, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(f.relPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FS derived file stat error = %v, want not-exist", err)
	}
	if got := f.cloudCopyState(t); got != "active" {
		t.Fatalf("cloud copy state = %q, want active", got)
	}
}
