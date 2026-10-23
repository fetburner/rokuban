package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/jobqfake"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/worker"
)

func TestRecordSavedKeepsSnoozedIngestAndCommitsLateSHA256(t *testing.T) {
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
		Publisher:     pgstore.NewMedia(pool),
		RecordTarget:  pgstore.NewRecordings(pool),
		MediaDir:      mediaDir,
		StallTimeout:  5 * time.Second,
		Precheck:      pgstore.NewMedia(pool),
	}
	ingestWorker.CMDetectionStore = pgstore.NewAnalysis(pool)
	workResults := make(chan error, 2)
	queue := newIngestLifecycleQueue(t, ingestWorker, workResults)
	watcherEnqueuer := &ingestLifecycleEnqueuer{}

	runCtx, cancel := context.WithCancel(context.Background())
	watcherDone := make(chan error, 1)
	go func() {
		watcherDone <- watcher.New(watcher.DefaultSite, mc, pgstore.NewRecordings(pool), pgstore.NewReservations(pool), watcherEnqueuer, nil).Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-watcherDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("watcher stopped after cancellation with %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watcher did not stop after context cancellation")
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := queue.Stop(stopCtx); err != nil {
			t.Errorf("jobqfake queue did not stop: %v", err)
		}
	})

	select {
	case <-mcServer.eventsReady:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not connect to the SSE endpoint")
	}
	mcServer.setEndTime(time.Now())
	mcServer.recordSaved <- struct{}{}
	waitForIngestLifecycleEnqueues(t, watcherEnqueuer, 1)
	waitForIngestLifecycleRecordSync(t, pool, record.ID)
	enqueued := watcherEnqueuer.IngestJobs()
	if _, err := queue.Enqueue(context.Background(), enqueued[0].args, enqueued[0].opts...); err != nil {
		t.Fatalf("queueing first watcher ingest hint: %v", err)
	}
	waitForIngestLifecycleDeliveries(t, queue, 1)
	waitForIngestLifecycleQueue(t, queue)
	firstResult := <-workResults
	if delay, ok := jobq.SnoozeDelay(firstResult); !ok || delay != 30*time.Second {
		t.Fatalf("first ingest result = %T %v (snooze %v, %v); want 30s snooze while SHA-256 is null", firstResult, firstResult, delay, ok)
	}
	assertIngestLifecycleWaited(t, pool, int32(mcServer.fake.CallCount(http.MethodDelete, "/api/recording/records/"+record.ID)), mediaDir, record.ID, int64(len(tsData)))

	mcServer.setHash(lifecycleSHA256(tsData))
	mcServer.recordSaved <- struct{}{}
	waitForIngestLifecycleEnqueues(t, watcherEnqueuer, 2)
	enqueued = watcherEnqueuer.IngestJobs()
	if _, err := queue.Enqueue(context.Background(), enqueued[1].args, enqueued[1].opts...); err != nil {
		t.Fatalf("queueing second watcher ingest hint: %v", err)
	}
	waitForIngestLifecycleDeliveries(t, queue, 2)
	waitForIngestLifecycleQueue(t, queue)
	if err := <-workResults; err != nil {
		t.Fatalf("ingest result after late SHA-256 arrived: %v", err)
	}
	if !strings.Contains(logOutput.String(), "sha256_verification=verified") {
		t.Errorf("logs after the second job = %q, want verified SHA-256 commit", logOutput.String())
	}
	assertIngestLifecycleCommitted(t, pool, mediaDir, contentPath, tsData, int32(mcServer.fake.CallCount(http.MethodDelete, "/api/recording/records/"+record.ID)))
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
	server      *httptest.Server
	fake        *mirakctest.Fake
	recordSaved chan struct{}
	eventsReady chan struct{}
	setHash     func(string)
	setEndTime  func(time.Time)
}

func newIngestLifecycleMirakc(t *testing.T, initial mirakc.Record, tsData []byte) *ingestLifecycleMirakc {
	t.Helper()
	run := &ingestLifecycleMirakc{
		recordSaved: make(chan struct{}, 2),
		eventsReady: make(chan struct{}),
	}
	var mu sync.RWMutex
	current := initial
	run.fake = mirakctest.New(t)
	run.fake.SetRecords([]mirakc.Record{initial})
	run.fake.SetRecordStream(initial.ID, tsData)
	setCurrent := func() {
		mu.RLock()
		record := current
		mu.RUnlock()
		run.fake.SetRecords([]mirakc.Record{record})
	}
	run.setHash = func(hash string) {
		mu.Lock()
		currentHash := hash
		current.Content.Sha256 = &currentHash
		mu.Unlock()
		setCurrent()
	}
	run.setEndTime = func(endTime time.Time) {
		mu.Lock()
		end := mirakc.Milliseconds(endTime)
		current.Recording.EndTime = &end
		mu.Unlock()
		setCurrent()
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
		default:
			run.fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(run.server.Close)
	return run
}

type ingestLifecycleWorker struct {
	worker *worker.IngestWorker
	result chan<- error
}

type ingestLifecycleEnqueue struct {
	args jobq.Args
	opts []jobq.EnqueueOpt
}

// ingestLifecycleEnqueuer records the watcher request.
type ingestLifecycleEnqueuer struct {
	mu   sync.Mutex
	jobs []ingestLifecycleEnqueue
}

func (enqueuer *ingestLifecycleEnqueuer) Enqueue(_ context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	return enqueuer.record(args, opts...), nil
}

func (enqueuer *ingestLifecycleEnqueuer) record(args jobq.Args, opts ...jobq.EnqueueOpt) jobq.Result {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.jobs = append(enqueuer.jobs, ingestLifecycleEnqueue{args: args, opts: append([]jobq.EnqueueOpt(nil), opts...)})
	return jobq.Result{}
}

func (enqueuer *ingestLifecycleEnqueuer) IngestJobs() []ingestLifecycleEnqueue {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	var found []ingestLifecycleEnqueue
	for _, enqueued := range enqueuer.jobs {
		if enqueued.args.Kind() == (jobs.IngestJobArgs{}).Kind() {
			found = append(found, enqueued)
		}
	}
	return found
}

func (worker ingestLifecycleWorker) Work(ctx context.Context, args jobs.IngestJobArgs) error {
	worker.result <- worker.worker.Work(ctx, args)
	return nil
}

func newIngestLifecycleQueue(t *testing.T, ingestWorker *worker.IngestWorker, workResults chan<- error) *jobqfake.Queue {
	t.Helper()
	registry := jobq.NewRegistry()
	jobq.Register(registry, ingestLifecycleWorker{worker: ingestWorker, result: workResults})
	delay := time.Duration(0)
	queue := jobqfake.New(registry, jobqfake.Config{SnoozeDelay: &delay})
	ingestWorker.Enqueuer = queue
	if err := queue.Start(context.Background()); err != nil {
		t.Fatalf("starting jobqfake queue: %v", err)
	}
	return queue
}

func waitForIngestLifecycleDeliveries(t *testing.T, queue *jobqfake.Queue, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, delivery := range queue.Deliveries() {
			if delivery.Kind == (jobs.IngestJobArgs{}).Kind() {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ingest deliveries = %d, want at least %d after record-saved", len(queue.Deliveries()), want)
}

func waitForIngestLifecycleEnqueues(t *testing.T, enqueuer *ingestLifecycleEnqueuer, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got := len(enqueuer.IngestJobs()); got >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watcher ingest enqueue requests = %d, want at least %d after record-saved", len(enqueuer.IngestJobs()), want)
}

// waitForIngestLifecycleRecordSync waits until the post-commit watcher hint can be delivered to the queue.
// The second hint needs no wait for new recording data: between the hints the fixture changes only sha256,
// which the worker reads from mirakc, and UpsertRecordSync keeps the committed recording_id.
func waitForIngestLifecycleRecordSync(t *testing.T, pool *pgxpool.Pool, recordID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var committed bool
		if err := pool.QueryRow(context.Background(),
			"SELECT EXISTS (SELECT 1 FROM record_sync WHERE site = 'default' AND record_id = $1 AND recording_id IS NOT NULL)", recordID,
		).Scan(&committed); err != nil {
			t.Fatalf("checking committed record_sync: %v", err)
		}
		if committed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("record_sync for %s was not committed with a recording_id", recordID)
}

func waitForIngestLifecycleQueue(t *testing.T, queue *jobqfake.Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := queue.WaitIdle(ctx); err != nil {
		t.Fatalf("jobqfake.WaitIdle: %v", err)
	}
}

func assertIngestLifecycleWaited(t *testing.T, pool *pgxpool.Pool, deleteAttempts int32, mediaDir, recordID string, wantTempSize int64) {
	t.Helper()
	ctx := context.Background()
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
