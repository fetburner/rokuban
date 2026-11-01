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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

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
			// temp を捨てたので、それを指す進捗行も残さない。
			if _, ok := readIngestProgress(t, pool, recordingID); ok {
				t.Fatalf("progress row remained after %s discarded the temp", mode)
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

// temp の作成直後・進捗行を書く前に止まっても、旧 HLS pointer と byte generation は、
// 新 writer が追いつく前後で維持される。
func TestIngestWorker_TempCreatedBeforeProgressKeepsOldGeneration(t *testing.T) {
	setFastIngestFollow(t)
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	data := makeTSData(20)
	hash := sha256Hex(data)
	const recordID = "rec-ingest-create-crash"
	const contentPath = "test/create-crash.m2ts"
	fake := newMirakcRecordFake(t, mirakc.Record{
		ID:        recordID,
		Recording: mirakc.RecordInfo{Status: "finished", Options: mirakc.Options{ContentPath: strPtr(contentPath)}},
		Content:   mirakc.ContentInfo{Path: "/recording/" + contentPath, Sha256: &hash},
	}, data)
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)
	mediaDir := t.TempDir()
	worker := &IngestWorker{
		Publisher:     pgstoretest.Media(pool),
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		RecordTarget:  pgstoretest.Recordings(pool),
		Precheck:      pgstoretest.Media(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}
	worker.CMDetectionStore = pgstoretest.Analysis(pool)

	oldGeneration, err := pgstoretest.Media(pool).NextIngestTempGeneration(context.Background())
	if err != nil {
		t.Fatalf("allocating old generation: %v", err)
	}
	dir := filepath.Join(mediaDir, "sites", "default", "test")
	oldTempPath := filepath.Join(dir, ".rokuban-ingest-default-rec-ingest-create-crash")
	oldTempRelPath, err := ingestTempRelPath(mediaDir, oldTempPath)
	if err != nil {
		t.Fatal(err)
	}
	seedIngestTempProgress(t, pool, recordingID, oldTempRelPath, oldGeneration)
	if _, err := pool.Exec(context.Background(), `UPDATE recording_ingest_progress SET written_bytes = 10 WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatalf("setting old visible offset: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldTempPath, []byte("old-prefix"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousHook := afterIngestTempCreate
	afterIngestTempCreate = func() error { return errors.New("simulated process stop after temp creation") }
	defer func() { afterIngestTempCreate = previousHook }()
	err = runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID})
	if err == nil || !strings.Contains(err.Error(), "simulated process stop") {
		t.Fatalf("first Work() error = %v, want simulated process stop", err)
	}
	var visiblePath string
	var visibleBytes, visibleGeneration int64
	if err := pool.QueryRow(context.Background(), `SELECT temp_rel_path, written_bytes, temp_generation FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&visiblePath, &visibleBytes, &visibleGeneration); err != nil {
		t.Fatalf("reading old visible progress after creating replacement: %v", err)
	}
	if visiblePath != oldTempRelPath || visibleBytes != 10 || visibleGeneration != oldGeneration {
		t.Fatalf("visible progress after creating replacement = (%q, %d, %d), want old pointer at 10 bytes and generation %d", visiblePath, visibleBytes, visibleGeneration, oldGeneration)
	}
	tempPath := ingestTempWriterFilePath(dir, "default", recordID, 1)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != 0 {
		t.Fatalf("replacement temp after the stop: info=%v err=%v, want an empty file", info, err)
	}

	afterIngestTempCreate = previousHook
	if err := runWorkerViaJobQFake(context.Background(), worker, IngestJobArgs{Site: "default", RecordID: recordID}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var committed int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(ingest_generation, 0) FROM media_assets WHERE recording_id = $1 AND kind = 'original'`,
		recordingID).Scan(&committed); err != nil {
		t.Fatalf("reading committed generation: %v", err)
	}
	if committed != oldGeneration {
		t.Fatalf("committed generation = %d, want the unchanged byte generation %d", committed, oldGeneration)
	}
}

type ingestTempGenerationCapture struct {
	store.MediaPublish
	mu          sync.Mutex
	generations []int64
}

func (p *ingestTempGenerationCapture) UpdateIngestProgress(ctx context.Context, progress store.IngestProgress, owner uuid.UUID, writerGeneration int64, ttl time.Duration) error {
	p.mu.Lock()
	p.generations = append(p.generations, progress.TempGeneration)
	p.mu.Unlock()
	return p.MediaPublish.UpdateIngestProgress(ctx, progress, owner, writerGeneration, ttl)
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
INSERT INTO recording_ingest_progress (recording_id, written_bytes, temp_rel_path, temp_store, temp_generation)
VALUES ($1, 0, $2, $3, $4)`, recordingID, tempRelPath, pgstoretest.TestStore, generation); err != nil {
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
