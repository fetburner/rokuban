package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// siteMarkerAt は loop_success_markers の (loop, testSite) の時刻を返す。行が無ければ ok=false。
func siteMarkerAt(t *testing.T, pool *pgxpool.Pool, loop string) (at time.Time, ok bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT last_success_at FROM loop_success_markers WHERE loop = $1 AND scope = $2`, loop, testSite).Scan(&at)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false
		}
		t.Fatalf("reading marker %s/%s: %v", loop, testSite, err)
	}
	return at, true
}

// rejectLoopMarker は loop の印の書き込みを DB 側で失敗させる。読み取りには影響しない。
func rejectLoopMarker(t *testing.T, pool *pgxpool.Pool, loop string) {
	t.Helper()
	testutil.MustExec(t, pool, "rejecting loop marker writes", `
CREATE OR REPLACE FUNCTION reject_sync_marker() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.loop = '`+loop+`' THEN
    RAISE EXCEPTION 'loop marker write rejected';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER reject_sync_marker BEFORE INSERT OR UPDATE ON loop_success_markers
FOR EACH ROW EXECUTE FUNCTION reject_sync_marker()`)
}

// rejectTableWrites は table への挿入を DB 側で失敗させ、同期の必須の書き込みを失敗させる。
func rejectTableWrites(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	testutil.MustExec(t, pool, "rejecting writes to "+table, `
CREATE OR REPLACE FUNCTION reject_sync_writes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'write to `+table+` rejected';
END;
$$;
CREATE TRIGGER reject_sync_writes BEFORE INSERT ON `+table+`
FOR EACH ROW EXECUTE FUNCTION reject_sync_writes()`)
}

// failingEnqueuer は ruler へのヒントの投入だけを失敗させる。
type failingEnqueuer struct{}

func (failingEnqueuer) Enqueue(context.Context, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	return jobq.Result{}, errors.New("enqueue rejected")
}

// unreachableMirakc は接続できない mirakc クライアントを返す。取得の失敗を起こすため。
func unreachableMirakc() map[string]*mirakc.Client {
	return singleSiteClients("", mirakc.NewClient("http://127.0.0.1:1", nil))
}

// 同期の確定時刻（loop_success_markers の epg_sync / tuner_sync、scope は site）は、
// 取得と必須の書き込みが全部済んだ同期だけで進む。ヒントや容量計算の失敗は同期の失敗ではない。
func TestSyncWorkersMarkSiteOnlyWhenSyncCompletes(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	services := []mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")}
	programs := []mirakc.Program{testProgram(32736, 1024, 1, "番組A", now.Add(time.Hour), time.Hour)}
	tuners := []mirakc.Tuner{{Index: 0, Name: "GR-0", Types: []string{"GR"}}}

	epgCases := []struct {
		name       string
		arrange    func(t *testing.T, pool *pgxpool.Pool, w *EpgSyncWorker)
		wantErr    bool
		wantMarked bool
	}{
		{name: "completed sync advances the mark", wantMarked: true},
		{
			name:       "ruler hint failure does not fail the sync",
			arrange:    func(_ *testing.T, _ *pgxpool.Pool, w *EpgSyncWorker) { w.Enqueuer = failingEnqueuer{} },
			wantMarked: true,
		},
		{
			name:    "mirakc fetch failure does not advance the mark",
			arrange: func(_ *testing.T, _ *pgxpool.Pool, w *EpgSyncWorker) { w.MirakcClients = unreachableMirakc() },
			wantErr: true,
		},
		{
			name:    "required DB write failure does not advance the mark",
			arrange: func(t *testing.T, pool *pgxpool.Pool, _ *EpgSyncWorker) { rejectTableWrites(t, pool, "epg_programs") },
			wantErr: true,
		},
		{
			name:    "marker write failure fails the job so that a retry re-marks it",
			arrange: func(t *testing.T, pool *pgxpool.Pool, _ *EpgSyncWorker) { rejectLoopMarker(t, pool, store.LoopEPGSync) },
			wantErr: true,
		},
	}
	for _, tc := range epgCases {
		t.Run("epg/"+tc.name, func(t *testing.T) {
			t.Parallel()
			pool := setupTestPool(t)
			fx := &epgFixture{services: services, programs: programs}
			fake := newEpgFake(t, fx)
			w := &EpgSyncWorker{
				MirakcClients: singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
				Store:         pgstoretest.EPG(pool), Markers: pgstoretest.Reservations(pool),
				RetentionGrace: 24 * time.Hour,
			}
			if tc.arrange != nil {
				tc.arrange(t, pool, w)
			}
			err := runWorkerViaJobQFake(context.Background(), w, jobs.EpgSyncArgs{Site: testSite})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Work() error = %v, want error %t", err, tc.wantErr)
			}
			at, marked := siteMarkerAt(t, pool, store.LoopEPGSync)
			if marked != tc.wantMarked {
				t.Fatalf("epg_sync marker present = %t (at %v), want %t", marked, at, tc.wantMarked)
			}
		})
	}

	tunerCases := []struct {
		name       string
		arrange    func(t *testing.T, pool *pgxpool.Pool, w *TunerSyncWorker)
		wantErr    bool
		wantMarked bool
	}{
		{name: "completed sync advances the mark", wantMarked: true},
		{
			name: "capacity computation failure does not fail the sync",
			arrange: func(_ *testing.T, _ *pgxpool.Pool, w *TunerSyncWorker) {
				w.CapacityStore = failingCapacity{Capacity: w.CapacityStore}
			},
			wantMarked: true,
		},
		{
			name:    "mirakc fetch failure does not advance the mark",
			arrange: func(_ *testing.T, _ *pgxpool.Pool, w *TunerSyncWorker) { w.MirakcClients = unreachableMirakc() },
			wantErr: true,
		},
		{
			name:    "required DB write failure does not advance the mark",
			arrange: func(t *testing.T, pool *pgxpool.Pool, _ *TunerSyncWorker) { rejectTableWrites(t, pool, "tuner_sync") },
			wantErr: true,
		},
		{
			name: "marker write failure fails the job so that a retry re-marks it",
			arrange: func(t *testing.T, pool *pgxpool.Pool, _ *TunerSyncWorker) {
				rejectLoopMarker(t, pool, store.LoopTunerSync)
			},
			wantErr: true,
		},
	}
	for _, tc := range tunerCases {
		t.Run("tuner/"+tc.name, func(t *testing.T) {
			t.Parallel()
			pool := setupTestPool(t)
			fx := &tunerFixture{tuners: tuners}
			w := newTunerSyncWorker(t, pool, fx)
			w.Markers = pgstoretest.Reservations(pool)
			if tc.arrange != nil {
				tc.arrange(t, pool, w)
			}
			err := runWorkerViaJobQFake(context.Background(), w, jobs.TunerSyncArgs{Site: testSite})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Work() error = %v, want error %t", err, tc.wantErr)
			}
			at, marked := siteMarkerAt(t, pool, store.LoopTunerSync)
			if marked != tc.wantMarked {
				t.Fatalf("tuner_sync marker present = %t (at %v), want %t", marked, at, tc.wantMarked)
			}
		})
	}
}

// failingCapacity は容量計算の読み取りだけを失敗させる。
type failingCapacity struct{ store.Capacity }

func (failingCapacity) ReadSite(context.Context, string) (store.CapacityInput, error) {
	return store.CapacityInput{}, errors.New("capacity read rejected")
}
