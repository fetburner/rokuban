package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

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
		if event.Job.Queue != epgQueue {
			t.Errorf("job queue = %q, want %q", event.Job.Queue, epgQueue)
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
// ここが誤ると、site 単位のジョブ（ingest/epg/ruler/reconciler/watcher）を
// 引く worker が空文字列 site のまま起動し、届いたジョブの site と一致せず
// 全滅して再試行し続ける（ログにも出ない）。
func TestRequiresSiteBinding(t *testing.T) {
	tests := []struct {
		name   string
		queues []string
		want   bool
	}{
		{"empty means all queues, including site-bound ones", nil, true},
		{"explicit ingest", []string{ingestQueue}, true},
		{"explicit epg", []string{epgQueue}, true},
		{"explicit ruler", []string{rulerQueue}, true},
		{"explicit reconciler", []string{reconcilerQueue}, true},
		{"explicit watcher (record_sweep)", []string{recordSweepQueue}, true},
		{"encode/thumbnail only excludes site-bound queues", []string{encodeQueue, thumbnailQueue}, false},
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

type noOpArgs struct{}

func (noOpArgs) Kind() string { return "noop" }

type noOpWorker struct {
	river.WorkerDefaults[noOpArgs]
}

func (w *noOpWorker) Work(_ context.Context, _ *river.Job[noOpArgs]) error { return nil }
