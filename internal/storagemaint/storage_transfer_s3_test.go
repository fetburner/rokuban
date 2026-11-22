package storagemaint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
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
	cfg := config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"test-store": {Kind: "fs"},
			"cloud":      {Kind: "s3", Bucket: "rokuban-test"},
		},
		Originals: []config.OriginalStoreRule{{Store: "test-store"}},
		Derived:   "cloud",
		Transfer:  config.StorageTransferConfig{Overlap: 2 * time.Hour},
	}
	return cfg
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
	testutil.MustExec(t, pool, "setting logical checksum", "UPDATE media_assets SET sha256 = $1 WHERE id = $2", sum[:], assetID)
	cfg := s3TransferTestConfig()
	cfg.Transfer.Bandwidth = map[string]string{"test-store": "64KiBps"}
	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
	restore := storageTransferSlice
	storageTransferSlice = 150 * time.Millisecond
	t.Cleanup(func() { storageTransferSlice = restore })

	sourceFS, err := fsstore.New(roots["test-store"])
	testutil.RequireNoError(t, err, "creating source fsstore")
	cloudRoot := t.TempDir()
	cloudFS, err := fsstore.New(cloudRoot)
	testutil.RequireNoError(t, err, "creating cloud fsstore")
	objects := map[string]objstore.Store{
		"test-store": &fakeResumableStore{Store: sourceFS},
		"cloud":      &fakeResumableStore{Store: cloudFS},
	}
	return s3DerivedFixture{
		pool: pool, root: roots["test-store"], cloudRoot: cloudRoot, cfg: cfg,
		worker: StorageTransferWorker{
			Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg, Roots: map[string]string{"test-store": roots["test-store"]},
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
	testutil.RequireNoError(t, err, "reading cloud copy state")
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

// TestStorageTransferWorkerSendsDerivedCopyToS3FromFSSource は、S3 と未対応種類名の派生物への転送が、転送元の FS 置き場の
// worker で slice をまたいで完了し、cloud のコピーが active になることを確かめる。遠隔の宛先には root も目印も無い。
func TestStorageTransferWorkerSendsDerivedCopyToS3FromFSSource(t *testing.T) {
	for _, tc := range []struct {
		kind string
	}{{kind: "s3"}, {kind: "gcs"}} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newS3DerivedFixture(t)
			f.cfg.Stores["cloud"] = config.StoreConfig{Kind: tc.kind, Bucket: "rokuban-test"}
			f.worker.Config = f.cfg
			fingerprint, err := f.cfg.PolicyFingerprint()
			testutil.RequireNoError(t, err, "cfg.PolicyFingerprint")
			f.args.PolicyFingerprint = fingerprint
			snoozes := f.driveToEnd(t)
			if snoozes == 0 {
				t.Fatal("transfer finished in one slice; the test needs several slices to exercise resumption")
			}
			relPath := transferTestCopyRelPath(t, f.pool, f.assetID, "cloud")
			got, err := os.ReadFile(filepath.Join(f.cloudRoot, filepath.FromSlash(relPath)))
			if err != nil || !bytes.Equal(got, f.content) {
				t.Fatalf("cloud object = %d bytes, %v; want the %d source bytes", len(got), err, len(f.content))
			}
		})
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
// S3 と未対応種類名の遠隔宛て候補を転送元のキューに積むことを確かめる。原本は FS 宛てにしかならない。
func TestStorageTransferReconcileRoutesS3CopiesToSourceQueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind string
	}{{name: "s3", kind: "s3"}, {name: "gcs", kind: "gcs"}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			roots := transferTestRoots(t, "test-store", "archive")
			originalID := transferTestOriginal(t, pool, roots["test-store"], []byte("original to archive"))
			recordingID := dbtest.Recording(t, pool)
			profile := "web"
			encodedID := seedEncodedOrThumbnailAsset(t, pool, roots["test-store"], recordingID, store.AssetKindEncoded, &profile, "sites/default/transfer/routed.mp4", []byte("encoded to cloud"))
			cfg := s3TransferTestConfig()
			cfg.Stores["cloud"] = config.StoreConfig{Kind: tc.kind, Bucket: "rokuban-test"}
			cfg.Stores["archive"] = config.StoreConfig{Kind: "fs"}
			cfg.Originals = []config.OriginalStoreRule{{Store: "archive"}}
			enqueuer := &jobqfaketest.RecordingEnqueuer{}
			reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMediaInStore(pool, "test-store", mustStoragePolicy(t, cfg)), Config: cfg}
			testutil.RequireNoError(t, reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "reconcile Work")
			var fsJobs, remoteJobs []jobs.StorageTransferArgs
			for _, call := range enqueuer.CallsOfKind("storage_transfer") {
				got, ok := call.Args.(jobs.StorageTransferArgs)
				if !ok {
					t.Fatalf("enqueued arg type = %T, want jobs.StorageTransferArgs", call.Args)
				}
				if got.Store == "cloud" {
					remoteJobs = append(remoteJobs, got)
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
			if len(remoteJobs) != 1 || remoteJobs[0].MediaAssetID != encodedID || remoteJobs[0].SourceStore != "test-store" {
				t.Fatalf("remote-destination jobs = %+v, want one copy of encoded asset %d sourced from test-store", remoteJobs, encodedID)
			}
			if queue, err := jobs.PhysicalQueueFor(remoteJobs[0]); err != nil || queue != "transfer_test-store" {
				t.Fatalf("remote-destination queue = %q, %v; want transfer_test-store", queue, err)
			}
		})
	}
}

// TestStorageTransferS3PendingReservationIsNotObsolete は、S3 の派生物の pending 予約を、FS の置き場だけを見る
// 掃除の判定が消さないことを確かめる。派生物の宛先は p_derived_store で判定される。
func TestStorageTransferS3PendingReservationIsNotObsolete(t *testing.T) {
	f := newS3DerivedFixture(t)
	dbtest.MediaAssetCopy(t, f.pool, f.assetID, func(p *dbtest.MediaAssetCopyParams) {
		p.Store = "cloud"
		p.RelPath = fmt.Sprintf("%d.mp4", f.assetID)
		p.State = "pending"
	})
	cloud := pgstore.NewMediaInStore(f.pool, "cloud", mustStoragePolicy(t, f.cfg))
	obsolete, err := cloud.ListObsoletePendingMediaAssetTransferCopies(context.Background(), 10)
	testutil.RequireNoError(t, err, "listing obsolete pending copies")
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
	t.Parallel()
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
			testutil.RequireNoError(t, err, "creating temp fsstore")
			testutil.RequireNoError(t, fs.Put(ctx, "encoded/head.mp4", strings.NewReader(string(content)), int64(len(content)), digest, "video/mp4"), "seeding object")
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
	enqueuer := &jobqfaketest.RecordingEnqueuer{}
	reconcile := StorageTransferReconcileWorker{Enqueuer: enqueuer, Storage: pgstore.NewMediaInStore(f.pool, "test-store", mustStoragePolicy(t, f.cfg)), Config: f.cfg}
	testutil.RequireNoError(t, reconcile.Work(context.Background(), jobs.StorageTransferReconcileArgs{}), "reconcile Work")
	queued := enqueuer.CallsOfKind("storage_transfer")
	if len(queued) != 1 {
		t.Fatalf("enqueued storage_transfer jobs = %d, want 1", len(queued))
	}
	job, ok := queued[0].Args.(jobs.StorageTransferArgs)
	if !ok || job.Store != "cloud" || job.SourceStore != "test-store" {
		t.Fatalf("enqueued job = %#v, want a cloud copy sourced from test-store", queued[0].Args)
	}
	f.args = job
	f.driveToEnd(t)

	media := pgstore.NewMediaInStore(f.pool, "test-store", mustStoragePolicy(t, f.cfg))
	deleter := DeleteReconcileWorker{
		DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(f.pool, "test-store")}, Storage: media,
		Breakers: pgstore.NewBreakers(f.pool),
		MediaDir: f.root, StorageObjects: testStorageObjects(t, "test-store", f.root), MediaStoreName: "test-store", TransferOverlap: time.Hour,
		DerivedObjects: f.worker.ObjectStores["cloud"],
	}
	testutil.RequireNoError(t, deleter.Work(t.Context(), jobs.DeleteReconcileArgs{}), "reconciling before overlap")
	if got := storeCopyState(t, f.pool, "test-store", f.assetID); got != "active" {
		t.Fatalf("FS copy before overlap = %q, want active", got)
	}
	if _, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(f.relPath))); err != nil {
		t.Fatalf("checking FS object before overlap: %v", err)
	}

	// S3 のコピーが overlap の期間を過ぎてから、FS の旧いコピーの配置の削除を走らせる。
	testutil.MustExec(t, f.pool, "aging the cloud copy past the overlap", `UPDATE media_asset_copies SET created_at = now() - interval '3 hours' WHERE media_asset_id = $1 AND store = 'cloud'`, f.assetID)
	testutil.RequireNoError(t, deleter.Work(context.Background(), jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	fsState := storeCopyState(t, f.pool, "test-store", f.assetID)
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

// TestStagingFilesystemReclaimsPendingPublicationAfterGrace は、転送元になった旧 FS 宛先で
// 旧い転送予約も生成中の派生物も公開の猶予まで残すことを検査する。
// 転送の所有記録だけが実体の削除を許し、進捗の送信状態は掃除待ちに移す。
func TestStagingFilesystemReclaimsPendingPublicationAfterGrace(t *testing.T) {
	for _, tc := range []struct {
		name             string
		recordedTransfer bool
	}{
		{name: "before progress was recorded"},
		{name: "with progress and owned main", recordedTransfer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newS3DerivedFixture(t)
			ctx := t.Context()
			roots := transferTestRoots(t, "archive")
			root := roots["archive"]
			legacy := transferTestConfig("archive", "test-store", "archive")
			legacyMedia := pgstore.NewMediaInStore(f.pool, "archive", mustStoragePolicy(t, legacy))
			reservation, ok, err := legacyMedia.PrepareMediaAssetTransfer(ctx, f.assetID, "archive", legacy.FSStoreNames(), legacy.FSStoreNames(), legacy.FSStoreNames(), store.MediaAssetTransferAttemptLimit)
			if !ok || err != nil {
				t.Fatalf("preparing legacy FS transfer = (%+v, %t, %v), want pending reservation", reservation, ok, err)
			}
			f.cfg.Stores["archive"] = config.StoreConfig{Kind: "fs"}
			media := pgstore.NewMediaInStore(f.pool, "archive", mustStoragePolicy(t, f.cfg))
			archive, err := fsstore.New(root)
			testutil.RequireNoError(t, err, "opening archive object store")
			objects := map[string]objstore.Store{"archive": archive}
			var upload []byte
			testutil.RequireNoError(t, archive.Put(ctx, reservation.RelPath, bytes.NewReader(f.content), int64(len(f.content)), sha256.Sum256(f.content), "video/mp4"), "putting legacy destination key")
			subtitle, err := mediapath.SubtitleSibling(reservation.RelPath)
			testutil.RequireNoError(t, err, "deriving legacy subtitle key")
			foreign := []byte("foreign subtitle")
			testutil.RequireNoError(t, archive.Put(ctx, subtitle, bytes.NewReader(foreign), int64(len(foreign)), sha256.Sum256(foreign), "text/vtt"), "putting unowned legacy sidecar")
			if tc.recordedTransfer {
				owner := uuid.New()
				progress, acquired, err := legacyMedia.AcquireMediaAssetTransferProgress(ctx, reservation.CopyID, owner, time.Minute)
				if !acquired || err != nil {
					t.Fatalf("acquiring legacy progress = (%+v, %t, %v)", progress, acquired, err)
				}
				upload, err = archive.StartUpload(ctx, reservation.RelPath, int64(len(f.content)), sha256.Sum256(f.content), "video/mp4")
				testutil.RequireNoError(t, err, "creating FS upload state")
				testutil.RequireNoError(t, legacyMedia.ReplaceMediaAssetTransferUpload(ctx, reservation.CopyID, owner, progress.WriterGeneration, "archive", upload), "storing legacy upload state")
				testutil.RequireNoError(t, legacyMedia.RecordMediaAssetTransferFailure(ctx, reservation.CopyID, "legacy transfer interrupted"), "recording legacy transfer failure")
				ownership, err := legacyMedia.MarkMediaAssetTransferObjectOwned(ctx, reservation.CopyID, "archive", reservation.RelPath)
				if ownership != store.TransferObjectOwned || err != nil {
					t.Fatalf("recording legacy ownership = (%v, %v), want owned", ownership, err)
				}
			}
			recordingID := dbtest.Recording(t, f.pool)
			key := fmt.Sprintf("thumbnails/%d.jpg", recordingID)
			payload := []byte("not yet committed")
			testutil.RequireNoError(t, media.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
				RecordingID: recordingID, Kind: store.AssetKindThumbnail, RelPath: key, SizeBytes: int64(len(payload)), ReserveNewGeneration: true,
			}), "reserving a generated key on FS")
			testutil.RequireNoError(t, objects["archive"].Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "image/jpeg"), "putting pending object on FS")

			deleter := DeleteReconcileWorker{
				DeleteReconcileStores: store.DeleteReconcileStores{
					MediaDelete: media, MediaPendingReclaim: media, StorageTransfer: media, RecordingPurge: pgstore.NewRecordings(f.pool, "archive"),
				},
				Storage: media, Breakers: pgstore.NewBreakers(f.pool),
				MediaDir: root, MediaStoreName: "archive", StorageObjects: objects,
				DerivedObjectStoreName: "cloud", FilesystemDerivedPublisher: true,
				MaxDeletesPerPass: 1, OrphanAge: 24 * time.Hour,
			}
			obsolete, err := media.ListObsoletePendingMediaAssetTransferCopies(ctx, 10)
			testutil.RequireNoError(t, err, "listing obsolete reservations on staging FS")
			if len(obsolete) != 0 {
				t.Fatalf("obsolete reservations = %v, want none", obsolete)
			}
			testutil.RequireNoError(t, deleter.Work(ctx, jobs.DeleteReconcileArgs{}), "reclaiming before publication grace")
			if got := copyStateIn(t, f.pool, f.assetID, "archive"); got != "pending" {
				t.Fatalf("legacy transfer reservation before grace = %q, want pending", got)
			}
			requireState := func(want string) {
				t.Helper()
				var got string
				testutil.RequireNoError(t, f.pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE store = 'archive' AND rel_path = $1`, key).Scan(&got), "reading pending publication state")
				if got != want {
					t.Fatalf("FS publication state = %q, want %q", got, want)
				}
			}
			requireState("pending")
			_, err = objects["archive"].Head(ctx, key)
			testutil.RequireNoError(t, err, "checking pending object before grace")

			testutil.MustExec(t, f.pool, "aging legacy transfer reservation", `UPDATE media_asset_copies SET created_at = now() - interval '48 hours' WHERE id = $1`, reservation.CopyID)
			testutil.RequireNoError(t, deleter.Work(ctx, jobs.DeleteReconcileArgs{}), "reclaiming the aged legacy reservation")
			if got := copyStateIn(t, f.pool, f.assetID, "archive"); got != "deleted" {
				t.Fatalf("legacy transfer reservation after grace = %q, want deleted", got)
			}
			_, err = archive.Head(ctx, subtitle)
			testutil.RequireNoError(t, err, "checking unowned legacy subtitle is retained")
			_, mainErr := archive.Head(ctx, reservation.RelPath)
			if tc.recordedTransfer {
				if !errors.Is(mainErr, objstore.ErrNotFound) {
					t.Fatalf("owned legacy destination: HEAD error = %v, want ErrNotFound", mainErr)
				}
				var progressRows, ownershipRows, attemptRows, abandonedRows int
				testutil.RequireNoError(t, f.pool.QueryRow(ctx, `SELECT
		  (SELECT count(*) FROM media_asset_copy_transfer_progress WHERE copy_id = $1),
		  (SELECT count(*) FROM media_asset_transfer_objects WHERE copy_id = $1),
		  (SELECT count(*) FROM media_asset_transfer_attempts WHERE copy_id = $1),
		  (SELECT count(*) FROM media_asset_transfer_abandoned_uploads WHERE store = 'archive' AND upload_state = $2)
		`, reservation.CopyID, upload).Scan(&progressRows, &ownershipRows, &attemptRows, &abandonedRows), "checking retired transfer state")
				if progressRows != 0 || ownershipRows != 0 || attemptRows != 0 || abandonedRows != 1 {
					t.Fatalf("legacy transfer state (progress, ownership, attempts, abandoned) = (%d, %d, %d, %d), want (0, 0, 0, 1)",
						progressRows, ownershipRows, attemptRows, abandonedRows)
				}
			} else {
				testutil.RequireNoError(t, mainErr, "checking legacy destination without ownership is retained")
			}
			requireState("pending")

			testutil.MustExec(t, f.pool, "aging the pending publication", `UPDATE media_asset_copies SET created_at = now() - interval '48 hours' WHERE store = 'archive' AND rel_path = $1`, key)
			testutil.RequireNoError(t, deleter.Work(ctx, jobs.DeleteReconcileArgs{}), "reclaiming after publication grace")
			requireState("deleted")
			if _, err := objects["archive"].Head(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
				t.Fatalf("pending object after grace: HEAD error = %v, want ErrNotFound", err)
			}
		})
	}
}
