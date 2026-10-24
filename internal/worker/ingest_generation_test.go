package worker

import (
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
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

func TestShouldAllocateIngestTempGeneration(t *testing.T) {
	const tempRelPath = "sites/default/test/.rokuban-ingest-default-rec-1"
	state := &store.IngestTempState{TempRelPath: tempRelPath, TempGeneration: 41}
	for _, tt := range []struct {
		name     string
		created  bool
		state    *store.IngestTempState
		path     string
		wantNext bool
	}{
		{name: "replay existing temp", state: state, path: tempRelPath, wantNext: false},
		{name: "new temp replaces prior temp", created: true, state: state, path: tempRelPath, wantNext: true},
		{name: "orphan temp without progress row", path: tempRelPath, wantNext: true},
		{name: "temp path changed", state: state, path: "sites/other/temp", wantNext: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAllocateIngestTempGeneration(tt.created, tt.state, tt.path); got != tt.wantNext {
				t.Fatalf("shouldAllocateIngestTempGeneration() = %t, want %t", got, tt.wantNext)
			}
		})
	}
}

func TestIngestWorker_InvalidatedTempGetsFreshGeneration(t *testing.T) {
	for _, mode := range []string{"size mismatch", "hash mismatch", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			setFastIngestFollow(t)
			pool := setupTestPool(t)
			if pool == nil {
				return
			}

			data := makeTSData(50)
			wrongHash := sha256Hex(append(append([]byte(nil), data...), 0x01))
			goodHash := sha256Hex(data)
			badAttempt := atomic.Bool{}
			badAttempt.Store(true)
			var recordGets atomic.Int32
			const contentPath = "test/generation.m2ts"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stream"):
					mirakctest.WriteRecordStream(w, r, data)
				case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/stream"):
					length := len(data)
					if mode == "size mismatch" && badAttempt.Load() {
						length++
					}
					w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/records/"):
					status := "finished"
					if mode == "cancel" && badAttempt.Load() {
						if recordGets.Add(1) == 1 {
							status = "recording"
						} else {
							status = "canceled"
						}
					}
					hash := goodHash
					if mode == "hash mismatch" && badAttempt.Load() {
						hash = wrongHash
					}
					record := mirakc.Record{
						Recording: mirakc.RecordInfo{Status: status, Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
						Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: &hash},
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(record)
				case r.Method == http.MethodDelete:
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			const recordID = "rec-ingest-generation"
			recordingID := dbtest.Recording(t, pool)
			insertTestRecordSync(t, pool, recordingID, recordID)
			media := &ingestTempGenerationCapture{MediaPublish: pgstoretest.Media(pool)}
			mediaDir := t.TempDir()
			newWorker := func() *IngestWorker {
				worker := &IngestWorker{
					Publisher:     media,
					MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil)),
					RecordTarget:  pgstoretest.Recordings(pool),
					Precheck:      pgstoretest.Media(pool),
					MediaDir:      mediaDir,
					StallTimeout:  5 * time.Second,
				}
				worker.CMDetectionStore = pgstoretest.Analysis(pool)
				return worker
			}
			worker := newWorker()

			err := runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID})
			if err == nil {
				t.Fatal("first Work() succeeded; want temp invalidation")
			}
			if mode == "cancel" && !errors.Is(err, jobq.ErrDiscard) {
				t.Fatalf("canceled Work() error = %v, want jobq.ErrDiscard", err)
			}
			if mode != "cancel" && !strings.Contains(err.Error(), mode) {
				t.Fatalf("first Work() error = %v, want %q", err, mode)
			}
			firstGeneration := media.firstGeneration(t)
			if firstGeneration <= 0 {
				t.Fatalf("first temp generation = %d, want a positive sequence value", firstGeneration)
			}
			if mode != "cancel" {
				_, got, ok := readIngestTempGeneration(t, pool, recordingID)
				if !ok || got != firstGeneration {
					t.Fatalf("progress generation after %s = (%d, %t), want %d", mode, got, ok, firstGeneration)
				}
			}

			if mode == "size mismatch" {
				assertIngestTempCreateCrashWindow(t, worker, pool, recordingID, mediaDir, contentPath, recordID)
				worker = newWorker()
			}

			badAttempt.Store(false)
			if err := runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID}); err != nil {
				t.Fatalf("retry after %s: %v", mode, err)
			}
			var committedGeneration int64
			if err := pool.QueryRow(context.Background(),
				`SELECT COALESCE(ingest_generation, 0) FROM media_assets WHERE recording_id = $1 AND kind = 'original'`,
				recordingID).Scan(&committedGeneration); err != nil {
				t.Fatalf("reading committed ingest generation: %v", err)
			}
			if committedGeneration <= firstGeneration {
				t.Fatalf("committed generation = %d after invalidated generation %d; want a newer sequence value", committedGeneration, firstGeneration)
			}
			if _, ok := readIngestProgress(t, pool, recordingID); ok {
				t.Fatal("progress row remained after original commit")
			}
		})
	}
}

func assertIngestTempCreateCrashWindow(t *testing.T, worker *IngestWorker, pool *pgxpool.Pool, recordingID int64, mediaDir, contentPath, recordID string) {
	t.Helper()
	created := make(chan struct{})
	release := make(chan struct{})
	previousHook := afterIngestTempCreateBeforeLock
	afterIngestTempCreateBeforeLock = func() error {
		close(created)
		<-release
		return errors.New("simulated process stop after temp creation")
	}
	defer func() { afterIngestTempCreateBeforeLock = previousHook }()

	firstAttemptDone := make(chan error, 1)
	go func() {
		firstAttemptDone <- runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID})
	}()
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		close(release)
		<-firstAttemptDone
		t.Fatal("worker did not pause after creating the replacement temp")
	}

	concurrentErr := runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID})
	if !errors.Is(concurrentErr, syscall.EWOULDBLOCK) {
		close(release)
		<-firstAttemptDone
		t.Fatalf("concurrent worker error = %v, want stable temp lock busy", concurrentErr)
	}
	close(release)
	if err := <-firstAttemptDone; err == nil || !strings.Contains(err.Error(), "simulated process stop") {
		t.Fatalf("replacement temp creation error = %v, want simulated process stop", err)
	}
	if _, ok := readIngestProgress(t, pool, recordingID); ok {
		t.Fatal("stale progress row remained after replacement temp creation")
	}
	relPath, err := catalog.SiteRelPath("default", contentPath)
	if err != nil {
		t.Fatalf("building ingest rel_path: %v", err)
	}
	tempPath := ingestTempFilePath(filepath.Join(mediaDir, filepath.Dir(relPath)), "default", recordID)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != 0 {
		t.Fatalf("replacement temp after simulated process stop: info=%v err=%v, want empty file", info, err)
	}
}

type ingestTempGenerationCapture struct {
	store.MediaPublish
	mu          sync.Mutex
	generations []int64
}

func (p *ingestTempGenerationCapture) UpsertIngestProgress(ctx context.Context, progress store.IngestProgress) error {
	p.mu.Lock()
	p.generations = append(p.generations, progress.TempGeneration)
	p.mu.Unlock()
	return p.MediaPublish.UpsertIngestProgress(ctx, progress)
}

func (p *ingestTempGenerationCapture) firstGeneration(t *testing.T) int64 {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.generations) == 0 {
		t.Fatal("no ingest generation was persisted")
	}
	return p.generations[0]
}

func readIngestTempGeneration(t *testing.T, pool *pgxpool.Pool, recordingID int64) (string, int64, bool) {
	t.Helper()
	var path string
	var generation int64
	err := pool.QueryRow(context.Background(),
		`SELECT temp_rel_path, temp_generation FROM recording_ingest_progress WHERE recording_id = $1`, recordingID,
	).Scan(&path, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false
	}
	if err != nil {
		t.Fatalf("reading ingest temp generation: %v", err)
	}
	return path, generation, true
}

func seedIngestTempProgress(t *testing.T, pool *pgxpool.Pool, recordingID int64, tempRelPath string, generation int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO recording_ingest_progress (recording_id, written_bytes, temp_rel_path, temp_generation)
VALUES ($1, 0, $2, $3)`, recordingID, tempRelPath, generation); err != nil {
		t.Fatalf("seeding ingest temp progress: %v", err)
	}
}

func seedIngestTempForCommit(t *testing.T, pool *pgxpool.Pool, recordingID int64, mediaDir, tempPath string) {
	t.Helper()
	tempRelPath, err := ingestTempRelPath(mediaDir, tempPath)
	if err != nil {
		t.Fatalf("building temp_rel_path for direct commit fixture: %v", err)
	}
	seedIngestTempProgress(t, pool, recordingID, tempRelPath, 1)
}
