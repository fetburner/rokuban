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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestIngestWorker_ConcurrentSameRelPathUsesTempFiles は同じ rel_path に対する 2 本の
// ingest が、canonical file を共有せず並行して転送できることを確認する。DB の
// unique INSERT だけが採用を決めるので、勝者のバイト列が丸ごと残り、敗者の
// 一時ファイルは DB エラー時の規約に従って残り、orphan 回収へ渡る。
func TestIngestWorker_ConcurrentSameRelPathUsesTempFiles(t *testing.T) {
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
		data     []byte
		recordID string
		err      error
	}
	results := make(chan result, 2)
	go func() {
		results <- result{data: tsDataA, recordID: jobA.RecordID, err: runWorkerViaJobQFake(context.Background(), wA, jobA)}
	}()
	go func() {
		results <- result{data: tsDataB, recordID: jobB.RecordID, err: runWorkerViaJobQFake(context.Background(), wB, jobB)}
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

	// winnerData の由来: DB でコミットが成功した側 (Work が nil を返した側) の
	// バイト列。この 1 行が「INSERT が rename より先」という commit の核心の
	// 不変条件を固定している --- rename + 親 fsync を CreateMediaAsset の前へ
	// 移す変異は、canonical file の中身を「A か B のどちらか」としか見ない
	// 旧アサーションでは検出できず、worker パッケージ全体が緑のまま通っていた
	// (変異で確認済み)。DB の勝者と canonical file の中身が食い違えば、それは
	// 敗者が勝者の canonical file を上書きしてから自分の INSERT で失敗した
	// (順序が逆転した) ことを意味する。
	var success, failure int
	var winnerData []byte
	var winnerRecordID, loserRecordID string
	var loserData []byte
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err == nil {
				success++
				winnerData = result.data
				winnerRecordID = result.recordID
			} else {
				failure++
				loserData = result.data
				loserRecordID = result.recordID
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent ingest did not finish")
		}
	}
	if success != 1 || failure != 1 {
		t.Fatalf("concurrent Work results = success %d, failure %d; want one of each", success, failure)
	}
	if got := deleteCalls.Load(); got != 1 {
		t.Errorf("DeleteRecord calls = %d, want one for the DB winner", got)
	}

	fullPath := filepath.Join(mediaDir, "sites", "default", filepath.FromSlash(contentPath))
	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading winning canonical file: %v", err)
	}
	if !bytes.Equal(got, winnerData) {
		t.Errorf("canonical file does not match the DB winner: got %d bytes", len(got))
	}
	winnerTempPath := ingestTempFilePath(filepath.Dir(fullPath), "default", winnerRecordID)
	if _, err := os.Stat(winnerTempPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("winner ingest temp still exists: stat error = %v, want not exist", err)
	}
	loserTempPath := ingestTempFilePath(filepath.Dir(fullPath), "default", loserRecordID)
	loserTemp, err := os.ReadFile(loserTempPath)
	if err != nil {
		t.Fatalf("reading preserved loser ingest temp: %v", err)
	}
	if !bytes.Equal(loserTemp, loserData) {
		t.Errorf("preserved loser ingest temp differs from its transferred bytes")
	}

	var liveAssets int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM media_asset_copies WHERE rel_path = $1 AND state <> 'deleted'",
		"sites/default/"+contentPath,
	).Scan(&liveAssets); err != nil {
		t.Fatalf("counting live assets for concurrent rel_path: %v", err)
	}
	if liveAssets != 1 {
		t.Errorf("live assets for concurrent rel_path = %d, want 1", liveAssets)
	}
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

// TestIngestWorker_CommitHoldsRelPathFileLockThroughRename は実際の commit 経路が
// rename 中も filesystem lock を保持することを確認する。lock の取得を外した変異は、
// rename 停止中に同じ lock file を取得できてしまうため検出できる。
func TestIngestWorker_CommitHoldsRelPathFileLockThroughRename(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	relPath := "sites/default/lock/commit.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-commit-lock")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating commit directory: %v", err)
	}
	if err := os.WriteFile(tempPath, []byte("committed bytes"), 0o644); err != nil {
		t.Fatalf("writing commit temp: %v", err)
	}
	seedIngestTempForCommit(t, pool, recordingID, mediaDir, tempPath)

	renameStarted := make(chan struct{})
	releaseRename := make(chan struct{})
	var releaseRenameOnce sync.Once
	release := func() { releaseRenameOnce.Do(func() { close(releaseRename) }) }
	originalRename := linkIngestFile
	t.Cleanup(func() { linkIngestFile = originalRename })
	linkIngestFile = func(src, dst string) error {
		close(renameStarted)
		<-releaseRename
		return originalRename(src, dst)
	}

	w := &IngestWorker{Publisher: pgstoretest.Media(pool), MediaDir: mediaDir, Precheck: pgstoretest.Media(pool)}
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- w.commit(context.Background(), recordingID, relPath, tempPath, fullPath,
			int64(len("committed bytes")), nil)
	}()

	waitCommit := func() error {
		t.Helper()
		select {
		case err := <-commitDone:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("commit did not finish after the rename gate was released")
			return nil
		}
	}
	select {
	case <-renameStarted:
	case err := <-commitDone:
		release()
		t.Fatalf("commit returned before reaching the rename hook: %v", err)
	case <-time.After(5 * time.Second):
		release()
		err := waitCommit()
		t.Fatalf("commit did not reach the rename hook (result: %v)", err)
	}
	fileLock, acquired, err := tryLockMediaRelPathFile(mediaDir, relPath)
	if err != nil {
		release()
		_ = waitCommit()
		t.Fatalf("trying rel_path file lock during commit: %v", err)
	}
	if acquired {
		_ = fileLock.Close()
		release()
		_ = waitCommit()
		t.Fatal("rel_path file lock was available while commit was renaming canonical")
	}
	release()
	if err := waitCommit(); err != nil {
		t.Fatalf("commit() error: %v", err)
	}
	if got, err := os.ReadFile(fullPath); err != nil || string(got) != "committed bytes" {
		t.Fatalf("committed canonical = %q, %v; want committed bytes", got, err)
	}
}

func TestIngestWorker_CompletedCommitsLeaveNoPerRelPathLockFiles(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	w := &IngestWorker{Publisher: pgstoretest.Media(pool), MediaDir: mediaDir, Precheck: pgstoretest.Media(pool)}
	const commits = 5
	for i := 0; i < commits; i++ {
		recordingID := dbtest.Recording(t, pool)
		relPath := fmt.Sprintf("sites/default/lock-lifecycle/commit-%d.m2ts", i)
		fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
		tempPath := filepath.Join(filepath.Dir(fullPath), fmt.Sprintf(".rokuban-ingest-lock-lifecycle-%d", i))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("creating commit directory: %v", err)
		}
		content := []byte(fmt.Sprintf("committed payload %d", i))
		if err := os.WriteFile(tempPath, content, 0o644); err != nil {
			t.Fatalf("writing commit temp: %v", err)
		}
		seedIngestTempForCommit(t, pool, recordingID, mediaDir, tempPath)
		if err := w.commit(context.Background(), recordingID, relPath, tempPath, fullPath,
			int64(len(content)), nil); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if _, err := os.Stat(fullPath); err != nil {
			t.Fatalf("canonical file after commit %d: %v", i, err)
		}
		lockPath := mediaRelPathLockPath(mediaDir, relPath)
		if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("per-rel_path lock after commit %d: stat error = %v, want not exist", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(mediaDir, ".rokuban-locks"))
	if err != nil {
		t.Fatalf("reading lock directory after %d commits: %v", commits, err)
	}
	if len(entries) != 1 || entries[0].Name() != mediaRelPathLockGateFile {
		t.Errorf("lock directory entries after %d successful commits = %v, want only persistent gate %q", commits, entryNames(entries), mediaRelPathLockGateFile)
	}
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
			int64(len(content)), nil)
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

	// The path lock prevents cleanup from deleting the file while the transaction is unresolved.
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
			fullPath := filepath.Join(mediaDir, "sites", "default", filepath.FromSlash(contentPath))
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
			tempPath := ingestTempFilePath(filepath.Dir(fullPath), "default", recordID)
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
