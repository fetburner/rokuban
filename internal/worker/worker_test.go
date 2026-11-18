package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func runWorkerViaJobQFake[T jobq.Args](ctx context.Context, worker jobq.Worker[T], args T) error {
	return jobqfaketest.RunWorkerOnce(ctx, worker, args)
}

type mirakcRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f mirakcRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestNewWorkersInjectsIngestPrecheck runs the registered ingest worker through the registry.
// The original row exists only in the DB: Work must find it through the injected precheck,
// skip transfer, and clear stale progress for that recording.
func TestNewWorkersInjectsIngestPrecheck(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	const recordID = "rec-new-workers-precheck"
	recordingID := dbtest.Recording(t, pool)
	insertTestRecordSync(t, pool, recordingID, recordID)
	media := pgstoretest.Media(pool)
	seedOriginalAsset(t, pool, t.TempDir(), recordingID, "sites/default/new-workers-precheck.m2ts", []byte("x"))
	testutil.RequireNoError(t, media.UpsertIngestProgress(context.Background(), store.IngestProgress{
		RecordingID:    recordingID,
		WrittenBytes:   32,
		TempRelPath:    "sites/default/.rokuban-ingest-precheck",
		TempGeneration: 1,
	}), "seeding ingest progress")

	fake := mirakctest.New(t)
	fake.SetRecords([]mirakc.Record{{ID: recordID}})

	// 置き場を 1 つ束ねた構成として組む。PerStore を持たない Deps からは置き場のワーカーは登録されない。
	registry := NewWorkers(&Deps{
		MountedStores: []string{""},
		PerStore: map[string]*Deps{"": {
			MirakcClients:      singleSiteClients(store.DefaultSite, mirakc.NewClient(fake.URL(), nil)),
			MediaPublish:       media,
			IngestTarget:       pgstoretest.Recordings(pool),
			IngestPrecheck:     media,
			Enqueuer:           newRecordingEnqueuer(),
			IngestStallTimeout: time.Second,
		}},
	})
	testutil.RequireNoError(t, registry.Work(context.Background(), jobs.IngestJobArgs{Site: store.DefaultSite, RecordID: recordID}), "registered ingest worker Work()")
	if _, ok := readIngestProgress(t, pool, recordingID); ok {
		t.Error("registered ingest worker did not clear stale progress")
	}
}

// singleSiteClients は 1 site だけを束縛するテスト用の Deps.MirakcClients /
// *Worker.MirakcClients を組み立てる（issue #532 で単一の *mirakc.Client
// フィールドが map になったことに伴うテストヘルパー）。site を省略（""）した
// 呼び出しは store.DefaultSite を使う --- 大半のワーカーテストは複数 site を
// 気にしないので、単一サイトの map を毎回手で組み立てずに済ませる。
func singleSiteClients(site string, client *mirakc.Client) map[string]*mirakc.Client {
	if site == "" {
		site = store.DefaultSite
	}
	return map[string]*mirakc.Client{site: client}
}

// recordingEnqueuer は投入された Args を記録する。tx の commit / rollback は観測しない
// （投入の原子性は jobq の契約テストが守る）。
type recordingEnqueuer struct {
	mu   sync.Mutex
	args []jobq.Args
}

func newRecordingEnqueuer() *recordingEnqueuer { return &recordingEnqueuer{} }

func (enqueuer *recordingEnqueuer) Enqueue(_ context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.args = append(enqueuer.args, args)
	return jobq.Result{}, nil
}

func (enqueuer *recordingEnqueuer) Enqueued(kind string) []jobq.Args {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	var found []jobq.Args
	for _, args := range enqueuer.args {
		if args.Kind() == kind {
			found = append(found, args)
		}
	}
	return found
}

func (enqueuer *recordingEnqueuer) Reset() {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.args = nil
}

func enqueuedArgs[T jobq.Args](enqueuer *recordingEnqueuer, kind string) []T {
	var found []T
	for _, args := range enqueuer.Enqueued(kind) {
		if typed, ok := args.(T); ok {
			found = append(found, typed)
		}
	}
	return found
}

func countEnqueuedForRecording(t *testing.T, enqueuer *recordingEnqueuer, kind string, recordingID int64) int {
	t.Helper()
	count := 0
	for _, args := range enqueuer.Enqueued(kind) {
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("encoding %s args: %v", kind, err)
		}
		var keyed struct {
			RecordingID int64 `json:"recording_id"`
		}
		if err := json.Unmarshal(encoded, &keyed); err != nil {
			t.Fatalf("decoding %s args: %v", kind, err)
		}
		if keyed.RecordingID == recordingID {
			count++
		}
	}
	return count
}

// epg_sync のパス完了はヒント経路の 1 つ: RulerPassArgs を投入して評価を早める
// （docs/recording.md §3.1「EPG 同期の完了」）。
func TestEpgSyncWorker_EnqueuesRulerPassHint(t *testing.T) {
	t.Parallel()
	fake := newEpgFake(t, &epgFixture{})
	enqueuer := newRecordingEnqueuer()
	pool := testutil.SetupDB(t)
	w := &EpgSyncWorker{
		Enqueuer:      enqueuer,
		MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		Store:         pgstoretest.EPG(pool),
		RulerInput:    pgstoretest.Reservations(pool),
	}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, EpgSyncArgs{Site: testSite}), "EpgSyncWorker.Work")
	hints := enqueuedArgs[jobs.RulerPassArgs](enqueuer, "ruler_pass")
	if len(hints) != 1 || hints[0].Site != testSite {
		t.Fatalf("enqueued ruler_pass hints = %+v, want one hint for site %q", hints, testSite)
	}
	var revision int64
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `SELECT revision FROM ruler_input_revisions WHERE site = $1`, testSite).Scan(&revision), "reading ruler input revision")
	if revision != 1 {
		t.Fatalf("ruler input revision = %d, want 1 after EPG sync", revision)
	}
}

// 区切り + 処理余白の上限は River の既定 JobTimeout より長く、rescue 待ちより短い。
func TestIngestWorker_TimeoutFitsBeforeRescue(t *testing.T) {
	t.Parallel()
	args := jobs.IngestJobArgs{}
	got := args.Spec().Timeout(args)
	if got != 5*time.Minute {
		t.Errorf("Timeout() = %v, want 5m", got)
	}
	if got := ingestTransferSlice; got != 4*time.Minute {
		t.Errorf("ingest transfer slice = %v, want 4m", got)
	}
}

// EPG 同期は無制限にせず、10 分で打ち切る。
func TestEpgSyncWorker_HasGenerousTimeout(t *testing.T) {
	t.Parallel()
	args := EpgSyncArgs{}
	got := args.Spec().Timeout(args)
	if got != 10*time.Minute {
		t.Errorf("Timeout() = %v, want 10m", got)
	}
}
