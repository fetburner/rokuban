package main

import (
	"bytes"
	"context"
	"slices"
	"testing"

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
