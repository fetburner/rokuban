package jobs

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestEncodeSpecTimeoutUsesSavedDeadlineOrFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		args EncodeJobArgs
		want time.Duration
	}{
		{name: "saved deadline", args: EncodeJobArgs{Timeout: 90 * time.Minute}, want: 90 * time.Minute},
		{name: "old args without deadline", args: EncodeJobArgs{}, want: 12 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Spec().Timeout(tt.args); got != tt.want {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.want)
			}
		})
	}
}

func TestCMDetectionSpecsUseRecordingDuration(t *testing.T) {
	for _, tt := range []struct {
		name string
		args jobq.Args
		want time.Duration
	}{
		{"CM detection zero duration", CMDetectJobArgs{}, 30 * time.Minute},
		{"CM detection known duration", CMDetectJobArgs{RecordingDurationMs: 20 * 60 * 1000}, 40 * time.Minute},
		{"logo candidate zero duration", CMLogoCandidateJobArgs{}, 30 * time.Minute},
		{"logo candidate known duration", CMLogoCandidateJobArgs{RecordingDurationMs: 20 * 60 * 1000}, 40 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Spec().Timeout(tt.args); got != tt.want {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.want)
			}
		})
	}
}

// River's projection explicitly excludes finalized states from its current duplicate filter.
func TestRiverProjectionLimitsCoalescingToUnfinishedStates(t *testing.T) {
	for _, tt := range []struct {
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			states := tt.opts.UniqueOpts.ByState
			if len(states) == 0 {
				t.Fatal("coalescing projection omitted the explicit state list")
			}
			for _, state := range states {
				switch state {
				case rivertype.JobStateCompleted, rivertype.JobStateDiscarded, rivertype.JobStateCancelled:
					t.Errorf("finalized state %q is in River's current coalescing filter", state)
				}
			}
			if !slices.Contains(states, rivertype.JobStateRunning) {
				t.Error("running is missing from River's current coalescing filter")
			}
		})
	}
}

func TestRiverProjectionIncludesQueueWhenCoalescing(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts river.InsertOpts
	}{
		{"ingest", IngestJobArgs{}.InsertOpts()},
		{"epg_sync", EpgSyncArgs{}.InsertOpts()},
		{"tuner_sync", TunerSyncArgs{}.InsertOpts()},
		{"ruler_pass", RulerPassArgs{}.InsertOpts()},
		{"reconcile_pass", ReconcilePassArgs{}.InsertOpts()},
		{"record_sweep", RecordSweepArgs{}.InsertOpts()},
		{"encode", EncodeJobArgs{}.InsertOpts()},
		{"thumbnail", ThumbnailJobArgs{}.InsertOpts()},
		{"seek_tiles", SeekTilesJobArgs{}.InsertOpts()},
		{"thumbnail_reconcile", ThumbnailReconcileArgs{}.InsertOpts()},
		{"cm_detect", CMDetectJobArgs{}.InsertOpts()},
		{"cm_detect_reconcile", CMDetectReconcileArgs{}.InsertOpts()},
		{"cm_logo_candidate", CMLogoCandidateJobArgs{}.InsertOpts()},
		{"encode_reconcile", EncodeReconcileArgs{}.InsertOpts()},
		{"encode_rebuild", EncodeRebuildArgs{}.InsertOpts()},
		{"delete_reconcile", DeleteReconcileArgs{}.InsertOpts()},
		{"catalog_export", CatalogExportArgs{}.InsertOpts()},
		{"storage_sync", StorageSyncArgs{}.InsertOpts()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.opts.UniqueOpts.ByQueue {
				t.Error("River projection must include the physical queue in its duplicate key")
			}
		})
	}
	if got := (LabelRuleReconcileArgs{}).InsertOpts().UniqueOpts; !reflect.DeepEqual(got, river.UniqueOpts{}) {
		t.Errorf("non-coalescing label rule projection = %+v, want zero UniqueOpts", got)
	}
}

func TestSeekTilesPriorityComesFromSpec(t *testing.T) {
	thumb := ThumbnailJobArgs{}.Spec()
	tiles := SeekTilesJobArgs{}.Spec()
	if tiles.Queue != ThumbnailQueue {
		t.Fatalf("seek_tiles queue = %q, want literal %q", tiles.Queue, "thumbnail")
	}
	if tiles.Priority != 4 || thumb.Priority != 1 {
		t.Fatalf("Spec priorities = tiles:%d thumbnail:%d, want literal 4 and 1", tiles.Priority, thumb.Priority)
	}
	if got := (SeekTilesJobArgs{}).InsertOpts().Priority; got != 4 {
		t.Errorf("River priority = %d, want literal 4", got)
	}
}
