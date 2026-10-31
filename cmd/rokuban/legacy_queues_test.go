package main

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/tsscan"
)

// 旧キューの一覧は、いまも置き場で修飾する論理キューである（修飾の無い名前を誰も購読しない前提）。
func TestLegacyQueuesAreStoreBoundToday(t *testing.T) {
	for _, queue := range legacyUnqualifiedQueues {
		if !jobs.IsStoreBound(queue) {
			t.Errorf("legacy queue %q is not store-bound any more; it may still have a subscriber, so it must not be cancelled", queue)
		}
	}
	if got, want := slices.Clone(legacyUnqualifiedQueues), []string{"encode", "thumbnail", "cm_detect", "ts_scan", "storage"}; !slices.Equal(got, want) {
		t.Errorf("legacy queues = %v, want literal %v", got, want)
	}
}

// legacyQueueFor は置き場で修飾する前の投入先（論理名のまま）を、種別ごとのリテラルで返す。
func legacyQueueFor(args jobq.Args) (string, error) {
	return map[string]string{
		"encode":                     "encode",
		"thumbnail_reconcile":        "thumbnail",
		"cm_detect_reconcile":        "cm_detect",
		"ts_scan_reconcile":          "ts_scan",
		"storage_sync":               "storage",
		"delete_reconcile":           "cleanup",
		"catalog_export":             "cleanup",
		"label_rule_reconcile":       "cleanup",
		"encode_reconcile":           "encode",
		"storage_transfer_reconcile": "storage",
	}[args.Kind()], nil
}

func queuedStates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) []string {
	t.Helper()
	var states []string
	for _, row := range riveradaptertest.MustListRiverJobsOfKind(t, ctx, pool, kind) {
		states = append(states, row.Queue+":"+string(row.State))
	}
	slices.Sort(states)
	return states
}

// 置き場で修飾する前に積まれたジョブは、取り消され、同じ引数の新しいジョブは旧キューの残骸と合流しない。
// River の一意性の鍵にキューが入っているので、旧キューの未完了のジョブが新しい投入を握り潰さない。
func TestCancelLegacyQueuesAndNewJobsDoNotCoalesceWithTheLeftovers(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	legacy, err := riveradapter.NewInsertOnlyEnqueuer(pool, legacyQueueFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []jobq.Args{
		jobs.EncodeJobArgs{RecordingID: 1, Profile: "h264"}, jobs.ThumbnailReconcileArgs{}, jobs.CMDetectReconcileArgs{},
		tsscan.ReconcileArgs{}, jobs.StorageSyncArgs{}, jobs.DeleteReconcileArgs{}, jobs.CatalogExportArgs{},
		jobs.LabelRuleReconcileArgs{}, jobs.EncodeReconcileArgs{},
	} {
		if _, err := legacy.Enqueue(ctx, args); err != nil {
			t.Fatalf("inserting a legacy %s job: %v", args.Kind(), err)
		}
	}

	// 同じ引数で、いまの投入先へ積む。旧キューの未完了のジョブと合流してはならない。
	current, err := newInsertOnlyEnqueuer(pool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := current.Enqueue(ctx, jobs.EncodeReconcileArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Coalesced {
		t.Fatal("encode_reconcile in the cleanup queue coalesced with the leftover in the legacy encode queue")
	}

	var out bytes.Buffer
	if err := runCancelLegacyQueues(ctx, pool, &out); err != nil {
		t.Fatalf("runCancelLegacyQueues: %v", err)
	}
	// encode, thumbnail_reconcile, cm_detect_reconcile, ts_scan_reconcile, storage_sync, encode_reconcile(旧 encode)、
	// delete_reconcile, catalog_export（旧 cleanup）の 8 件。label_rule_reconcile と、いまの cleanup の encode_reconcile は残る。
	if got, want := out.String(), "cancelled 8 job(s) in the legacy unqualified queues\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	for kind, want := range map[string][]string{
		"encode":               {"encode:cancelled"},
		"thumbnail_reconcile":  {"thumbnail:cancelled"},
		"cm_detect_reconcile":  {"cm_detect:cancelled"},
		"ts_scan_reconcile":    {"ts_scan:cancelled"},
		"storage_sync":         {"storage:cancelled"},
		"delete_reconcile":     {"cleanup:cancelled"},
		"catalog_export":       {"cleanup:cancelled"},
		"label_rule_reconcile": {"cleanup:available"},
		"encode_reconcile":     {"cleanup:available", "encode:cancelled"},
	} {
		if got := queuedStates(t, ctx, pool, kind); !slices.Equal(got, want) {
			t.Errorf("%s jobs = %v, want literal %v", kind, got, want)
		}
	}

	// 取り消した後は、同じ引数の新しい投入が合流せずに入る。2 回目の実行は何も取り消さない。
	out.Reset()
	if err := runCancelLegacyQueues(ctx, pool, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "cancelled 0 job(s) in the legacy unqualified queues\n"; got != want {
		t.Fatalf("second run output = %q, want %q", got, want)
	}
}

// blockingEncodeWorker は release が閉じられるまで Work を返さない。started に 1 度知らせる。
// 旧キュー encode に載ったジョブを、取り消せない実行中の状態にしておくために使う。
type blockingEncodeWorker struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingEncodeWorker) Work(context.Context, jobs.EncodeJobArgs) error {
	select {
	case w.started <- struct{}{}:
	default:
	}
	<-w.release
	return nil
}

// 実行中の旧キューのジョブが残っていると、cancel-legacy は取り消さず、再実行を促す行を出す。
// 実行中が無ければその行は出ない。
//
// 変異確認: runCancelLegacyQueues の `if running > 0` を `if running >= 0` に変えると、
// 実行中が無いケースで行が出て落ちる。`if running < 0` に変えると、実行中があるケースで行が消えて落ちる。
func TestCancelLegacyQueuesReportsRunningJobsAndOmitsTheLineWithout(t *testing.T) {
	const runningLine = "1 job(s) are still running and were not cancelled; run cancel-legacy again after they finish\n"
	const cancelledLine = "cancelled 0 job(s) in the legacy unqualified queues\n"

	t.Run("no running job", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		var out bytes.Buffer
		if err := runCancelLegacyQueues(context.Background(), pool, &out); err != nil {
			t.Fatalf("runCancelLegacyQueues: %v", err)
		}
		if got, want := out.String(), cancelledLine; got != want {
			t.Fatalf("output = %q, want %q (no running line without a running job)", got, want)
		}
	})

	t.Run("one running job", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		ctx := context.Background()

		registry := jobq.NewRegistry()
		worker := &blockingEncodeWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
		riveradapter.Register(registry, worker)
		// 旧キュー encode を、修飾の無い名前のまま購読する（置き場で修飾する前の worker と同じ形）。
		spec := jobq.WorkerSpec{Queues: []jobq.QueueSpec{{Logical: jobs.EncodeQueue, Physical: []string{jobs.EncodeQueue}, MaxWorkers: 1}}}
		adapter, err := riveradapter.NewAdapter(pool, registry, legacyQueueFor, riveradapter.Config{Spec: spec})
		if err != nil {
			t.Fatalf("creating adapter: %v", err)
		}
		if err := adapter.Start(ctx); err != nil {
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

		if _, err := adapter.Enqueue(ctx, jobs.EncodeJobArgs{RecordingID: 1, Profile: "h264"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		select {
		case <-worker.started:
		case <-time.After(10 * time.Second):
			t.Fatal("the worker never started the legacy encode job")
		}

		var out bytes.Buffer
		if err := runCancelLegacyQueues(ctx, pool, &out); err != nil {
			t.Fatalf("runCancelLegacyQueues: %v", err)
		}
		if got, want := out.String(), cancelledLine+runningLine; got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})
}
