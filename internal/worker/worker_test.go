package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// singleSiteClients は 1 site だけを束縛するテスト用の Deps.MirakcClients /
// *Worker.MirakcClients を組み立てる（issue #532 で単一の *mirakc.Client
// フィールドが map になったことに伴うテストヘルパー）。site を省略（""）した
// 呼び出しは db.DefaultSite を使う --- 大半のワーカーテストは複数 site を
// 気にしないので、単一サイトの map を毎回手で組み立てずに済ませる。
func singleSiteClients(site string, client *mirakc.Client) map[string]*mirakc.Client {
	if site == "" {
		site = db.DefaultSite
	}
	return map[string]*mirakc.Client{site: client}
}

// TestVerifySite は issue #532 の受け入れ基準 3 を固定する: verifySite は
// clients（束縛サイトの map）に無い jobSite を拒み、ある jobSite は通して
// そのクライアントを返す。
//
// このテストが検出すべき変異: verifySite が clients の中身を見ずに常に通す
// （map の判定 `clients[site]` を消して常に何らかのクライアントを返す等）。
// その場合 "unbound site is refused" が誤って通ってしまい、このテストが落ちる。
func TestVerifySite(t *testing.T) {
	tokyoClient := mirakc.NewClient("http://tokyo.example", nil)
	takamatsuClient := mirakc.NewClient("http://takamatsu.example", nil)
	clients := map[string]*mirakc.Client{
		"tokyo":     tokyoClient,
		"takamatsu": takamatsuClient,
	}

	t.Run("bound site is allowed and returns its own client", func(t *testing.T) {
		got, err := verifySite(clients, "tokyo", "ingest")
		if err != nil {
			t.Fatalf("verifySite: %v", err)
		}
		if got != tokyoClient {
			t.Errorf("verifySite returned %v, want the tokyo client", got)
		}
	})

	t.Run("a second bound site is also allowed (N-site binding)", func(t *testing.T) {
		got, err := verifySite(clients, "takamatsu", "ingest")
		if err != nil {
			t.Fatalf("verifySite: %v", err)
		}
		if got != takamatsuClient {
			t.Errorf("verifySite returned %v, want the takamatsu client", got)
		}
	})

	t.Run("unbound site is refused", func(t *testing.T) {
		if _, err := verifySite(clients, "osaka", "ingest"); err == nil {
			t.Fatal("verifySite() error = nil, want error for a site not in clients")
		}
	})

	// jobSite は正規化しない: 空文字列は無条件に拒む。これは "default" という
	// 名前の site がたまたま束縛されているデプロイでも同じでなければならない
	// --- 正規化すると、args.Site 自体が壊れている（本来 verifySite が拾うべき）
	// ジョブが「default site 宛のジョブ」として素通りしてしまう。
	t.Run("empty jobSite is refused even when a site named default is bound", func(t *testing.T) {
		defaultOnly := singleSiteClients("", tokyoClient)
		if _, err := verifySite(defaultOnly, "", "ingest"); err == nil {
			t.Fatal("verifySite() error = nil, want error for an empty (unnormalized) jobSite")
		}
	})
}

func TestNoOpJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	// River の配線（投入 → 実行 → 完了イベント）だけを確かめるための何もしない
	// ワーカー。本番の NewWorkers には登録しない（テスト専用のジョブ種別が
	// 本番のキューに存在してしまうのを避ける）。
	workers := NewWorkers(&Deps{Pool: pool})
	river.AddWorker(workers, &noOpWorker{})

	client, err := NewClient(pool, workers, ClientConfig{IngestConcurrency: 2})
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

	_, err = client.Insert(ctx, noOpArgs{}, nil)
	if err != nil {
		t.Fatalf("inserting job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "noop" {
			t.Errorf("job kind = %q, want %q", event.Job.Kind, "noop")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for job completion")
	}
}

// startPeriodicJobClient は *PeriodicJob テスト（TestEpgSyncPeriodicJob /
// TestRecordSweepPeriodicJob / TestTunerSyncPeriodicJob /
// TestReconcilePassPeriodicJob）が共有する配線のセットアップだけを担う。
// ジョブ種別・キュー名・args の主張はテスト側に残す（CLAUDE.md「実装の定数と
// 比較するテストは何も主張していない」）。
func startPeriodicJobClient(t *testing.T, pool *pgxpool.Pool, deps *Deps, cfg ClientConfig, eventKind river.EventKind) <-chan *river.Event {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	deps.Pool = pool
	workers := NewWorkers(deps)
	client, err := NewClient(pool, workers, cfg)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(eventKind)
	t.Cleanup(subscribeCancel)

	clientCtx, clientCancel := context.WithCancel(ctx)
	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	t.Cleanup(func() {
		clientCancel()
		<-client.Stopped()
	})

	return subscribeCh
}

// waitPeriodicJobEvent は startPeriodicJobClient と対になる待ち受けの
// 共通部分（timeout 付き select）だけを担う。kind / queue / args の主張は
// 呼び出し側に残す。
//
// **jobKind が一致するイベントが来るまで読み飛ばす。** issue #532 で
// ClientConfig.BoundSites が epg_sync/tuner_sync/ruler_pass/reconcile_pass/
// record_sweep の 5 種をまとめて登録するようになったため、1 つの site を
// 束縛しただけで残り 4 種も RunOnStart で同時に走る。呼び出し側が見たいのは
// そのうちの 1 種だけなので、最初に届いたイベントを無条件に返すと別の種類の
// ジョブに化けたときにテストが誤判定する（実際、複数種が同時完了する構成で
// 順序は保証されない）。
func waitPeriodicJobEvent(t *testing.T, subscribeCh <-chan *river.Event, jobKind string) *river.Event {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind == jobKind {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the periodic %s job", jobKind)
			return nil
		}
	}
}

// BoundSites を指定すると epg_sync が定期ジョブとして投入され、登録済み
// ワーカーが epg キューで拾うこと（配線の確認。BoundSites は epg_sync 以外の
// 4 種も同時に登録するが、waitPeriodicJobEvent が kind で選り分ける）。
func TestEpgSyncPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)

	// mirakc を叩かせずに配線だけ見たいので、/api/services で失敗させる。
	// ジョブが失敗すれば「投入されてワーカーに届いた」ことは確認できる。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	// t.Cleanup（defer だとクライアント停止より先に走り、動いている最中にスタブを閉じる）。
	t.Cleanup(srv.Close)

	subscribeCh := startPeriodicJobClient(t, pool, &Deps{MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil))}, ClientConfig{
		PeriodicJobs:    true,
		BoundSites:      []string{"default"},
		EpgSyncInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	}, river.EventKindJobFailed)

	event := waitPeriodicJobEvent(t, subscribeCh, "epg_sync")
	if event.Job.Kind != "epg_sync" {
		t.Errorf("job kind = %q, want %q", event.Job.Kind, "epg_sync")
	}
	wantQueue := qualifyQueueName(epgQueue, "default")
	if event.Job.Queue != wantQueue {
		t.Errorf("job queue = %q, want %q", event.Job.Queue, wantQueue)
	}
	var args EpgSyncArgs
	if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
		t.Fatalf("unmarshalling job args: %v", err)
	}
	if args.Site != "default" {
		t.Errorf("job args site = %q, want %q", args.Site, "default")
	}
}

// epg_sync のパス完了はヒント経路の 1 つ: RulerPassArgs を投入して評価を早める
// （docs/recording.md §3.1「EPG 同期の完了」）。
func TestEpgSyncWorker_EnqueuesRulerPassHint(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	srv := newEpgServer(t, &epgFixture{})

	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{})
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

	if _, err := client.Insert(ctx, EpgSyncArgs{Site: testSite}, nil); err != nil {
		t.Fatalf("inserting epg_sync job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "epg_sync" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "epg_sync")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for epg_sync job completion")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'ruler_pass' AND (args->>'site') = $1`, testSite,
	).Scan(&count); err != nil {
		t.Fatalf("counting ruler_pass jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("ruler_pass job count after epg_sync completion = %d, want 1 "+
			"(epg_sync 完了時にヒントとして投入されるはず)", count)
	}
}

// ingest は数百 MB〜数十 GB の転送なので、River の総時間タイムアウト（既定 1 分）が
// 効いていると実際の録画が完走しない。総時間で切らずストール検知に委ねる設計が
// 崩れていないことを固定する（実機 687MB の録画がこれで落ちていた）。
func TestIngestWorker_HasNoTotalTimeout(t *testing.T) {
	w := &IngestWorker{}
	if got := w.Timeout(nil); got >= 0 {
		t.Errorf("Timeout() = %v, want negative (River のタイムアウト無効化)", got)
	}
}

// EPG 同期は無制限にはせず、既定より長い上限を置く。
func TestEpgSyncWorker_HasGenerousTimeout(t *testing.T) {
	w := &EpgSyncWorker{}
	got := w.Timeout(nil)
	if got <= river.JobTimeoutDefault {
		t.Errorf("Timeout() = %v, want > JobTimeoutDefault (%v)", got, river.JobTimeoutDefault)
	}
}

// 完了済みのジョブが一意性の判定に入っていないこと。
//
// River の既定（UniqueOptsByStateDefault）は completed を含むため、既定のままだと
// 一度成功した引数のジョブが二度と投入できなくなる。epg_sync は 10 分間隔の
// 定期ジョブなので、これに当たると実質ワンショットになる（実際にそうなっていた）。
func TestInsertOpts_UniqueStatesExcludeFinalized(t *testing.T) {
	tests := []struct {
		name string
		opts river.InsertOpts
	}{
		{"epg_sync", EpgSyncArgs{}.InsertOpts()},
		{"ingest", IngestJobArgs{}.InsertOpts()},
		{"encode", EncodeJobArgs{}.InsertOpts()},
		{"ruler_pass", RulerPassArgs{}.InsertOpts()},
		{"reconcile_pass", ReconcilePassArgs{}.InsertOpts()},
		{"catalog_export", CatalogExportArgs{}.InsertOpts()},
		{"storage_sync", StorageSyncArgs{}.InsertOpts()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := tt.opts.UniqueOpts.ByState
			if len(states) == 0 {
				t.Fatal("ByState が空だと River の既定（completed を含む）が使われる")
			}
			for _, s := range states {
				switch s {
				case rivertype.JobStateCompleted, rivertype.JobStateDiscarded, rivertype.JobStateCancelled:
					t.Errorf("終了状態 %q が一意性の判定に含まれている", s)
				}
			}
			// 同時実行を防ぐ目的は満たしていること
			if !slices.Contains(states, rivertype.JobStateRunning) {
				t.Error("running が含まれていないと同時実行を防げない")
			}
		})
	}
}

// site 単位のキュー（ingest/epg/reconciler/watcher。tuner_sync は epg キューを
// 共有）と cleanup（delete_reconcile/catalog_export）は UniqueOpts.ByQueue: true
// を立てていること。ruler は対象外（キュー名を変えていないので不要。
// physicalQueueName のコメント参照）。
//
// 立てないと、キュー名を変える（今回の site 修飾・cleanup への移設）だけで
// 旧キューの残骸が新キューへの Insert を UniqueSkippedAsDuplicate として
// 黙って塞ぐ（pendingJobStates 直後の doc コメント、issue #185 のレビュー
// 指摘）。この 1 つのテーブルにまとめておくことで、7 種のうち 1 つでも
// ByQueue を書き忘れたときに検出漏れが起きないようにする。
//
// storage_sync だけは事情が違う（キュー名を変えたことがないので、この表が
// 押さえている「リネームで塞がる」失敗はまだ起きえない）。専用の storage
// キューを新設した時点で先に立てておくという選択の記録としてここに置く ---
// 後から `storage` を改名したくなったときに、上記の失敗を踏み直さないため。
func TestInsertOpts_ByQueueForRenamedQueues(t *testing.T) {
	tests := []struct {
		name string
		opts river.InsertOpts
		want bool
	}{
		{"ingest", IngestJobArgs{}.InsertOpts(), true},
		{"epg_sync", EpgSyncArgs{}.InsertOpts(), true},
		{"tuner_sync", TunerSyncArgs{}.InsertOpts(), true},
		{"reconcile_pass", ReconcilePassArgs{}.InsertOpts(), true},
		{"record_sweep", RecordSweepArgs{}.InsertOpts(), true},
		{"delete_reconcile", DeleteReconcileArgs{}.InsertOpts(), true},
		{"catalog_export", CatalogExportArgs{}.InsertOpts(), true},
		{"storage_sync (new queue, set up-front against a future rename)", StorageSyncArgs{}.InsertOpts(), true},
		{"ruler_pass (queue name unchanged, not required)", RulerPassArgs{}.InsertOpts(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.UniqueOpts.ByQueue; got != tt.want {
				t.Errorf("UniqueOpts.ByQueue = %v, want %v", got, tt.want)
			}
		})
	}
}

// 定期投入された epg_sync が、前回のジョブが完了した後でも再度投入できること。
func TestEpgSync_ReinsertableAfterCompletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	args := EpgSyncArgs{Site: "default"}
	first, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting first job: %v", err)
	}

	// 未完了のうちは重複として弾かれる（同時実行を防ぐ）
	dup, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting duplicate: %v", err)
	}
	if !dup.UniqueSkippedAsDuplicate {
		t.Error("未完了のジョブがあるのに重複として弾かれなかった")
	}

	// 完了させると再度投入できる。公開テスト API は実物の completer 経路を
	// 通るため、River が完了時に更新する列の集合もこのテストに反映される。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning transaction for completion: %v", err)
	}
	defer tx.Rollback(ctx) // harmless after a successful commit
	testWorker := rivertest.NewWorker[EpgSyncArgs, pgx.Tx](
		t, riverpgxv5.New(pool), &river.Config{}, &epgSyncNoOpWorker{},
	)
	result, err := testWorker.WorkJob(ctx, t, tx, first.Job)
	if err != nil {
		t.Fatalf("working epg_sync job: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing completed epg_sync job: %v", err)
	}
	if result.Job.State != rivertype.JobStateCompleted {
		t.Fatalf("completed epg_sync state = %q, want %q", result.Job.State, rivertype.JobStateCompleted)
	}

	again, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting after completion: %v", err)
	}
	if again.UniqueSkippedAsDuplicate {
		t.Error("完了済みのジョブが一意性の判定に残っており、定期ジョブが再投入できない")
	}
}

// ruler は無制限にはせず、既定より長い上限を置く（rulerPassTimeout のコメント参照）。
func TestRulerPassWorker_HasGenerousTimeout(t *testing.T) {
	w := &RulerPassWorker{}
	got := w.Timeout(nil)
	if got <= river.JobTimeoutDefault {
		t.Errorf("Timeout() = %v, want > JobTimeoutDefault (%v)", got, river.JobTimeoutDefault)
	}
}

// BoundSites を指定すると ruler_pass が定期ジョブとして投入され、登録済み
// ワーカーが ruler キューで拾うこと（配線の確認）。BoundSites は
// epg_sync/tuner_sync/ruler_pass/reconcile_pass/record_sweep の 5 種を
// まとめて登録する（RunOnStart で全部 1 回走る）ので、mirakc スタブ
// （newScheduleStub、reconcile_pass_test.go）を与えて reconcile_pass だけは
// 実際に正常完了することも確認する。epg_sync/tuner_sync/record_sweep は
// このスタブが `/api/recording/schedules` しか実装していないため、依然として
// 失敗する（以前は Deps.MirakcClients が空で verifySite の「bound sites に
// 無い」エラーだったが、スタブを与えた今は HTTP 404 に変わっただけで、
// 3 種とも成功はしない。JobCompleted しか購読していないのでこの失敗は
// テストに現れない）。
//
// **下の 2 つ目の待ち受けは「ruler_pass 完了時に連鎖投入される reconcile_pass
// ヒント」を待っているとは限らない。** BoundSites + PeriodicJobs は
// RunOnStart 付きの reconcile_pass も別途 site ごとに 1 本登録する
// （buildRiverConfig、defaultReconcilePassInterval）。ReconcilePassArgs の
// UniqueOpts{ByArgs, ByState: pendingJobStates} により、この定期ジョブと
// ruler_pass 完了時のヒントは同じ (site) の重複としてほぼ必ず合流するため、
// kind だけで待っても両者を区別できない。ここで実際に検証できているのは
// 「BoundSites=[default] のこの構成で、何らかの reconcile_pass が正常完了
// する」ことだけ。**連鎖されたヒント自体が実行されて成功する**という、より
// 狭い主張は TestRulerPassWorker_EnqueuesReconcilePassHint
// （reconcile_pass_test.go。PeriodicJobs を登録しないので合流する定期ジョブが
// 存在せず、区別できる）が固定している。
func TestRulerPassPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	stub := newScheduleStub()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	workers := NewWorkers(&Deps{Pool: pool, MirakcClients: singleSiteClients("", mirakc.NewClient(srv.URL, nil))})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:      true,
		BoundSites:        []string{"default"},
		RulerPassInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
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

	rulerEvent := waitPeriodicJobEvent(t, subscribeCh, "ruler_pass")
	if rulerEvent.Job.Queue != rulerQueue {
		t.Errorf("job queue = %q, want %q", rulerEvent.Job.Queue, rulerQueue)
	}
	var rulerArgs RulerPassArgs
	if err := json.Unmarshal(rulerEvent.Job.EncodedArgs, &rulerArgs); err != nil {
		t.Fatalf("unmarshalling job args: %v", err)
	}
	if rulerArgs.Site != "default" {
		t.Errorf("job args site = %q, want %q", rulerArgs.Site, "default")
	}

	// この構成（BoundSites=[default] + mirakc スタブ）で reconcile_pass が実際に
	// 正常完了することの確認（どちらが完了したかは上のコメントの通り区別しない）。
	// 正常完了しないとここが 20 秒でタイムアウトする。
	reconcileEvent := waitPeriodicJobEvent(t, subscribeCh, "reconcile_pass")
	var reconcileArgs ReconcilePassArgs
	if err := json.Unmarshal(reconcileEvent.Job.EncodedArgs, &reconcileArgs); err != nil {
		t.Fatalf("unmarshalling reconcile_pass args: %v", err)
	}
	if reconcileArgs.Site != "default" {
		t.Errorf("reconcile_pass job args site = %q, want %q", reconcileArgs.Site, "default")
	}
}

// TestRulerPassPeriodicJob_MultiSite は issue #532 の受け入れ基準 2 の site 軸を
// 固定する: BoundSites に 2 site を渡すと、両方の site に対して ruler_pass の
// 定期ジョブが実際に投入されて実行される（TestBuildRiverConfig_PeriodicJobsPerSite
// は件数だけを見る純粋な単体テストなので、こちらは実際に DB へ書かれる site の
// 値まで見る）。ruler は mirakc に触れないので、site 単位の配線だけを確認する
// のに向く（Deps に MirakcClients を渡していない）。
//
// このテストが検出すべき変異: buildRiverConfig が BoundSites の 1 要素目だけを
// 使う。その場合 2 つ目の site（下のテストでは "site-b"）向けの ruler_pass が
// 一度も投入されず、このテストが 20 秒でタイムアウトして落ちる。
func TestRulerPassPeriodicJob_MultiSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:      true,
		BoundSites:        []string{"site-a", "site-b"},
		RulerPassInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
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

	seen := map[string]bool{}
	deadline := time.After(20 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-subscribeCh:
			if event.Job.Kind != "ruler_pass" {
				continue
			}
			var args RulerPassArgs
			if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
				t.Fatalf("unmarshalling job args: %v", err)
			}
			seen[args.Site] = true
		case <-deadline:
			t.Fatalf("timed out waiting for ruler_pass on both sites; seen=%v", seen)
		}
	}
	if !seen["site-a"] || !seen["site-b"] {
		t.Errorf("seen sites = %v, want site-a and site-b", seen)
	}
}

// ruler_pass ジョブが投入され、実際に実行されて予約が作られること
// （ロジック自体は internal/ruler でテスト済みなので、ここでは「ジョブとして
// 呼ばれると本当に動く」という配線だけを確認する）。
func TestRulerPassWorker_CreatesReservation(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	const site = "default"
	const networkID, serviceID int32 = 32736, 1024
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)`, site, networkID, serviceID); err != nil {
		t.Fatalf("inserting epg_services fixture: %v", err)
	}

	const programID int64 = 1
	startAt := time.Now().Add(time.Hour)
	const durationMs int64 = 1800000
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at, is_free, name, description, genre_lv1)
VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, 'テスト番組', '', '{}'::smallint[])`,
		site, programID, networkID, serviceID, startAt, durationMs, startAt.Add(time.Duration(durationMs)*time.Millisecond)); err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}

	var ruleID int64
	if err := pool.QueryRow(ctx, `INSERT INTO rules (name, priority) VALUES ('全部録る', 10) RETURNING id`).Scan(&ruleID); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, 0, 'name', 'keyword', 'テスト', true, false)`, ruleID); err != nil {
		t.Fatalf("inserting rule_text_matches fixture: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
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

	if _, err := client.Insert(ctx, RulerPassArgs{Site: site}, nil); err != nil {
		t.Fatalf("inserting ruler_pass job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "ruler_pass" {
			t.Fatalf("job kind = %q, want %q", event.Job.Kind, "ruler_pass")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for ruler_pass job completion")
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE site = $1 AND program_id = $2`, site, programID,
	).Scan(&count); err != nil {
		t.Fatalf("querying reservations: %v", err)
	}
	if count != 1 {
		t.Fatalf("reservations count = %d, want 1 (ruler_pass ジョブがルール評価を実行して予約を作ったはず)", count)
	}
}

// UniqueOpts による合流: 同じサイトの ruler_pass を 2 回投入すると 1 件しか
// 作られないこと（docs/data.md §2「排他はジョブロック + UniqueOpts」）。
func TestRulerPass_DuplicateInsertMerges(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	args := RulerPassArgs{Site: "default"}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		t.Fatalf("inserting first job: %v", err)
	}
	dup, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("inserting duplicate: %v", err)
	}
	if !dup.UniqueSkippedAsDuplicate {
		t.Error("同じサイトの ruler_pass を 2 回投入したのに合流しなかった")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'ruler_pass'`).Scan(&count); err != nil {
		t.Fatalf("counting river_job rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("river_job count for ruler_pass = %d, want 1", count)
	}
}

// worker.queues に未知のキュー名を渡すと起動時エラーになること（typo で静かに
// 何も引かなくなる事故を防ぐ knob。docs/configuration.md の worker.queues）。
func TestBuildRiverConfig_UnknownQueueErrors(t *testing.T) {
	_, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{Queues: []string{"bogus"}})
	if err == nil {
		t.Fatal("unknown queue のとき error を期待したが nil だった")
	}
}

// buildRiverConfig が実際に購読する物理キュー名の集合を起動時ログに出すこと
// （issue #185 の「罠」: 全キュー購読（既定）は「全部購読しているつもりで
// 実は site 束縛キューを引いていない」を起動時に伝える手段が要る）。
//
// このテストが検出すべき変異: buildRiverConfig からログ出力そのものを削除する、
// または queues の代わりに空リストや別の値をログに渡す。いずれも
// ログバッファに期待した物理名（"ingest_tokyo" 等）が現れなくなり、
// strings.Contains のアサーションが落ちる。実際に注入して確認済み
// （PR 本文の失敗出力を参照）。
func TestBuildRiverConfig_LogsSubscribedQueues(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if _, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{BoundSites: []string{"tokyo"}}); err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}

	logged := logBuf.String()
	for _, want := range []string{"ingest_tokyo", "epg_tokyo", "reconciler_tokyo", "watcher_tokyo", "cleanup", "ruler"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output does not mention %q; log:\n%s", want, logged)
		}
	}
	// site 束縛の値そのものも出ていること（「中央プロセスで既定のまま起動して
	// 実は何も引いていない」を区別できるようにするため）。
	if !strings.Contains(logged, "bound_sites=[tokyo]") {
		t.Errorf("log output does not mention bound_sites=[tokyo]; log:\n%s", logged)
	}
}

// **SoftStopTimeout が未設定（0）でも river.Config に非 0 が載ること。**
//
// これは「設定し忘れ」が最も危険な側に倒れないための既定である。River は
// SoftStopTimeout が 0 のとき work ctx を start ctx から継ぐので
// （river@v0.47.0/client.go:1150-1154 の workParentCtx）、0 のまま渡すと
// `signal.NotifyContext` の ctx を Start に渡している構成で **SIGTERM が
// 実行中のジョブを即座に打ち切る**。0 は「無制限」ではなく「待たない」である。
//
// SIGTERM が実際に drain になることは cmd/rokuban の
// TestServerCmd_SigtermDrainsRunningJob が実 DB で測る。ここでは
// 「0 を素通しさせない」ことだけを固定する。
//
// 実測: `SoftStopTimeout: 0` を river.Config に渡す変異で 2 行とも赤くなる
// ことを確認した。
func TestBuildRiverConfig_SoftStopTimeoutIsNeverZero(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if riverCfg.SoftStopTimeout <= 0 {
		t.Errorf("SoftStopTimeout = %s, want > 0（SIGTERM が実行中のジョブを即座に打ち切る）",
			riverCfg.SoftStopTimeout)
	}

	// 明示した値はそのまま載る（既定に丸められない）。
	explicit, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{SoftStopTimeout: 90 * time.Second})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if explicit.SoftStopTimeout != 90*time.Second {
		t.Errorf("SoftStopTimeout = %s, want 90s", explicit.SoftStopTimeout)
	}
}

// encode / thumbnail キューが allQueues に載り、concurrency が独立に効くこと
// （issue #64。ワーカー本体は M3-3 / M3-4 で、枠だけ先に用意する）。
func TestBuildRiverConfig_EncodeThumbnailConcurrency(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		EncodeConcurrency:    3,
		ThumbnailConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if qc, ok := riverCfg.Queues[encodeQueue]; !ok {
		t.Fatalf("queue %q missing from Queues", encodeQueue)
	} else if qc.MaxWorkers != 3 {
		t.Errorf("encode MaxWorkers = %d, want 3", qc.MaxWorkers)
	}
	if qc, ok := riverCfg.Queues[thumbnailQueue]; !ok {
		t.Fatalf("queue %q missing from Queues", thumbnailQueue)
	} else if qc.MaxWorkers != 2 {
		t.Errorf("thumbnail MaxWorkers = %d, want 2", qc.MaxWorkers)
	}
	// 既定（0）は 1 になる
	defaults, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{})
	if err != nil {
		t.Fatalf("buildRiverConfig defaults: %v", err)
	}
	if defaults.Queues[encodeQueue].MaxWorkers != 1 {
		t.Errorf("default encode MaxWorkers = %d, want 1", defaults.Queues[encodeQueue].MaxWorkers)
	}
	if defaults.Queues[thumbnailQueue].MaxWorkers != 1 {
		t.Errorf("default thumbnail MaxWorkers = %d, want 1", defaults.Queues[thumbnailQueue].MaxWorkers)
	}
}

// worker.periodic_jobs: false のとき、BoundSites が設定されていても
// PeriodicJobs が登録されないこと。
func TestBuildRiverConfig_PeriodicJobsDisabled(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs: false,
		BoundSites:   []string{"default"},
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs = %d 件, want 0 (worker.periodic_jobs=false のとき登録されないこと)",
			len(riverCfg.PeriodicJobs))
	}
}

// worker.periodic_jobs: true なら site 単位の 5 種（epg_sync/tuner_sync/
// ruler_pass/reconcile_pass/record_sweep）全部が BoundSites の 1 site ぶん
// 登録されること。
func TestBuildRiverConfig_PeriodicJobsEnabled(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs: true,
		BoundSites:   []string{"default"},
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 5 {
		t.Errorf("PeriodicJobs = %d 件, want 5 (epg_sync + tuner_sync + ruler_pass + reconcile_pass + record_sweep)",
			len(riverCfg.PeriodicJobs))
	}
}

// TestBuildRiverConfig_PeriodicJobsPerSite は issue #532 の受け入れ基準 2 を
// 固定する: BoundSites に 2 site を渡すと、site 単位の 5 種
// （epg_sync/tuner_sync/ruler_pass/reconcile_pass/record_sweep）が **site ごとに
// 1 本ずつ**登録される（1 度だけではない）。
//
// このテストが検出すべき変異: buildRiverConfig が BoundSites の最初の 1 要素
// だけを使う（旧 BoundSite string の名残りで `cfg.BoundSites[0]` に固定する
// 等）。その場合 PeriodicJobs は 5 件のまま増えず、このテストが落ちる
// （TestBuildRiverConfig_PeriodicJobsEnabled の 1-site ケースと数を比較して
// 初めて検出できるので、2 つのテストは対になっている）。
func TestBuildRiverConfig_PeriodicJobsPerSite(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs: true,
		BoundSites:   []string{"tokyo", "takamatsu"},
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	const perSite = 5 // epg_sync + tuner_sync + ruler_pass + reconcile_pass + record_sweep
	if want := perSite * 2; len(riverCfg.PeriodicJobs) != want {
		t.Errorf("PeriodicJobs = %d 件, want %d (%d 種 x 2 site)", len(riverCfg.PeriodicJobs), want, perSite)
	}
}

// TestBuildRiverConfig_SubscribesSiteBoundQueuesPerSite は issue #532 の
// 「含むもの」2 を固定する: BoundSites に 2 site を渡すと、site 単位のキュー
// （ingest/epg/reconciler/watcher）は物理名（`<base>_<site>`）で site ごとに
// **両方**購読される --- insert 側（各 JobArgs.InsertOpts）は 1 ジョブの
// args.Site から物理名が 1 つに決まるので変わらないが、subscribe 側
// （buildRiverConfig の river.Config.Queues）だけが N 回展開される
// （qualifyQueueName のコメント「subscribe 側だけ N 回呼ぶ」）。
//
// このテストが検出すべき変異: 物理キュー展開のループが BoundSites の最初の
// 1 要素だけを使う（例: `boundSites[:1]`）。その場合 takamatsu 側の物理キュー
// （`ingest_takamatsu` 等）が river.Config.Queues に一度も現れず、この
// テストが落ちる。実際に注入して確認済み（golangci-lint 通過後の変異確認）。
func TestBuildRiverConfig_SubscribesSiteBoundQueuesPerSite(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		BoundSites: []string{"tokyo", "takamatsu"},
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	for _, base := range []string{"ingest", "epg", "reconciler", "watcher"} {
		for _, site := range []string{"tokyo", "takamatsu"} {
			want := base + "_" + site
			if _, ok := riverCfg.Queues[want]; !ok {
				t.Errorf("river.Config.Queues is missing %q (site-bound queues must be subscribed per bound site); got %v",
					want, sortedQueueNames(riverCfg.Queues))
			}
		}
	}
}

// RequiresEncodeTools が worker.queues の絞り込みと連動していること
// （issue #113 決定 C）。既定（空）や encode/thumbnail を明示的に含む場合は
// ffmpeg/ffprobe 検査が必要、それ以外に絞った場合は不要になる。
func TestRequiresEncodeTools(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including encode/thumbnail", nil, true},
		{"explicit encode", []string{encodeQueue}, true},
		{"explicit thumbnail", []string{thumbnailQueue}, true},
		{"explicit both", []string{encodeQueue, thumbnailQueue}, true},
		{"ingest only excludes encode/thumbnail", []string{ingestQueue}, false},
		{"ruler/reconciler only excludes encode/thumbnail", []string{rulerQueue, reconcilerQueue}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresEncodeTools(tt.queues); got != tt.want {
				t.Errorf("RequiresEncodeTools(%v) = %v, want %v", tt.queues, got, tt.want)
			}
		})
	}
}

// RequiresSiteBinding は cmd/rokuban が「worker ロールを 0 サイト束縛
// （issue #183 M4-11 の --sites=）で起動してよいか」を判定する唯一の材料。
// ここが誤ると、site 単位のジョブ（ingest/epg/reconciler/watcher）を
// 引く worker が空文字列 site のまま起動し、届いたジョブの site と一致せず
// 全滅して再試行し続ける（ログにも出ない）。
//
// ruler は site 非依存（issue #185 M4-13。#138 の決定表 --- DB のみで mirakc に
// 触れない）なので、ruler だけの購読は 0 サイト束縛を要求しない。
func TestRequiresSiteBinding(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including site-bound ones", nil, true},
		{"explicit ingest", []string{ingestQueue}, true},
		{"explicit epg", []string{epgQueue}, true},
		{"explicit ruler does not require binding (site-independent, issue #185)", []string{rulerQueue}, false},
		{"explicit reconciler", []string{reconcilerQueue}, true},
		{"explicit watcher (record_sweep)", []string{recordSweepQueue}, true},
		{"encode/thumbnail/cleanup/ruler only excludes site-bound queues", []string{encodeQueue, thumbnailQueue, cleanupQueue, rulerQueue}, false},
		{"explicit storage does not require binding (site-independent, issue #238)", []string{storageQueue}, false},
		{"encode/thumbnail plus one site-bound queue still requires binding", []string{encodeQueue, ingestQueue}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresSiteBinding(tt.queues); got != tt.want {
				t.Errorf("RequiresSiteBinding(%v) = %v, want %v", tt.queues, got, tt.want)
			}
		})
	}
}

// この 1 つのテストが、M4-11 が M4-13 に申し送った罠を直接固定する（issue #185
// のコメント）: worker.queues（config）は論理名のままで、RequiresSiteBinding /
// RequiresEncodeTools はその論理名に対して判定する。キュー名を site で修飾する
// 実装が、誤ってこの論理名そのもの（キュー定数や siteBoundQueueNames の要素）を
// 修飾済みの文字列に変えてしまうと、cmd/rokuban.validateSiteBinding が
// worker.queues の値と一致判定できなくなり、0 サイト束縛の worker が site 単位の
// キューを購読できる状態のまま起動時ガードを素通りする。
func TestRequiresSiteBinding_LogicalQueueNamesStayUnqualified(t *testing.T) {
	for _, base := range []string{ingestQueue, epgQueue, reconcilerQueue, recordSweepQueue} {
		if strings.Contains(base, "_") {
			t.Errorf("queue base %q looks site-qualified; worker.queues (config) と "+
				"RequiresSiteBinding/RequiresEncodeTools は論理名（unqualified）を前提にしている", base)
		}
		if !RequiresSiteBinding([]string{base}) {
			t.Errorf("RequiresSiteBinding([%q]) = false, want true (site-bound queue のはず)", base)
		}
	}
}

// qualifyQueueName は base_site の形にする。空文字列 site は db.DefaultSite に
// 解決する（verifySite / DeleteReconcileWorker.Work と同じ規約。issue #185）。
func TestQualifyQueueName(t *testing.T) {
	tests := []struct {
		name string
		base string
		site string
		want string
	}{
		{"basic", ingestQueue, "tokyo", "ingest_tokyo"},
		{"empty site resolves to db.DefaultSite", epgQueue, "", "epg_default"},
		{"reconciler", reconcilerQueue, "takamatsu", "reconciler_takamatsu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qualifyQueueName(tt.base, tt.site); got != tt.want {
				t.Errorf("qualifyQueueName(%q, %q) = %q, want %q", tt.base, tt.site, got, tt.want)
			}
		})
	}
}

// physicalQueueName は siteBoundQueueNames に含まれる論理名だけ修飾し、
// それ以外（ruler/encode/thumbnail/cleanup/default）はそのまま通す。
// want はすべてリテラルで書く（logical と同じ定数を want にも使うと、
// 「qualify しない」ケースは定数の値が何であっても常に一致してしまい、
// 意図した論理名そのものが正しいかを確認しない。issue #185 のレビュー指摘）。
func TestPhysicalQueueName(t *testing.T) {
	tests := []struct {
		name      string
		logical   string
		boundSite string
		want      string
	}{
		{"ingest gets qualified", ingestQueue, "tokyo", "ingest_tokyo"},
		{"epg gets qualified", epgQueue, "tokyo", "epg_tokyo"},
		{"reconciler gets qualified", reconcilerQueue, "tokyo", "reconciler_tokyo"},
		{"watcher (record_sweep) gets qualified", recordSweepQueue, "tokyo", "watcher_tokyo"},
		{"ruler is NOT qualified (site-independent, issue #185)", rulerQueue, "tokyo", "ruler"},
		{"encode is NOT qualified", encodeQueue, "tokyo", "encode"},
		{"thumbnail is NOT qualified", thumbnailQueue, "tokyo", "thumbnail"},
		{"cleanup is NOT qualified", cleanupQueue, "tokyo", "cleanup"},
		{"storage is NOT qualified (site-independent, issue #238)", storageQueue, "tokyo", "storage"},
		{"default is NOT qualified", river.QueueDefault, "tokyo", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := physicalQueueName(tt.logical, tt.boundSite); got != tt.want {
				t.Errorf("physicalQueueName(%q, %q) = %q, want %q", tt.logical, tt.boundSite, got, tt.want)
			}
		})
	}
}

// TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen は、
// config.MirakcSiteNameMaxLen まで許した site 名を qualifyQueueName で修飾しても
// riverQueueNameMaxLen を超えないことを機械的に固定する。config は worker を
// import できない（逆方向のみ許される）ので、両方が見える worker 側にこの関係の
// テストを置く。site 名の長さ検査は config.validateSiteName の 1 本だけなので
// （worker 側の重複検査は無くした）、この関係が壊れると config のロード時検査を
// 通った site 名が qualifyQueueName で 64 文字を超える。それを渡した先は
// worker ロールを持つプロセスなら起動時（river.NewClient → Config.validate →
// QueueConfig.validate、river@v0.47.0/client.go:605-606,692-693 が
// validateQueueName を呼ぶ）に落ち、insert-only クライアント（`rokuban
// enqueue` 等）では Insert 時（client.go:1723）に初めて落ちる。
//
// siteBoundQueueNames のどの論理名についても、site 名を
// config.MirakcSiteNameMaxLen まで許してキュー修飾しても riverQueueNameMaxLen を
// 超えないことを検査する。破る典型は siteBoundQueueNames に `reconciler` より
// 長い論理名を足す、または config.MirakcSiteNameMaxLen を大きくすること。
func TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen(t *testing.T) {
	maxLenSite := strings.Repeat("a", config.MirakcSiteNameMaxLen)
	for _, base := range siteBoundQueueNames {
		name := qualifyQueueName(base, maxLenSite)
		if len(name) > riverQueueNameMaxLen {
			t.Errorf("qualifyQueueName(%q, <%d-char site>) = %q (%d chars) exceeds riverQueueNameMaxLen(%d)",
				base, len(maxLenSite), name, len(name), riverQueueNameMaxLen)
		}
	}
}

type noOpArgs struct{}

func (noOpArgs) Kind() string { return "noop" }

type noOpWorker struct {
	river.WorkerDefaults[noOpArgs]
}

func (w *noOpWorker) Work(_ context.Context, _ *river.Job[noOpArgs]) error { return nil }

type epgSyncNoOpWorker struct {
	river.WorkerDefaults[EpgSyncArgs]
}

func (w *epgSyncNoOpWorker) Work(_ context.Context, _ *river.Job[EpgSyncArgs]) error { return nil }
