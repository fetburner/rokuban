package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestWorker_ConcurrentSameFilenameUsesDistinctPaths は、同じファイル名の
// 原本を並行 ingest しても、それぞれ別の canonical path と全バイトを保持することを確かめる。
func TestIngestWorker_ConcurrentSameFilenameUsesDistinctPaths(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	const contentPath = "shared/concurrent.m2ts"
	tsDataA := makeTSDataFill(0x21)
	tsDataB := makeTSDataFill(0x22)
	recordingIDA := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 731101
	})
	recordingIDB := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 731102
	})
	insertTestRecordSyncForSite(t, pool, "default", recordingIDA, "rec-concurrent-a", 327361024000731101)
	insertTestRecordSyncForSite(t, pool, "default", recordingIDB, "rec-concurrent-b", 327361024000731102)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var deleteCalls atomic.Int32
	srvA := newConcurrentIngestServer(t, tsDataA, contentPath, started, release, &deleteCalls)
	srvB := newConcurrentIngestServer(t, tsDataB, contentPath, started, release, &deleteCalls)

	wA := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("default", mirakc.NewClient(srvA.URL, nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	wB := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("default", mirakc.NewClient(srvB.URL, nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstoretest.Media(pool),
	}
	wA.CMDetectionStore = pgstoretest.Analysis(pool)
	wB.CMDetectionStore = pgstoretest.Analysis(pool)
	jobA := IngestJobArgs{Site: "default", RecordID: "rec-concurrent-a"}
	jobB := IngestJobArgs{Site: "default", RecordID: "rec-concurrent-b"}

	type result struct {
		data        []byte
		recordID    string
		recordingID int64
		err         error
	}
	results := make(chan result, 2)
	go func() {
		results <- result{data: tsDataA, recordID: jobA.RecordID, recordingID: recordingIDA, err: runWorkerViaJobQFake(context.Background(), wA, jobA)}
	}()
	go func() {
		results <- result{data: tsDataB, recordID: jobB.RecordID, recordingID: recordingIDB, err: runWorkerViaJobQFake(context.Background(), wB, jobB)}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("did not observe both concurrent stream requests")
		}
	}
	close(release)

	resultsByRecording := make(map[int64]result, 2)
	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Errorf("concurrent Work for %s: %v", got.recordID, got.err)
			}
			resultsByRecording[got.recordingID] = got
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent ingest did not finish")
		}
	}
	if len(resultsByRecording) != 2 {
		t.Fatalf("concurrent Work results = %d recording(s), want 2", len(resultsByRecording))
	}
	if got := deleteCalls.Load(); got != 2 {
		t.Errorf("DeleteRecord calls = %d, want one for each committed recording", got)
	}

	paths := make(map[int64]string, 2)
	for _, recordingID := range []int64{recordingIDA, recordingIDB} {
		result := resultsByRecording[recordingID]
		relPath := ingestTestCommittedRelPath(t, pool, recordingID)
		assertIngestRelPath(t, relPath, "default", "concurrent.m2ts")
		paths[recordingID] = relPath
		got, err := os.ReadFile(filepath.Join(mediaDir, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatalf("reading committed file for %s: %v", result.recordID, err)
		}
		if !bytes.Equal(got, result.data) {
			t.Errorf("committed file for %s differs from its input bytes", result.recordID)
		}
	}
	if paths[recordingIDA] == paths[recordingIDB] {
		t.Fatalf("concurrent recordings share rel_path %q", paths[recordingIDA])
	}
	assertNoIngestTempFiles(t, mediaDir)
}

func newConcurrentIngestServer(t *testing.T, tsData []byte, contentPath string, started chan<- struct{}, release <-chan struct{}, deleteCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	// 追従ループは 1 回の Work で /stream を複数回叩く（差分 + finished 後の
	// drain）。ゲートは最初の 1 回だけに掛ける --- 毎回掛けると release 前に
	// 2 回目の要求が自身を待ってデッドロックする。
	var gate sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
			gate.Do(func() {
				started <- struct{}{}
				<-release
			})
			mirakctest.WriteRecordStream(w, r, tsData)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "finished", Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: strPtr(sha256Hex(tsData))},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(record)
		case r.Method == http.MethodDelete:
			deleteCalls.Add(1)
			result := mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestIngestWorker_CanonicalOrphanSurvivesPublishCallbackFailure は、rename 後に
// publish callback が失敗しても canonical を孤児回収が消さず、orphan_files の
// 追跡記録も保持することを確認する。
func TestIngestWorker_CanonicalOrphanSurvivesPublishCallbackFailure(t *testing.T) {
	runCanonicalOrphanSurvives(t, "sites/default/lock/callback-failure.m2ts", true,
		func(t *testing.T, _ *pgxpool.Pool, paused func(), wait <-chan struct{}) {
			original := afterIngestFilePublication
			t.Cleanup(func() { afterIngestFilePublication = original })
			afterIngestFilePublication = func() error {
				paused()
				<-wait
				return errors.New("injected publication callback failure")
			}
		})
}

// TestIngestWorker_CanonicalOrphanSurvivesDBDisconnect は、rename の前に DB セッションが
// 失われると、publish は成功しても commit が失敗する。このとき canonical を孤児回収が消さず、
// orphan_files の追跡記録も保持することを確認する。
func TestIngestWorker_CanonicalOrphanSurvivesDBDisconnect(t *testing.T) {
	runCanonicalOrphanSurvives(t, "sites/default/lock/db-disconnect.m2ts", false,
		func(t *testing.T, pool *pgxpool.Pool, paused func(), wait <-chan struct{}) {
			original := beforeIngestFilePublication
			t.Cleanup(func() { beforeIngestFilePublication = original })
			beforeIngestFilePublication = func() error {
				testutil.TerminateOpenTransactions(t, pool)
				paused()
				<-wait
				return nil
			}
		})
}

// runCanonicalOrphanSurvives は commit の失敗で残った canonical が孤児として扱われる経路を検査する。
// install は公開関数の途中に止める hook を入れ、止まったら paused を呼び、wait が閉じるまで待つ。
// renamedWhilePaused は止まっている間に canonical が既にあるかを表す。
func runCanonicalOrphanSurvives(
	t *testing.T,
	relPath string,
	renamedWhilePaused bool,
	install func(t *testing.T, pool *pgxpool.Pool, paused func(), wait <-chan struct{}),
) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	ctx := context.Background()
	const content = "canonical bytes after the failed commit"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-db-disconnect")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating commit directory: %v", err)
	}
	if err := os.WriteFile(tempPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing commit temp: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO orphan_files (store, rel_path, first_seen) VALUES ('test-store', $1, now())", relPath); err != nil {
		t.Fatalf("seeding orphan record: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM orphan_files WHERE rel_path = $1", relPath)
	})
	recordingID := dbtest.Recording(t, pool)
	seedIngestTempForCommit(t, pool, recordingID, mediaDir, tempPath)

	publicationStarted := make(chan struct{})
	releasePublication := make(chan struct{})
	var releasePublicationOnce sync.Once
	release := func() { releasePublicationOnce.Do(func() { close(releasePublication) }) }
	t.Cleanup(release)
	install(t, pool, func() { close(publicationStarted) }, releasePublication)

	w := &IngestWorker{Publisher: pgstoretest.Media(pool), MediaDir: mediaDir, Precheck: pgstoretest.Media(pool)}
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- w.commit(ctx, recordingID, relPath, tempPath, fullPath,
			int64(len(content)), nil, uuid.Nil, 0)
	}()

	waitCommit := func() error {
		t.Helper()
		select {
		case err := <-commitDone:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("commit did not finish after the publication gate was released")
			return nil
		}
	}
	select {
	case <-publicationStarted:
	case err := <-commitDone:
		release()
		t.Fatalf("commit returned before the publication hook: %v", err)
	case <-time.After(5 * time.Second):
		release()
		err := waitCommit()
		t.Fatalf("commit did not reach the publication hook (result: %v)", err)
	}

	// Canonical orphan cleanup is report-only, including while the publication transaction is unresolved.
	cleanup := &DeleteReconcileWorker{MediaDelete: pgstoretest.Media(pool), Storage: pgstoretest.Media(pool), RecordingPurge: pgstoretest.Recordings(pool), Breakers: pgstoretest.Breakers(pool), MediaDir: mediaDir, StorageObjects: testStorageObjects(t, "", mediaDir),
		DerivedObjects: testFilesystemObjectStore(t, mediaDir)}
	cleanup.deleteOrphanFile(relPath, 0)
	var orphanCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&orphanCount); err != nil {
		t.Fatalf("querying orphan record while commit is paused: %v", err)
	}
	if orphanCount != 1 {
		t.Fatalf("orphan record count while commit is paused = %d, want 1", orphanCount)
	}
	if renamedWhilePaused {
		if got, err := os.ReadFile(fullPath); err != nil || string(got) != content {
			t.Fatalf("canonical file while publication is paused = %q, err %v; want %q", got, err, content)
		}
	} else if _, err := os.Stat(fullPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical file before publication: stat error = %v, want not exist", err)
	}
	select {
	case err := <-commitDone:
		release()
		t.Fatalf("commit completed before publication gate was released: %v", err)
	default:
	}

	release()
	if err := waitCommit(); err == nil {
		t.Fatal("commit succeeded although its publication or database session failed")
	}
	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading canonical file after commit failure: %v", err)
	}
	if string(got) != content {
		t.Errorf("canonical file after commit failure = %q, want %q", got, content)
	}

	// A later cleanup pass still cannot infer publication state from this canonical
	// name, so it must preserve both the file and its orphan record.
	cleanup.deleteOrphanFile(relPath, 0)
	if _, err := os.Stat(fullPath); err != nil {
		t.Errorf("canonical file after report-only cleanup: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&orphanCount); err != nil {
		t.Fatalf("querying orphan record after deferred cleanup: %v", err)
	}
	if orphanCount != 1 {
		t.Errorf("orphan record count after report-only cleanup = %d, want 1", orphanCount)
	}
}

// TestIngestWorker_CommitStopPointsKeepMirakcRecord は、公開プロトコルの 4 つの
// 停止点を検証する。どの停止点でも temp を残す。link 後の fsync / DB callback / commit 失敗では
// canonical が孤児として残り、temp と同じ inode になる。DB 行と mirakc の削除は公開点
// （DB commit）まで発生せず、同じジョブを再試行できる。
func TestIngestWorker_CommitStopPointsKeepMirakcRecord(t *testing.T) {
	setFastIngestFollow(t)
	tests := []struct {
		name             string
		canonicalOnError bool
		installFailure   func(t *testing.T, pool *pgxpool.Pool)
	}{
		{
			name: "link before publication",
			installFailure: func(t *testing.T, pool *pgxpool.Pool) {
				linkIngestFile = func(string, string) error { return errors.New("injected link failure") }
			},
		},
		{
			name:             "parent fsync after link",
			canonicalOnError: true,
			installFailure: func(t *testing.T, pool *pgxpool.Pool) {
				original := syncIngestParentDir
				syncIngestParentDir = func(path string) error {
					if err := original(path); err != nil {
						return err
					}
					return errors.New("injected parent directory fsync failure")
				}
			},
		},
		{
			name:             "database commit after link",
			canonicalOnError: true,
			installFailure: func(t *testing.T, pool *pgxpool.Pool) {
				original := afterIngestFilePublication
				afterIngestFilePublication = func() error {
					if err := original(); err != nil {
						return err
					}
					testutil.TerminateOpenTransactions(t, pool)
					return nil
				}
			},
		},
		{
			name:             "publication callback after link",
			canonicalOnError: true,
			installFailure: func(t *testing.T, pool *pgxpool.Pool) {
				original := afterIngestFilePublication
				afterIngestFilePublication = func() error {
					if err := original(); err != nil {
						return err
					}
					return errors.New("injected publication callback failure")
				}
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := setupTestPool(t)
			mediaDir := t.TempDir()
			contentPath := fmt.Sprintf("stop-points/%s.m2ts", strings.ReplaceAll(tt.name, " ", "-"))
			tsData := makeTSDataFill(byte(0x30 + i))
			recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
				fixtureParams.Site, fixtureParams.EventID = "default", int32(731200+i)
			})
			recordID := fmt.Sprintf("rec-stop-point-%d", i)
			insertTestRecordSyncForSite(t, pool, "default", recordingID, recordID, int64(327361024000731200+i))

			fake := newFullTransferFake(t, recordID, tsData, contentPath)
			w := &IngestWorker{
				MirakcClients: singleSiteClients("default", mirakc.NewClient(fake.URL(), nil)),
				Publisher:     pgstoretest.Media(pool),
				RecordTarget:  pgstoretest.Recordings(pool),
				MediaDir:      mediaDir,
				StallTimeout:  5 * time.Second,
				Precheck:      pgstoretest.Media(pool),
			}
			w.CMDetectionStore = pgstoretest.Analysis(pool)
			job := IngestJobArgs{Site: "default", RecordID: recordID}

			originalRename := linkIngestFile
			originalSync := syncIngestParentDir
			originalAfter := afterIngestFilePublication
			restoreHooks := func() {
				linkIngestFile = originalRename
				syncIngestParentDir = originalSync
				afterIngestFilePublication = originalAfter
			}
			t.Cleanup(restoreHooks)
			tt.installFailure(t, pool)

			if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
				t.Fatal("first Work() returned nil despite the injected stop point")
			}
			assertIngestAssetCount(t, pool, recordingID, 0)
			if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 0 {
				t.Errorf("DeleteRecord calls after failed publication = %d, want 0", got)
			}
			canonicalRelPath := ingestTestReservedRelPath(t, pool, recordingID)
			fullPath := filepath.Join(mediaDir, filepath.FromSlash(canonicalRelPath))
			if tt.canonicalOnError {
				got, err := os.ReadFile(fullPath)
				if err != nil {
					t.Fatalf("reading orphan canonical file: %v", err)
				}
				if !bytes.Equal(got, tsData) {
					t.Errorf("orphan canonical file differs from transferred bytes")
				}
			} else if _, err := os.Stat(fullPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("canonical file stat error = %v, want not exist", err)
			}
			tempPath := ingestTestTempFilePath(t, pool, mediaDir, recordingID, "default", recordID)
			got, err := os.ReadFile(tempPath)
			if err != nil {
				t.Fatalf("reading preserved ingest temp: %v", err)
			}
			if !bytes.Equal(got, tsData) {
				t.Errorf("preserved ingest temp differs from transferred bytes")
			}
			if tt.canonicalOnError && !fsink.SameFile(tempPath, fullPath) {
				t.Errorf("preserved ingest temp is not linked to the orphan canonical file")
			}

			restoreHooks()
			if err := runWorkerViaJobQFake(context.Background(), w, job); err != nil {
				t.Fatalf("retry Work() error: %v", err)
			}
			assertIngestAssetCount(t, pool, recordingID, 1)
			if got := fake.CallCount(http.MethodDelete, "/api/recording/records/"+recordID); got != 1 {
				t.Errorf("DeleteRecord calls after retry = %d, want 1", got)
			}
			got, err = os.ReadFile(fullPath)
			if err != nil {
				t.Fatalf("reading retried canonical file: %v", err)
			}
			if !bytes.Equal(got, tsData) {
				t.Errorf("retried canonical file differs from transferred bytes")
			}
			assertNoIngestTempFiles(t, mediaDir)
		})
	}
}

func assertIngestAssetCount(t *testing.T, pool *pgxpool.Pool, recordingID int64, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_assets WHERE recording_id = $1 AND kind = 'original'",
		recordingID,
	).Scan(&got); err != nil {
		t.Fatalf("counting original media_assets for recording %d: %v", recordingID, err)
	}
	if got != want {
		t.Errorf("original media_assets for recording %d = %d, want %d", recordingID, got, want)
	}
}

func assertNoIngestTempFiles(t *testing.T, mediaDir string) {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(mediaDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && mediapath.IsIngestTempFile(entry.Name()) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking media dir for ingest temps: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("ingest temporary files remain: %v", paths)
	}
}
