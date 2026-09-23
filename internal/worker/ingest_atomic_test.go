package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/tsstat"
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
	recordingIDA := insertTestRecordingForSite(t, pool, "default", 731101)
	recordingIDB := insertTestRecordingForSite(t, pool, "default", 731102)
	insertTestRecordSyncForSite(t, pool, "default", recordingIDA, "rec-concurrent-a", 327361024000731101)
	insertTestRecordSyncForSite(t, pool, "default", recordingIDB, "rec-concurrent-b", 327361024000731102)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var deleteCalls atomic.Int32
	srvA := newConcurrentIngestServer(t, tsDataA, contentPath, started, release, &deleteCalls)
	srvB := newConcurrentIngestServer(t, tsDataB, contentPath, started, release, &deleteCalls)

	wA := &IngestWorker{
		MirakcClients: singleSiteClients("default", mirakc.NewClient(srvA.URL, nil)),
		Pool:          pool,
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}
	wB := &IngestWorker{
		MirakcClients: singleSiteClients("default", mirakc.NewClient(srvB.URL, nil)),
		Pool:          pool,
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}
	jobA := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{ID: 731101},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-concurrent-a"},
	}
	jobB := &river.Job[IngestJobArgs]{
		JobRow: &rivertype.JobRow{ID: 731102},
		Args:   IngestJobArgs{Site: "default", RecordID: "rec-concurrent-b"},
	}

	type result struct {
		data     []byte
		recordID string
		err      error
	}
	results := make(chan result, 2)
	go func() {
		results <- result{data: tsDataA, recordID: jobA.Args.RecordID, err: wA.Work(context.Background(), jobA)}
	}()
	go func() {
		results <- result{data: tsDataB, recordID: jobB.Args.RecordID, err: wB.Work(context.Background(), jobB)}
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
		"SELECT count(*) FROM media_assets WHERE rel_path = $1 AND state <> 'deleted'",
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
			writeRecordStream(w, r, tsData)
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
			record := mirakc.Record{
				Recording: mirakc.RecordInfo{Status: "finished", Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
				Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath},
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
// rename 中も filesystem lock を保持することを確認する。DB advisory lock だけを
// 残した変異でも、ここで同じ lock file を取得できてしまうため検出できる。
func TestIngestWorker_CommitHoldsRelPathFileLockThroughRename(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	relPath := "sites/default/lock/commit.m2ts"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-commit-lock")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating commit directory: %v", err)
	}
	if err := os.WriteFile(tempPath, []byte("committed bytes"), 0o644); err != nil {
		t.Fatalf("writing commit temp: %v", err)
	}

	renameStarted := make(chan struct{})
	releaseRename := make(chan struct{})
	var releaseRenameOnce sync.Once
	release := func() { releaseRenameOnce.Do(func() { close(releaseRename) }) }
	originalRename := renameIngestFile
	t.Cleanup(func() { renameIngestFile = originalRename })
	renameIngestFile = func(src, dst string) error {
		close(renameStarted)
		<-releaseRename
		return originalRename(src, dst)
	}

	w := &IngestWorker{Pool: pool}
	commitDone := make(chan error, 1)
	go func() {
		counter := tsstat.NewCounter(io.Discard)
		commitDone <- w.commit(context.Background(), recordingID, relPath, tempPath, fullPath,
			int64(len("committed bytes")), counter)
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
	fileLock, acquired, err := tryLockMediaRelPathFile(fullPath, relPath)
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

// TestIngestWorker_CommitAndCanonicalOrphanCleanupStayLockedAfterDBDisconnect は、
// DB transaction の予約後、canonical file の公開前に DB セッションが失われても、
// filesystem lock が cleanup を待たせることを確認する。DB advisory lock だけの
// 実装では orphan_files 行を先に消してから commit の rename が走るため、公開に
// 失敗した canonical を次回の orphan cleanup が追跡できなくなる。
func TestIngestWorker_CommitAndCanonicalOrphanCleanupStayLockedAfterDBDisconnect(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	ctx := context.Background()
	const relPath = "sites/default/lock/db-disconnect.m2ts"
	const content = "canonical bytes after the lost database session"
	fullPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	tempPath := filepath.Join(filepath.Dir(fullPath), ".rokuban-ingest-db-disconnect")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating commit directory: %v", err)
	}
	if err := os.WriteFile(tempPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing commit temp: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO orphan_files (rel_path, first_seen) VALUES ($1, now())", relPath); err != nil {
		t.Fatalf("seeding orphan record: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM orphan_files WHERE rel_path = $1", relPath)
	})
	recordingID := insertTestRecording(t, pool)

	publicationStarted := make(chan struct{})
	releasePublication := make(chan struct{})
	var releasePublicationOnce sync.Once
	release := func() { releasePublicationOnce.Do(func() { close(releasePublication) }) }
	originalPublication := beforeIngestFilePublication
	t.Cleanup(func() {
		release()
		beforeIngestFilePublication = originalPublication
	})
	beforeIngestFilePublication = func(_ context.Context, tx pgx5.Tx) error {
		// Close the actual transaction connection before rename. This releases the
		// DB advisory lock while the commit goroutine still owns the filesystem lock.
		// The commit will fail after the hook is released, leaving the canonical as
		// an orphan for the final cleanup below.
		if err := tx.Conn().Close(context.Background()); err != nil {
			return fmt.Errorf("closing transaction connection in test: %w", err)
		}
		close(publicationStarted)
		<-releasePublication
		return nil
	}

	w := &IngestWorker{Pool: pool}
	commitDone := make(chan error, 1)
	go func() {
		counter := tsstat.NewCounter(io.Discard)
		commitDone <- w.commit(ctx, recordingID, relPath, tempPath, fullPath,
			int64(len(content)), counter)
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

	// The DB session is already gone, but rename has not started. Cleanup must
	// still defer because commit holds the rel_path filesystem lock.
	q := sqlcgen.New(pool)
	cleanup := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
	cleanup.deleteOrphanFile(q, relPath, 0)
	var orphanCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&orphanCount); err != nil {
		t.Fatalf("querying orphan record while commit is paused: %v", err)
	}
	if orphanCount != 1 {
		t.Fatalf("orphan record count while commit is paused = %d, want 1", orphanCount)
	}
	if _, err := os.Stat(fullPath); !errors.Is(err, os.ErrNotExist) {
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
		t.Fatal("commit succeeded after its database session was closed")
	}
	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading canonical file after commit failure: %v", err)
	}
	if string(got) != content {
		t.Errorf("canonical file after commit failure = %q, want %q", got, content)
	}

	// Once commit has released the filesystem lock, the same cleanup path may
	// remove the now-unpublished canonical and its orphan_files row together.
	cleanup.deleteOrphanFile(q, relPath, 0)
	if _, err := os.Stat(fullPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canonical file after deferred cleanup: stat error = %v, want not exist", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM orphan_files WHERE rel_path = $1", relPath).Scan(&orphanCount); err != nil {
		t.Fatalf("querying orphan record after deferred cleanup: %v", err)
	}
	if orphanCount != 0 {
		t.Errorf("orphan record count after deferred cleanup = %d, want 0", orphanCount)
	}
}

// TestIngestWorker_CommitStopPointsKeepMirakcRecord は、公開プロトコルの 3 つの
// 停止点を検証する。rename 前の失敗では temp を残し、rename 後の fsync/DB
// commit 失敗では canonical が孤児として残る。どの場合も DB 行と mirakc の
// 削除は公開点（DB commit）まで発生せず、同じジョブを再試行できる。
func TestIngestWorker_CommitStopPointsKeepMirakcRecord(t *testing.T) {
	tests := []struct {
		name             string
		canonicalOnError bool
		tempOnError      bool
		installFailure   func(t *testing.T)
	}{
		{
			name:        "rename before publication",
			tempOnError: true,
			installFailure: func(t *testing.T) {
				renameIngestFile = func(string, string) error { return errors.New("injected rename failure") }
			},
		},
		{
			name:             "parent fsync after rename",
			canonicalOnError: true,
			installFailure: func(t *testing.T) {
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
			name:             "database commit after rename",
			canonicalOnError: true,
			installFailure: func(t *testing.T) {
				commitIngestTransaction = func(context.Context, pgx5.Tx) error {
					return errors.New("injected database commit failure")
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
			recordingID := insertTestRecordingForSite(t, pool, "default", int32(731200+i))
			recordID := fmt.Sprintf("rec-stop-point-%d", i)
			insertTestRecordSyncForSite(t, pool, "default", recordingID, recordID, int64(327361024000731200+i))

			var deleteCalls atomic.Int32
			srv := newInstrumentedIngestServer(t, tsData, contentPath, func() { deleteCalls.Add(1) })
			w := &IngestWorker{
				MirakcClients: singleSiteClients("default", mirakc.NewClient(srv.URL, nil)),
				Pool:          pool,
				MediaDir:      mediaDir,
				StallTimeout:  5 * time.Second,
			}
			job := &river.Job[IngestJobArgs]{
				JobRow: &rivertype.JobRow{ID: int64(731200 + i)},
				Args:   IngestJobArgs{Site: "default", RecordID: recordID},
			}

			originalRename := renameIngestFile
			originalSync := syncIngestParentDir
			originalCommit := commitIngestTransaction
			restoreHooks := func() {
				renameIngestFile = originalRename
				syncIngestParentDir = originalSync
				commitIngestTransaction = originalCommit
			}
			t.Cleanup(restoreHooks)
			tt.installFailure(t)

			if err := w.Work(context.Background(), job); err == nil {
				t.Fatal("first Work() returned nil despite the injected stop point")
			}
			assertIngestAssetCount(t, pool, recordingID, 0)
			if got := deleteCalls.Load(); got != 0 {
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
			if tt.tempOnError {
				got, err := os.ReadFile(tempPath)
				if err != nil {
					t.Fatalf("reading preserved ingest temp: %v", err)
				}
				if !bytes.Equal(got, tsData) {
					t.Errorf("preserved ingest temp differs from transferred bytes")
				}
			} else {
				assertNoIngestTempFiles(t, mediaDir)
			}

			restoreHooks()
			if err := w.Work(context.Background(), job); err != nil {
				t.Fatalf("retry Work() error: %v", err)
			}
			assertIngestAssetCount(t, pool, recordingID, 1)
			if got := deleteCalls.Load(); got != 1 {
				t.Errorf("DeleteRecord calls after retry = %d, want 1", got)
			}
			got, err := os.ReadFile(fullPath)
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
