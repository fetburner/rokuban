package worker

import "github.com/fetburner/rokuban/internal/jobs"

// The worker package tests exercise handlers using the job names that were
// historically declared in this package. Keep test-local aliases so the
// contract can move to internal/jobs without rewriting every handler fixture.
type IngestJobArgs = jobs.IngestJobArgs
type EpgSyncArgs = jobs.EpgSyncArgs
type TunerSyncArgs = jobs.TunerSyncArgs
type RulerPassArgs = jobs.RulerPassArgs
type ReconcilePassArgs = jobs.ReconcilePassArgs
type RecordSweepArgs = jobs.RecordSweepArgs
type EncodeJobArgs = jobs.EncodeJobArgs
type EncodeEnqueueHintArgs = jobs.EncodeEnqueueHintArgs
type ThumbnailJobArgs = jobs.ThumbnailJobArgs
type EncodeReconcileArgs = jobs.EncodeReconcileArgs
type DeleteReconcileArgs = jobs.DeleteReconcileArgs
type CatalogExportArgs = jobs.CatalogExportArgs
type StorageSyncArgs = jobs.StorageSyncArgs

const (
	ingestQueue          = jobs.IngestQueue
	epgQueue             = jobs.EpgQueue
	rulerQueue           = jobs.RulerQueue
	reconcilerQueue      = jobs.ReconcilerQueue
	recordSweepQueue     = jobs.RecordSweepQueue
	encodeQueue          = jobs.EncodeQueue
	thumbnailQueue       = jobs.ThumbnailQueue
	cleanupQueue         = jobs.CleanupQueue
	storageQueue         = jobs.StorageQueue
	uniqueByQueue        = jobs.UniqueByQueue
	riverQueueNameMaxLen = jobs.RiverQueueNameMaxLen
)

var siteBoundQueueNames = jobs.SiteBoundQueueNames()

func qualifyQueueName(base, site string) string {
	return jobs.QualifyQueueName(base, site)
}

func physicalQueueName(logical, boundSite string) string {
	return jobs.PhysicalQueueName(logical, boundSite)
}

func AllQueueNames() []string {
	return jobs.AllQueueNames()
}

func RequiresEncodeTools(queues []string) bool {
	return jobs.RequiresEncodeTools(queues)
}

func RequiresSiteBinding(queues []string) bool {
	return jobs.RequiresSiteBinding(queues)
}
