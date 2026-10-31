package riveradapter

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/testutil"
)

const cancelTestQueue = "cancel_test_queue"

// cancelTestArgs は取り消しのテスト用。Seq を変えると別のジョブになる。
type cancelTestArgs struct {
	Seq int `json:"seq"`
}

func (cancelTestArgs) Kind() string { return "cancel_test" }

func (cancelTestArgs) Spec() jobq.Spec {
	return jobq.Spec{Queue: cancelTestQueue, Timeout: func(jobq.Args) time.Duration { return time.Minute }}
}

// cancelTestWorker は最初の Work を release が閉じられるまで返さない。started に 1 度知らせる。
// JobCancel が実行中のジョブに掛かると、River の契約どおりジョブの context が閉じられるので、
// そのとき interrupted を閉じる。context を無視して release まで走り続けるのは、
// 取り消しに応じない実行中のジョブ（取り消しが終わらない原因になった形）を再現するため。
type cancelTestWorker struct {
	started     chan struct{}
	release     chan struct{}
	interrupted chan struct{}
}

func (w *cancelTestWorker) Work(ctx context.Context, _ cancelTestArgs) error {
	select {
	case w.started <- struct{}{}:
	default:
	}
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		close(w.interrupted)
	}
	<-w.release
	return nil
}

// TestCancelQueued_RunningJobIsReportedNotCancelled は、実行中のジョブが 1 件あり、待機中のジョブが 3 件あるとき、
// CancelQueued が期限内に戻り、待機中の 3 件だけを取り消し、実行中の 1 件を running として報告することを確かめる。
//
// 変異の確認: pendingJobStates（running を含む）で列挙していた旧実装では、実行中のジョブが一覧に戻り続けて
// 期限まで回り、err が返る。
func TestCancelQueued_RunningJobIsReportedNotCancelled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	registry := jobq.NewRegistry()
	worker := &cancelTestWorker{
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
		interrupted: make(chan struct{}),
	}
	Register(registry, worker)
	// MaxWorkers 1 なので、1 件が実行中の間は残りが available のまま残る。
	spec := jobq.WorkerSpec{Queues: []jobq.QueueSpec{plainQueue(cancelTestQueue, 1)}}
	adapter, err := NewAdapter(pool, registry, logicalQueueFor, Config{Spec: spec})
	if err != nil {
		t.Fatalf("creating adapter: %v", err)
	}
	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()
	if err := adapter.Start(clientCtx); err != nil {
		t.Fatalf("starting adapter: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if err := adapter.Stop(stopCtx); err != nil {
			t.Errorf("stopping adapter: %v", err)
		}
	}()
	// Stop より後に実行されるので、worker を返して Stop が待てるようにする。
	defer close(worker.release)

	if _, err := adapter.Enqueue(ctx, cancelTestArgs{Seq: 0}); err != nil {
		t.Fatalf("enqueue running job: %v", err)
	}
	select {
	case <-worker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never started the first job")
	}
	for seq := 1; seq <= 3; seq++ {
		if _, err := adapter.Enqueue(ctx, cancelTestArgs{Seq: seq}); err != nil {
			t.Fatalf("enqueue pending job %d: %v", seq, err)
		}
	}

	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	start := time.Now()
	cancelled, running, err := adapter.CancelQueued(callCtx, []QueuedJobSelector{{Queue: cancelTestQueue}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CancelQueued: %v (after %v; a running job must not keep the loop going until the deadline)", err, elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("CancelQueued took %v, want it to return promptly", elapsed)
	}
	if cancelled != 3 {
		t.Errorf("cancelled = %d, want 3 (the pending jobs)", cancelled)
	}
	if running != 1 {
		t.Errorf("running = %d, want 1 (the job that is still executing)", running)
	}
	// 実行中のジョブに JobCancel を掛けると、その context が閉じられる。CancelQueued は掛けてはならない。
	// 閉じられるのは通知経由で非同期なので、少し待ってから確かめる。
	select {
	case <-worker.interrupted:
		t.Error("CancelQueued interrupted the running job; it must leave running jobs alone")
	case <-time.After(500 * time.Millisecond):
	}

	states := map[string]int{}
	for _, row := range riveradaptertest.MustListRiverJobsOfKind(t, ctx, pool, "cancel_test") {
		states[string(row.State)]++
	}
	if states["cancelled"] != 3 || states["running"] != 1 || len(states) != 2 {
		t.Errorf("job states = %v, want cancelled=3 and running=1 only", states)
	}
}
