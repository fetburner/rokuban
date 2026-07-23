package worker

import (
	"context"
	"log/slog"

	"github.com/riverqueue/river"
)

type IngestJobArgs struct {
	Site     string `json:"site"`
	RecordID string `json:"record_id"`
}

func (IngestJobArgs) Kind() string { return "ingest" }

func (IngestJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
		},
	}
}

type IngestWorker struct {
	river.WorkerDefaults[IngestJobArgs]
}

func (w *IngestWorker) Work(ctx context.Context, job *river.Job[IngestJobArgs]) error {
	slog.Info("ingest job placeholder", "site", job.Args.Site, "record_id", job.Args.RecordID)
	return nil
}
