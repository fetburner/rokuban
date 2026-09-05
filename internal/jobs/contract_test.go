package jobs

import (
	"reflect"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestJobArgsContract(t *testing.T) {
	pending := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}

	tests := []struct {
		name    string
		args    river.JobArgs
		kind    string
		queue   string
		byQueue bool
	}{
		{name: "ingest", args: IngestJobArgs{Site: "tokyo", RecordID: "rec-1"}, kind: "ingest", queue: "ingest_tokyo", byQueue: true},
		{name: "epg_sync", args: EpgSyncArgs{Site: "tokyo"}, kind: "epg_sync", queue: "epg_tokyo", byQueue: true},
		{name: "tuner_sync", args: TunerSyncArgs{Site: "tokyo"}, kind: "tuner_sync", queue: "epg_tokyo", byQueue: true},
		{name: "ruler_pass", args: RulerPassArgs{Site: "tokyo"}, kind: "ruler_pass", queue: "ruler"},
		{name: "reconcile_pass", args: ReconcilePassArgs{Site: "tokyo"}, kind: "reconcile_pass", queue: "reconciler_tokyo", byQueue: true},
		{name: "record_sweep", args: RecordSweepArgs{Site: "tokyo"}, kind: "record_sweep", queue: "watcher_tokyo", byQueue: true},
		{name: "encode", args: EncodeJobArgs{RecordingID: 1, Profile: "mobile"}, kind: "encode", queue: "encode"},
		{name: "encode_enqueue_hint", args: EncodeEnqueueHintArgs{RecordingID: 1}, kind: "encode_enqueue_hint", queue: "encode"},
		{name: "thumbnail", args: ThumbnailJobArgs{RecordingID: 1}, kind: "thumbnail", queue: "thumbnail"},
		{name: "encode_reconcile", args: EncodeReconcileArgs{}, kind: "encode_reconcile", queue: "encode"},
		{name: "delete_reconcile", args: DeleteReconcileArgs{}, kind: "delete_reconcile", queue: "cleanup", byQueue: true},
		{name: "catalog_export", args: CatalogExportArgs{Keep: 3}, kind: "catalog_export", queue: "cleanup", byQueue: true},
		{name: "storage_sync", args: StorageSyncArgs{}, kind: "storage_sync", queue: "storage", byQueue: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.args.Kind(); got != tt.kind {
				t.Fatalf("Kind() = %q, want %q", got, tt.kind)
			}

			argsWithOpts, ok := tt.args.(river.JobArgsWithInsertOpts)
			if !ok {
				t.Fatal("job args do not implement river.JobArgsWithInsertOpts")
			}
			got := argsWithOpts.InsertOpts()
			if got.Queue != tt.queue {
				t.Errorf("InsertOpts().Queue = %q, want %q", got.Queue, tt.queue)
			}
			wantUnique := river.UniqueOpts{
				ByArgs:  true,
				ByQueue: tt.byQueue,
				ByState: pending,
			}
			if !reflect.DeepEqual(got.UniqueOpts, wantUnique) {
				t.Errorf("InsertOpts().UniqueOpts = %#v, want %#v", got.UniqueOpts, wantUnique)
			}
		})
	}
}
