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

func TestQueueContract(t *testing.T) {
	if got, want := AllQueueNames(), []string{
		"cleanup",
		"default",
		"encode",
		"epg",
		"ingest",
		"reconciler",
		"ruler",
		"storage",
		"thumbnail",
		"watcher",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("AllQueueNames() = %#v, want %#v", got, want)
	}

	for _, tt := range []struct {
		base string
		site string
		want string
	}{
		{base: "ingest", site: "tokyo", want: "ingest_tokyo"},
		{base: "ingest", site: "", want: "ingest_default"},
	} {
		if got := QualifyQueueName(tt.base, tt.site); got != tt.want {
			t.Errorf("QualifyQueueName(%q, %q) = %q, want %q", tt.base, tt.site, got, tt.want)
		}
	}

	for _, tt := range []struct {
		logical string
		site    string
		want    string
	}{
		{logical: IngestQueue, site: "tokyo", want: "ingest_tokyo"},
		{logical: EpgQueue, site: "tokyo", want: "epg_tokyo"},
		{logical: ReconcilerQueue, site: "tokyo", want: "reconciler_tokyo"},
		{logical: RecordSweepQueue, site: "tokyo", want: "watcher_tokyo"},
		{logical: RulerQueue, site: "tokyo", want: "ruler"},
		{logical: CleanupQueue, site: "tokyo", want: "cleanup"},
	} {
		if got := PhysicalQueueName(tt.logical, tt.site); got != tt.want {
			t.Errorf("PhysicalQueueName(%q, %q) = %q, want %q", tt.logical, tt.site, got, tt.want)
		}
	}
}

func TestQueueRequirements(t *testing.T) {
	tests := []struct {
		name          string
		queues        []string
		needsEncode   bool
		needsSiteBind bool
	}{
		{name: "all", queues: nil, needsEncode: true, needsSiteBind: true},
		{name: "encode", queues: []string{EncodeQueue}, needsEncode: true},
		{name: "thumbnail", queues: []string{ThumbnailQueue}, needsEncode: true},
		{name: "ruler only", queues: []string{RulerQueue}, needsSiteBind: false},
		{name: "site queue", queues: []string{IngestQueue}, needsSiteBind: true},
		{name: "cleanup and storage", queues: []string{CleanupQueue, StorageQueue}, needsSiteBind: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresEncodeTools(tt.queues); got != tt.needsEncode {
				t.Errorf("RequiresEncodeTools(%v) = %t, want %t", tt.queues, got, tt.needsEncode)
			}
			if got := RequiresSiteBinding(tt.queues); got != tt.needsSiteBind {
				t.Errorf("RequiresSiteBinding(%v) = %t, want %t", tt.queues, got, tt.needsSiteBind)
			}
		})
	}
}
