package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// tunerFixture は mirakc の /api/tuners のレスポンスを差し替え可能に保持する。
type tunerFixture struct {
	tuners []mirakc.Tuner
	// raw が非 nil ならそれをそのまま返す（レスポンスの生 JSON を検証したいとき用）。
	raw []byte
}

func newTunerServer(t *testing.T, fx *tunerFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tuners" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if fx.raw != nil {
			_, _ = w.Write(fx.raw)
			return
		}
		_ = json.NewEncoder(w).Encode(fx.tuners)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTunerSyncWorker(t *testing.T, pool *pgxpool.Pool, fx *tunerFixture) *TunerSyncWorker {
	t.Helper()
	srv := newTunerServer(t, fx)
	return &TunerSyncWorker{MirakcClient: mirakc.NewClient(srv.URL, nil), Pool: pool}
}

func runTunerSync(t *testing.T, w *TunerSyncWorker) {
	t.Helper()
	job := &river.Job[TunerSyncArgs]{JobRow: &rivertype.JobRow{}, Args: TunerSyncArgs{Site: testSite}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
}

func allTuners(t *testing.T, pool *pgxpool.Pool) []sqlcgen.TunerSync {
	t.Helper()
	rows, err := sqlcgen.New(pool).ListTunerSync(context.Background(), testSite)
	if err != nil {
		t.Fatalf("ListTunerSync: %v", err)
	}
	return rows
}

// 全量取得 → upsert → スイープの 1 パス。
func TestTunerSyncWorker_FullSync(t *testing.T) {
	pool := setupTestPool(t)
	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
		{Index: 1, Name: "PX-W3U4_S1", Types: []string{"BS", "CS"}, IsAvailable: true},
	}}
	w := newTunerSyncWorker(t, pool, fx)

	runTunerSync(t, w)

	rows := allTuners(t, pool)
	if len(rows) != 2 {
		t.Fatalf("tuner_sync rows = %+v, want 2", rows)
	}
	if rows[0].TunerIndex != 0 || rows[0].Name != "PX-S1UD_T1" {
		t.Errorf("row[0] = %+v, want index 0 / PX-S1UD_T1", rows[0])
	}
	if !reflect.DeepEqual(rows[1].Types, []string{"BS", "CS"}) {
		t.Errorf("row[1].types = %v, want [BS CS]", rows[1].Types)
	}
	if !rows[0].IsAvailable || rows[0].IsFault {
		t.Errorf("row[0] availability = (%v, %v), want (true, false)", rows[0].IsAvailable, rows[0].IsFault)
	}

	// 2 パス目で更新が反映され、消えたチューナーがスイープされること。
	fx.tuners = []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true, IsFault: true},
	}
	runTunerSync(t, w)

	rows = allTuners(t, pool)
	if len(rows) != 1 {
		t.Fatalf("tuner_sync rows after sweep = %+v, want 1", rows)
	}
	if !rows[0].IsFault {
		t.Errorf("row[0].is_fault = %v, want true (2 パス目の観測が反映される)", rows[0].IsFault)
	}
}

// 実行時状態（users / isFree / isUsing / command / pid）は投影しない。
// 現在の利用者を容量から引くと「見えない消費者は数えない = 下界」の性質が崩れる。
func TestTunerSyncWorker_DoesNotProjectRuntimeState(t *testing.T) {
	pool := setupTestPool(t)
	fx := &tunerFixture{raw: []byte(`[{
      "index": 0, "name": "PX-S1UD_T1", "types": ["GR"],
      "isAvailable": true, "isFault": false,
      "users": [{"agent":"epgstation","priority":1}],
      "isFree": false, "isUsing": true, "command": "recdvb", "pid": 4242
    }]`)}
	w := newTunerSyncWorker(t, pool, fx)

	runTunerSync(t, w)

	rows := allTuners(t, pool)
	if len(rows) != 1 {
		t.Fatalf("tuner_sync rows = %+v, want 1", rows)
	}
	// 使用中のチューナーも「存在して壊れていない」ので容量に数える。
	if !rows[0].IsAvailable || rows[0].IsFault {
		t.Errorf("row = %+v, want available and not faulty even while in use", rows[0])
	}
	// 実行時状態を保持する列は存在しない（存在したら投影される経路ができてしまう）。
	var count int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM information_schema.columns
WHERE table_name = 'tuner_sync'
  AND column_name IN ('users', 'is_free', 'is_using', 'command', 'pid')`).Scan(&count); err != nil {
		t.Fatalf("querying tuner_sync columns: %v", err)
	}
	if count != 0 {
		t.Errorf("tuner_sync has %d runtime-state columns, want 0", count)
	}
}

// 未知の channel_type を含むチューナーは、そのチューナーだけ捨てて同期を続行する
// （epg_services が未知の channel_type のサービスだけ捨てるのと同じ規律）。
func TestTunerSyncWorker_SkipsTunerWithUnknownChannelType(t *testing.T) {
	pool := setupTestPool(t)
	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
		{Index: 1, Name: "未来の受信機", Types: []string{"GR", "ISDB-T3"}, IsAvailable: true},
	}}
	w := newTunerSyncWorker(t, pool, fx)

	runTunerSync(t, w)

	rows := allTuners(t, pool)
	if len(rows) != 1 {
		t.Fatalf("tuner_sync rows = %+v, want 1 (未知の種別を含む 1 本だけを捨てる)", rows)
	}
	if rows[0].TunerIndex != 0 {
		t.Errorf("projected tuner index = %d, want 0", rows[0].TunerIndex)
	}
}

// types が空のチューナーは投影する（どの cap にも数えられないだけで無害）。
func TestTunerSyncWorker_ProjectsTunerWithoutTypes(t *testing.T) {
	pool := setupTestPool(t)
	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "設定ミスのチューナー", Types: []string{}, IsAvailable: true},
	}}
	w := newTunerSyncWorker(t, pool, fx)

	runTunerSync(t, w)

	rows := allTuners(t, pool)
	if len(rows) != 1 {
		t.Fatalf("tuner_sync rows = %+v, want 1", rows)
	}
	if len(rows[0].Types) != 0 {
		t.Errorf("row.types = %v, want empty", rows[0].Types)
	}
}

// 空レスポンスでスイープを走らせない。射影が消えると容量判定が何も主張しなくなり
// （tuner_sync が空 = 判定しない）、警告が黙って消える。
func TestTunerSyncWorker_SkipsSweepOnEmptyResponse(t *testing.T) {
	pool := setupTestPool(t)
	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
	}}
	w := newTunerSyncWorker(t, pool, fx)
	runTunerSync(t, w)
	if len(allTuners(t, pool)) != 1 {
		t.Fatalf("precondition failed: want 1 projected tuner")
	}

	fx.tuners = []mirakc.Tuner{}
	runTunerSync(t, w)

	if rows := allTuners(t, pool); len(rows) != 1 {
		t.Errorf("tuner_sync rows = %+v, want the projection kept (空レスポンスでスイープしない)", rows)
	}

	// 反対方向: 非空のレスポンスならスイープする。
	fx.tuners = []mirakc.Tuner{
		{Index: 9, Name: "別のチューナー", Types: []string{"BS"}, IsAvailable: true},
	}
	runTunerSync(t, w)
	rows := allTuners(t, pool)
	if len(rows) != 1 || rows[0].TunerIndex != 9 {
		t.Errorf("tuner_sync rows = %+v, want only index 9 after a non-empty pass", rows)
	}
}

// 容量超過区間のゲージが同期パスで入れ直されること（site ラベル付き）。
//
// 非ゼロは信頼できるがゼロは「大丈夫」を意味しない、という非対称な読み方は
// metrics.CapacityOverages のコメント側の責務。ここでは配線だけを見る。
func TestTunerSyncWorker_SetsCapacityOverageGauge(t *testing.T) {
	pool := setupTestPool(t)
	start := time.Now().Truncate(time.Hour).Add(24 * time.Hour)

	// GR 1 本に対して別チャンネル 2 予約 → 1 区間が超過。
	// #27 で番組の事実のスナップショットが program_snapshots に抽出され、
	// reservations への FK が張られたため、予約行より先に program_snapshots を作る。
	ctx := context.Background()
	for i, channel := range []string{"27", "25"} {
		programID := int64(900 + i)
		if _, err := pool.Exec(ctx, `
INSERT INTO program_snapshots (
  site, program_id, title, start_at, duration_ms,
  network_id, service_id, channel_type, channel, event_id, service_name
) VALUES ($1, $2, 'ゲージ確認', $3, $4, 32678, 5168, 'GR', $5, $6, 'テスト局')`,
			testSite, programID, start, time.Hour.Milliseconds(), channel, int32(programID)); err != nil {
			t.Fatalf("inserting program_snapshot: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO reservations (site, program_id, base) VALUES ($1, $2, '{}')`,
			testSite, programID); err != nil {
			t.Fatalf("inserting reservation: %v", err)
		}
	}

	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
	}}
	w := newTunerSyncWorker(t, pool, fx)
	runTunerSync(t, w)

	if got := promtestutil.ToFloat64(metrics.CapacityOverages.WithLabelValues(testSite)); got != 1 {
		t.Errorf("rokuban_capacity_overages{site=%q} = %v, want 1", testSite, got)
	}
	if got := promtestutil.ToFloat64(metrics.TunersProjected.WithLabelValues(testSite)); got != 1 {
		t.Errorf("rokuban_tuners_projected{site=%q} = %v, want 1", testSite, got)
	}

	// 反対方向: チューナーが 2 本になれば収まってゼロに戻る。
	fx.tuners = append(fx.tuners, mirakc.Tuner{Index: 1, Name: "PX-S1UD_T2", Types: []string{"GR"}, IsAvailable: true})
	runTunerSync(t, w)

	if got := promtestutil.ToFloat64(metrics.CapacityOverages.WithLabelValues(testSite)); got != 0 {
		t.Errorf("rokuban_capacity_overages{site=%q} = %v, want 0 after adding a tuner", testSite, got)
	}
}

// TunerSyncSite を指定すると tuner_sync が定期ジョブとして投入され、
// 登録済みワーカーが epg キューで拾うこと（配線の確認）。
func TestTunerSyncPeriodicJob(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := newTunerServer(t, &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
	}})

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil)})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:      true,
		TunerSyncSite:     testSite,
		TunerSyncInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "tuner_sync" {
			t.Errorf("job kind = %q, want %q", event.Job.Kind, "tuner_sync")
		}
		if event.Job.Queue != epgQueue {
			t.Errorf("job queue = %q, want %q", event.Job.Queue, epgQueue)
		}
		var args TunerSyncArgs
		if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
			t.Fatalf("unmarshalling job args: %v", err)
		}
		if args.Site != testSite {
			t.Errorf("job args site = %q, want %q", args.Site, testSite)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the periodic tuner_sync job")
	}

	if rows := allTuners(t, pool); len(rows) != 1 {
		t.Errorf("tuner_sync rows = %+v, want 1 after the periodic job ran", rows)
	}
}

// TestTunerSyncWorker_SiteMismatch は、job.Args.Site がワーカー自身の site
// （w.Site）と一致しないジョブが mirakc に一切触れずに fail-fast することを
// 確認する（issue #139）。tuner_sync は epg_sync と同じ epg キューを使う
// 「使い捨てプロジェクションの全量同期」で、同じくガード対象（TunerSyncWorker
// の doc コメント参照）。モックは 200 を返す（弱いテストにしないため。issue
// #139 のテスト規律）。
func TestTunerSyncWorker_SiteMismatch(t *testing.T) {
	pool := setupTestPool(t)

	var requests atomic.Int32
	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
	}}
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/tuners" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fx.tuners)
	}))
	defer countingSrv.Close()

	// このワーカープロセスは site-a の mirakc を向いている。
	w := &TunerSyncWorker{MirakcClient: mirakc.NewClient(countingSrv.URL, nil), Pool: pool, Site: "site-a"}

	job := &river.Job[TunerSyncArgs]{JobRow: &rivertype.JobRow{}, Args: TunerSyncArgs{Site: "site-b"}}
	err := w.Work(context.Background(), job)
	if err == nil {
		t.Fatal("Work() error = nil, want error for site mismatch (site-a worker handling a site-b job)")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("mirakc received %d requests, want 0 (guard must fail before touching mirakc): err=%v", got, err)
	}
}

// TestTunerSyncWorker_SiteMatch は、args.Site が一致するジョブは従来どおり
// 処理されることを確認する（TestTunerSyncWorker_SiteMismatch と対になる
// 両方向の確認）。
func TestTunerSyncWorker_SiteMatch(t *testing.T) {
	pool := setupTestPool(t)

	fx := &tunerFixture{tuners: []mirakc.Tuner{
		{Index: 0, Name: "PX-S1UD_T1", Types: []string{"GR"}, IsAvailable: true},
	}}
	srv := newTunerServer(t, fx)

	w := &TunerSyncWorker{MirakcClient: mirakc.NewClient(srv.URL, nil), Pool: pool, Site: "site-a"}

	job := &river.Job[TunerSyncArgs]{JobRow: &rivertype.JobRow{}, Args: TunerSyncArgs{Site: "site-a"}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error = %v, want nil for matching site", err)
	}

	rows, err := sqlcgen.New(pool).ListTunerSync(context.Background(), "site-a")
	if err != nil {
		t.Fatalf("ListTunerSync: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("tuner_sync rows for site-a = %d, want 1", len(rows))
	}
}
