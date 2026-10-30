package jobs

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestJobArgsSpecAndPhysicalQueue(t *testing.T) {
	tests := []struct {
		name          string
		args          jobq.Args
		kind          string
		logicalQueue  string
		physicalQueue string
		timeout       time.Duration
		maxAttempts   int
		coalesce      bool
		priority      int
	}{
		{"ingest", IngestJobArgs{Site: "tokyo", RecordID: "rec-1"}, "ingest", IngestQueue, "ingest_tokyo", 5 * time.Minute, 0, true, 1},
		{"epg_sync", EpgSyncArgs{Site: "tokyo"}, "epg_sync", EpgQueue, "epg_tokyo", 10 * time.Minute, 1, true, 1},
		{"tuner_sync", TunerSyncArgs{Site: "tokyo"}, "tuner_sync", EpgQueue, "epg_tokyo", time.Minute, 1, true, 1},
		{"ruler_pass", RulerPassArgs{Site: "tokyo"}, "ruler_pass", RulerQueue, "ruler", 5 * time.Minute, 1, true, 1},
		{"reconcile_pass", ReconcilePassArgs{Site: "tokyo"}, "reconcile_pass", ReconcilerQueue, "reconciler_tokyo", 10 * time.Minute, 1, true, 1},
		{"record_sweep", RecordSweepArgs{Site: "tokyo"}, "record_sweep", RecordSweepQueue, "watcher_tokyo", 10 * time.Minute, 1, true, 1},
		{"encode", EncodeJobArgs{RecordingID: 1, Profile: "mobile", Timeout: 2 * time.Hour}, "encode", EncodeQueue, "encode", 2 * time.Hour, 26, true, 1},
		{"thumbnail", ThumbnailJobArgs{RecordingID: 1}, "thumbnail", ThumbnailQueue, "thumbnail", 5 * time.Minute, 0, true, 1},
		{"seek_tiles", SeekTilesJobArgs{RecordingID: 1}, "seek_tiles", ThumbnailQueue, "thumbnail", 15 * time.Minute, 0, true, 4},
		{"thumbnail_reconcile", ThumbnailReconcileArgs{
			AfterRecordingID: 101, AfterReselectRecordingID: 202, AfterSeekTilesRecordingID: 303,
			MissingDone: true, SeekTilesDone: true,
		}, "thumbnail_reconcile", ThumbnailQueue, "thumbnail", 5 * time.Minute, 1, true, 1},
		{"cm_detect", CMDetectJobArgs{RecordingID: 1, RecordingDurationMs: 20 * 60 * 1000}, "cm_detect", CMDetectQueue, "cm_detect", 40 * time.Minute, 10, true, 1},
		{"cm_detect_reconcile", CMDetectReconcileArgs{}, "cm_detect_reconcile", CMDetectQueue, "cm_detect", 5 * time.Minute, 1, true, 1},
		{"cm_logo_candidate", CMLogoCandidateJobArgs{RecordingID: 1, RecordingDurationMs: 20 * 60 * 1000}, "cm_logo_candidate", CMDetectQueue, "cm_detect", 40 * time.Minute, 1, true, 1},
		{"label_rule_reconcile", LabelRuleReconcileArgs{}, "label_rule_reconcile", CleanupQueue, "cleanup", time.Minute, 1, false, 1},
		{"encode_reconcile", EncodeReconcileArgs{}, "encode_reconcile", EncodeQueue, "encode", 5 * time.Minute, 1, true, 1},
		{"delete_reconcile", DeleteReconcileArgs{}, "delete_reconcile", CleanupQueue, "cleanup", 10 * time.Minute, 1, true, 1},
		{"catalog_export", CatalogExportArgs{Keep: 3}, "catalog_export", CleanupQueue, "cleanup", 5 * time.Minute, 1, true, 1},
		{"storage_sync", StorageSyncArgs{}, "storage_sync", StorageQueue, "storage", time.Minute, 1, true, 1},
		{"storage_transfer_reconcile", StorageTransferReconcileArgs{}, "storage_transfer_reconcile", StorageQueue, "storage", 5 * time.Minute, 1, true, 1},
		{"storage_transfer", StorageTransferArgs{MediaAssetID: 9, Store: "tokyo", PolicyFingerprint: "abc"}, "storage_transfer", TransferQueue, "transfer_tokyo", 12 * time.Hour, 1, true, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.args.Spec()
			if got := tt.args.Kind(); got != tt.kind {
				t.Fatalf("Kind() = %q, want %q", got, tt.kind)
			}
			if spec.Queue != tt.logicalQueue {
				t.Errorf("Spec().Queue = %q, want %q", spec.Queue, tt.logicalQueue)
			}
			if spec.Timeout == nil {
				t.Fatal("Spec().Timeout is nil")
			}
			if got := spec.Timeout(tt.args); got != tt.timeout {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.timeout)
			}
			if spec.MaxAttempts != tt.maxAttempts {
				t.Errorf("Spec().MaxAttempts = %d, want literal %d", spec.MaxAttempts, tt.maxAttempts)
			}
			if spec.Coalesce != tt.coalesce {
				t.Errorf("Spec().Coalesce = %v, want %v", spec.Coalesce, tt.coalesce)
			}
			if spec.Priority != tt.priority {
				t.Errorf("Spec().Priority = %d, want literal %d", spec.Priority, tt.priority)
			}

			gotPhysicalQueue, err := PhysicalQueueFor(tt.args)
			if err != nil {
				t.Fatalf("PhysicalQueueFor(args): %v", err)
			}
			if gotPhysicalQueue != tt.physicalQueue {
				t.Errorf("PhysicalQueueFor(args) = %q, want literal %q", gotPhysicalQueue, tt.physicalQueue)
			}

			// JSON を経由しても Args が変わらない（json:"-" で Timeout 等が落ちない）ことを固定する。
			raw, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("json.Marshal(%T): %v", tt.args, err)
			}
			ptr := reflect.New(reflect.TypeOf(tt.args))
			if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
				t.Fatalf("json.Unmarshal(%s) into %T: %v", raw, tt.args, err)
			}
			rt, ok := ptr.Elem().Interface().(jobq.Args)
			if !ok {
				t.Fatalf("%T is not jobq.Args after round trip", ptr.Elem().Interface())
			}
			if !reflect.DeepEqual(rt, tt.args) {
				t.Errorf("JSON round trip of %T = %+v, want %+v (json %s)", tt.args, rt, tt.args, raw)
			}
			if got := rt.Spec().Timeout(rt); got != tt.timeout {
				t.Errorf("Spec().Timeout after round trip of %T = %s, want literal %s", tt.args, got, tt.timeout)
			}
		})
	}
}
