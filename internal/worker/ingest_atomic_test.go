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
	"sync/atomic"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// TestIngestWorker_ConcurrentSameRelPathUsesTempFiles は同じ rel_path に対する 2 本の
// ingest が、canonical file を共有せず並行して転送できることを確認する。DB の
// unique INSERT だけが採用を決めるので、勝者のバイト列が丸ごと残り、敗者の
// 一時ファイルは消える。
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
	newBlockedServer := func(tsData []byte) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
				started <- struct{}{}
				<-release
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tsData)
			case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tsData)))
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
				record := mirakc.Record{
					Recording: mirakc.RecordInfo{Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
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
	srvA := newBlockedServer(tsDataA)
	srvB := newBlockedServer(tsDataB)

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

	type result struct{ err error }
	results := make(chan result, 2)
	go func() { results <- result{err: wA.Work(context.Background(), jobA)} }()
	go func() { results <- result{err: wB.Work(context.Background(), jobB)} }()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("did not observe both concurrent stream requests")
		}
	}
	close(release)

	var success, failure int
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err == nil {
				success++
			} else {
				failure++
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
	if !bytes.Equal(got, tsDataA) && !bytes.Equal(got, tsDataB) {
		t.Errorf("canonical file is neither complete winner: got %d bytes", len(got))
	}
	assertNoIngestTempFiles(t, mediaDir)

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

// TestIngestWorker_CommitStopPointsKeepMirakcRecord は、公開プロトコルの 3 つの
// 停止点を検証する。rename 前の失敗では temp だけが消え、rename 後の fsync/DB
// commit 失敗では canonical が孤児として残る。どの場合も DB 行と mirakc の
// 削除は公開点（DB commit）まで発生せず、同じジョブを再試行できる。
func TestIngestWorker_CommitStopPointsKeepMirakcRecord(t *testing.T) {
	tests := []struct {
		name             string
		canonicalOnError bool
		installFailure   func(t *testing.T)
	}{
		{
			name: "rename before publication",
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
			assertNoIngestTempFiles(t, mediaDir)

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
