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

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

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

// EpgSyncSite を指定すると epg_sync が定期ジョブとして投入され、
// 登録済みワーカーが epg キューで拾うこと（配線の確認）。
func TestEpgSyncPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	// mirakc を叩かせずに配線だけ見たいので、/api/services で失敗させる。
	// ジョブが失敗すれば「投入されてワーカーに届いた」ことは確認できる。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil)})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:    true,
		EpgSyncSite:     "default",
		EpgSyncInterval: time.Hour, // RunOnStart で 1 回だけ走らせる
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobFailed)
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
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the periodic epg_sync job")
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

	workers := NewWorkers(&Deps{Pool: pool, MirakcClient: mirakc.NewClient(srv.URL, nil)})
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

// resolveStallTimeout は「設定あり → 注入値」「設定なし（0）→ 既定 30 秒」の両方向。
// ingest.stall_timeout を cmd から注入する経路（issue #57）の受け入れ基準。
func TestIngestWorker_ResolveStallTimeout(t *testing.T) {
	t.Run("unset uses default", func(t *testing.T) {
		w := &IngestWorker{}
		if got := w.resolveStallTimeout(); got != defaultStallTimeout {
			t.Errorf("resolveStallTimeout() = %v, want default %v", got, defaultStallTimeout)
		}
	})
	t.Run("configured value is used", func(t *testing.T) {
		want := 2 * time.Minute
		w := &IngestWorker{StallTimeout: want}
		if got := w.resolveStallTimeout(); got != want {
			t.Errorf("resolveStallTimeout() = %v, want %v", got, want)
		}
	})
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

	// 完了させると再度投入できる
	if _, err := pool.Exec(ctx,
		"UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1", first.Job.ID,
	); err != nil {
		t.Fatalf("marking completed: %v", err)
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

// RulerPassSite を指定すると ruler_pass が定期ジョブとして投入され、
// 登録済みワーカーが ruler キューで拾うこと（配線の確認）。
func TestRulerPassPeriodicJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers(&Deps{Pool: pool})
	client, err := NewClient(pool, workers, ClientConfig{
		PeriodicJobs:      true,
		RulerPassSite:     "default",
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

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "ruler_pass" {
			t.Errorf("job kind = %q, want %q", event.Job.Kind, "ruler_pass")
		}
		if event.Job.Queue != rulerQueue {
			t.Errorf("job queue = %q, want %q", event.Job.Queue, rulerQueue)
		}
		var args RulerPassArgs
		if err := json.Unmarshal(event.Job.EncodedArgs, &args); err != nil {
			t.Fatalf("unmarshalling job args: %v", err)
		}
		if args.Site != "default" {
			t.Errorf("job args site = %q, want %q", args.Site, "default")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the periodic ruler_pass job")
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

	if _, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{BoundSite: "tokyo"}); err != nil {
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
	if !strings.Contains(logged, "bound_site=tokyo") {
		t.Errorf("log output does not mention bound_site=tokyo; log:\n%s", logged)
	}
}

// **SoftStopTimeout が未設定（0）でも river.Config に非 0 が載ること。**
//
// これは「設定し忘れ」が最も危険な側に倒れないための既定である。River は
// SoftStopTimeout が 0 のとき work ctx を start ctx から継ぐので
// （river@v0.40.0/client.go の workParentCtx）、0 のまま渡すと
// `signal.NotifyContext` の ctx を Start に渡している構成で **SIGTERM が
// 実行中のジョブを即座に打ち切る**。0 は「無制限」ではなく「待たない」である。
//
// SIGTERM が実際に drain になることは cmd/rokuban の
// TestServerCmd_SigtermDrainsRunningJob が実 DB で測る。ここでは
// 「0 を素通しさせない」ことだけを固定する。
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

// worker.periodic_jobs: false のとき、EpgSyncSite / RulerPassSite / ReconcilePassSite が
// 設定されていても PeriodicJobs が登録されないこと。
func TestBuildRiverConfig_PeriodicJobsDisabled(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:      false,
		EpgSyncSite:       "default",
		RulerPassSite:     "default",
		ReconcilePassSite: "default",
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 0 {
		t.Errorf("PeriodicJobs = %d 件, want 0 (worker.periodic_jobs=false のとき登録されないこと)",
			len(riverCfg.PeriodicJobs))
	}
}

// worker.periodic_jobs: true なら epg_sync / ruler_pass / reconcile_pass の全部が登録されること。
func TestBuildRiverConfig_PeriodicJobsEnabled(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		PeriodicJobs:      true,
		EpgSyncSite:       "default",
		RulerPassSite:     "default",
		ReconcilePassSite: "default",
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if len(riverCfg.PeriodicJobs) != 3 {
		t.Errorf("PeriodicJobs = %d 件, want 3 (epg_sync + ruler_pass + reconcile_pass)", len(riverCfg.PeriodicJobs))
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

// ValidateSiteForQueueNames は、site 修飾後のキュー名が River の 64 文字上限
// （riverQueueNameMaxLen）を超えると起動時エラーにする。config.MirakcSiteNameMaxLen
// はこの上限を見込んで既に 53 に締められている（TestSiteBoundQueueNames_
// FitWithinMirakcSiteNameMaxLen 参照）が、ValidateSiteForQueueNames は site 名が
// config 以外の経路から来る場合の最後の砦として独立に検査する。
func TestValidateSiteForQueueNames(t *testing.T) {
	t.Run("short site name is fine", func(t *testing.T) {
		if err := ValidateSiteForQueueNames("tokyo"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("53-char site name (the boundary for the longest prefix, reconciler_) is fine", func(t *testing.T) {
		// River の上限は 64 文字（issue #185 が固定する値そのもの。riverQueueNameMaxLen
		// を参照するとこの値自体の変化を検出できなくなるため、ここは意図的にリテラルで
		// 書く）。"reconciler_" は 11 文字なので、64 - 11 = 53 文字まではちょうど収まる。
		site := strings.Repeat("a", 53)
		if err := ValidateSiteForQueueNames(site); err != nil {
			t.Errorf("unexpected error for %d-char site name: %v", len(site), err)
		}
	})

	t.Run("54-char site name (one over the boundary) is an error", func(t *testing.T) {
		site := strings.Repeat("a", 54)
		err := ValidateSiteForQueueNames(site)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "reconciler_") {
			t.Errorf("error = %v, want mention of the offending queue name", err)
		}
	})

	t.Run("a 64-char site name is rejected once qualified (config no longer permits it; this is the non-config last line of defence)", func(t *testing.T) {
		site := strings.Repeat("a", 64)
		if err := ValidateSiteForQueueNames(site); err == nil {
			t.Fatal("expected error for a 64-char site name once queue-qualified, got nil")
		}
	})
}

// TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen は、
// config.MirakcSiteNameMaxLen が ValidateSiteForQueueNames の上位集合であることを
// 機械的に固定する。config は worker を import できない（逆方向のみ許される）ので、
// 両方が見える worker 側にこの関係のテストを置く。
//
// siteBoundQueueNames のどの論理名についても、site 名を
// config.MirakcSiteNameMaxLen まで許してキュー修飾しても riverQueueNameMaxLen を
// 超えないことを検査する。破ると（siteBoundQueueNames に `reconciler` より
// 長い論理名を足す、または config.MirakcSiteNameMaxLen を大きくする）、config の
// ロード時検査を通った site 名が ValidateSiteForQueueNames で落ちる、つまり
// 「起動できるはずの設定が、束縛した瞬間に初めて起動エラーになる」状態に戻る。
func TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen(t *testing.T) {
	for _, base := range siteBoundQueueNames {
		qualifiedLen := len(base) + 1 + config.MirakcSiteNameMaxLen
		if qualifiedLen > riverQueueNameMaxLen {
			t.Errorf("queue %q: len(%q)=%d + 1(separator) + config.MirakcSiteNameMaxLen(%d) = %d exceeds riverQueueNameMaxLen(%d)",
				base, base, len(base), config.MirakcSiteNameMaxLen, qualifiedLen, riverQueueNameMaxLen)
		}
	}
}

type noOpArgs struct{}

func (noOpArgs) Kind() string { return "noop" }

type noOpWorker struct {
	river.WorkerDefaults[noOpArgs]
}

func (w *noOpWorker) Work(_ context.Context, _ *river.Job[noOpArgs]) error { return nil }
