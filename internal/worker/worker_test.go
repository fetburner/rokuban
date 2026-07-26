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

type noOpArgs struct{}

func (noOpArgs) Kind() string { return "noop" }

type noOpWorker struct {
	river.WorkerDefaults[noOpArgs]
}

func (w *noOpWorker) Work(_ context.Context, _ *river.Job[noOpArgs]) error { return nil }
