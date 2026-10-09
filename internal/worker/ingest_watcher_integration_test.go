package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/worker"
)

func TestRecordSavedRequeuesIngestAndCommitsLateSHA256(t *testing.T) {
	pool := testutil.SetupDB(t)
	const programID int64 = 5000000001281
	seedIngestLifecycleReservation(t, pool, programID)

	record, contentPath := ingestLifecycleRecord(programID)
	tsData := ingestLifecycleTSData(20)
	mcServer := newIngestLifecycleMirakc(t, record, tsData)
	originalLogger := slog.Default()
	var logOutput bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })
	mc := mirakc.NewClient(mcServer.server.URL, nil)
	mediaDir := t.TempDir()
	ingestWorker := &worker.IngestWorker{
		MirakcClients: map[string]*mirakc.Client{watcher.DefaultSite: mc},
		Pool:          pool,
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
	}
	client := newIngestLifecycleRiverClient(t, pool, ingestWorker)
	events, cancelSubscribe := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSubscribe()

	runCtx, cancel := context.WithCancel(context.Background())
	if err := client.Start(runCtx); err != nil {
		cancel()
		t.Fatalf("starting River client: %v", err)
	}
	watcherDone := make(chan error, 1)
	go func() {
		watcherDone <- watcher.New(watcher.DefaultSite, mc, pool, client, nil).Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-watcherDone:
		case <-time.After(5 * time.Second):
			t.Error("watcher did not stop after context cancellation")
		}
		select {
		case <-client.Stopped():
		case <-time.After(5 * time.Second):
			t.Error("River client did not stop after context cancellation")
		}
	})

	select {
	case <-mcServer.eventsReady:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not connect to the SSE endpoint")
	}
	mcServer.setEndTime(time.Now())
	mcServer.recordSaved <- struct{}{}
	first := waitForIngestLifecycleCompletion(t, events)
	if first.Job.State != rivertype.JobStateCompleted {
		t.Fatalf("first ingest state = %q, want completed while SHA-256 is null", first.Job.State)
	}
	assertIngestLifecycleWaited(t, pool, first.Job.ID, mcServer.deleteAttempts.Load(), mediaDir, record.ID, int64(len(tsData)))

	mcServer.setHash(lifecycleSHA256(tsData))
	mcServer.recordSaved <- struct{}{}
	second := waitForIngestLifecycleCompletion(t, events)
	if second.Job.ID == first.Job.ID {
		t.Fatalf("job after record-saved reused completed job ID %d", first.Job.ID)
	}
	if second.Job.State != rivertype.JobStateCompleted {
		t.Fatalf("second ingest state = %q, want completed after SHA-256 arrives", second.Job.State)
	}
	if !strings.Contains(logOutput.String(), "sha256_verification=verified") {
		t.Errorf("logs after the second job = %q, want verified SHA-256 commit", logOutput.String())
	}
	assertIngestLifecycleCommitted(t, pool, mediaDir, contentPath, tsData, mcServer.deleteAttempts.Load())
}

func seedIngestLifecycleReservation(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ('default', $1, 'SHA-256 lifecycle', now(), 3600000, 32736, 1024, 'GR', '27', 100, 'テスト局')`, programID,
	); err != nil {
		t.Fatalf("creating program snapshot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO reservations (site, program_id) VALUES ('default', $1)", programID); err != nil {
		t.Fatalf("creating reservation fixture: %v", err)
	}
}

func ingestLifecycleRecord(programID int64) (mirakc.Record, string) {
	contentPath := "test/sha256-lifecycle.m2ts"
	startAt := mirakc.Milliseconds(time.Now().Add(-time.Hour).Truncate(time.Millisecond))
	startTime := mirakc.Milliseconds(time.Now().Add(-time.Hour).Truncate(time.Millisecond))
	duration := int64(time.Hour / time.Millisecond)
	name := "SHA-256 lifecycle"
	record := mirakc.Record{
		ID: "record-sha256-lifecycle",
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
		Service: mirakc.Service{
			Name:    "テスト局",
			Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
		Tags: []string{mirakc.ProgramTag(programID)},
		Recording: mirakc.RecordInfo{
			Status:    "finished",
			StartTime: startTime,
			Options:   mirakc.Options{ContentPath: &contentPath},
		},
		Content: mirakc.ContentInfo{
			Path: "/recording/" + contentPath,
			Type: "video/mp2t",
		},
	}
	return record, contentPath
}

type ingestLifecycleMirakc struct {
	server         *httptest.Server
	recordSaved    chan struct{}
	eventsReady    chan struct{}
	setHash        func(string)
	setEndTime     func(time.Time)
	deleteAttempts *atomic.Int32
}

func newIngestLifecycleMirakc(t *testing.T, initial mirakc.Record, tsData []byte) *ingestLifecycleMirakc {
	t.Helper()
	run := &ingestLifecycleMirakc{
		recordSaved:    make(chan struct{}, 2),
		eventsReady:    make(chan struct{}),
		deleteAttempts: &atomic.Int32{},
	}
	var mu sync.RWMutex
	current := initial
	run.setHash = func(hash string) {
		mu.Lock()
		currentHash := hash
		current.Content.Sha256 = &currentHash
		mu.Unlock()
	}
	run.setEndTime = func(endTime time.Time) {
		mu.Lock()
		end := mirakc.Milliseconds(endTime)
		current.Recording.EndTime = &end
		mu.Unlock()
	}
	var connected sync.Once
	run.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/events" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Errorf("SSE response writer %T does not support flushing", w)
				return
			}
			flusher.Flush()
			connected.Do(func() { close(run.eventsReady) })
			for {
				select {
				case <-run.recordSaved:
					data, _ := json.Marshal(mirakc.RecordSavedData{RecordID: initial.ID, RecordingStatus: "finished"})
					_, _ = fmt.Fprintf(w, "event: recording.record-saved\ndata: %s\n\n", data)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.URL.Path == "/api/recording/records/"+initial.ID && r.Method == http.MethodGet:
			mu.RLock()
			record := current
			if current.Content.Sha256 != nil {
				hash := *current.Content.Sha256
				record.Content.Sha256 = &hash
			}
			mu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(record)
		case r.URL.Path == "/api/recording/records/"+initial.ID+"/stream" && r.Method == http.MethodHead:
			w.Header().Set("Content-Length", strconv.Itoa(len(tsData)))
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/recording/records/"+initial.ID+"/stream" && r.Method == http.MethodGet:
			writeIngestLifecycleRange(w, r, tsData)
		case r.URL.Path == "/api/recording/records/"+initial.ID && r.Method == http.MethodDelete:
			run.deleteAttempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mirakc.RecordRemovalResult{RecordRemoved: true, ContentRemoved: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(run.server.Close)
	return run
}

func newIngestLifecycleRiverClient(t *testing.T, pool *pgxpool.Pool, ingestWorker *worker.IngestWorker) *river.Client[pgx5.Tx] {
	t.Helper()
	workers := river.NewWorkers()
	river.AddWorker(workers, ingestWorker)
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			jobs.PhysicalQueueName(jobs.IngestQueue, watcher.DefaultSite): {MaxWorkers: 1},
		},
		Workers: workers,
	})
	if err != nil {
		t.Fatalf("creating River client: %v", err)
	}
	return client
}

func waitForIngestLifecycleCompletion(t *testing.T, events <-chan *river.Event) *river.Event {
	t.Helper()
	select {
	case event := <-events:
		if event.Job == nil {
			t.Fatal("completed River event has no job")
		}
		return event
	case <-time.After(30 * time.Second):
		t.Fatal("ingest job did not complete after record-saved")
		return nil
	}
}

func assertIngestLifecycleWaited(t *testing.T, pool *pgxpool.Pool, jobID int64, deleteAttempts int32, mediaDir, recordID string, wantTempSize int64) {
	t.Helper()
	ctx := context.Background()
	job := testutil.MustGetRiverJob(t, ctx, testutil.NewRiverClient(t, pool), jobID)
	if job.State != rivertype.JobStateCompleted {
		t.Errorf("persisted first job state = %q, want completed", job.State)
	}
	var assetCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM media_assets WHERE kind = 'original'").Scan(&assetCount); err != nil {
		t.Fatalf("counting original media assets while waiting: %v", err)
	}
	if assetCount != 0 {
		t.Errorf("original media assets while waiting = %d, want 0", assetCount)
	}
	if deleteAttempts != 0 {
		t.Errorf("DeleteRecord attempts while waiting = %d, want 0", deleteAttempts)
	}
	tempPath := filepath.Join(mediaDir, "sites", "default", "test", mediapath.IngestTempFilePrefix+"default-"+recordID)
	if info, err := os.Stat(tempPath); err != nil || info.Size() != wantTempSize {
		t.Errorf("ingest temp while waiting: stat=(%v, %v), want size %d", info, err, wantTempSize)
	}
}

func assertIngestLifecycleCommitted(t *testing.T, pool *pgxpool.Pool, mediaDir, contentPath string, tsData []byte, deleteAttempts int32) {
	t.Helper()
	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE kind = 'original'").Scan(&assetCount); err != nil {
		t.Fatalf("counting committed original media assets: %v", err)
	}
	if assetCount != 1 {
		t.Fatalf("committed original media assets = %d, want 1", assetCount)
	}
	if deleteAttempts != 1 {
		t.Errorf("DeleteRecord attempts after verified commit = %d, want 1", deleteAttempts)
	}
	fullPath := filepath.Join(mediaDir, "sites", "default", filepath.FromSlash(contentPath))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading committed media file: %v", err)
	}
	if !bytes.Equal(data, tsData) {
		t.Error("committed media file differs from the downloaded TS data")
	}
}

func writeIngestLifecycleRange(w http.ResponseWriter, r *http.Request, data []byte) {
	var offset int64
	if header := r.Header.Get("Range"); header != "" {
		_, _ = fmt.Sscanf(strings.TrimPrefix(header, "bytes="), "%d-", &offset)
	}
	if offset >= int64(len(data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := data[offset:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

func ingestLifecycleTSData(packets int) []byte {
	data := make([]byte, packets*188)
	for i := 0; i < packets; i++ {
		offset := i * 188
		data[offset] = 0x47
		data[offset+1] = 0x01
		data[offset+2] = 0x00
		data[offset+3] = 0x10 | byte(i%16)
	}
	return data
}

func lifecycleSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}
