package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
)

type riverTestInsertResult struct {
	Job                      *rivertype.JobRow
	UniqueSkippedAsDuplicate bool
}

func insertJobWithSpec(ctx context.Context, client *river.Client[pgx.Tx], args jobq.Args) (*riverTestInsertResult, error) {
	result, err := riveradapter.NewLegacyEnqueuer(client).Enqueue(ctx, args)
	if err != nil {
		return nil, err
	}
	queue, err := jobs.PhysicalQueueFor(args)
	if err != nil {
		return nil, err
	}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s args: %w", args.Kind(), err)
	}
	var expected any
	if err := json.Unmarshal(encodedArgs, &expected); err != nil {
		return nil, fmt.Errorf("decoding %s args: %w", args.Kind(), err)
	}
	jobsResult, err := client.JobList(ctx, river.NewJobListParams().Kinds(args.Kind()).Queues(queue).OrderBy(river.JobListOrderByID, river.SortOrderDesc).First(1000))
	if err != nil {
		return nil, fmt.Errorf("listing %s jobs in %s: %w", args.Kind(), queue, err)
	}
	for _, job := range jobsResult.Jobs {
		var actual any
		if err := json.Unmarshal(job.EncodedArgs, &actual); err != nil {
			return nil, fmt.Errorf("decoding listed %s args: %w", args.Kind(), err)
		}
		if reflect.DeepEqual(actual, expected) {
			return &riverTestInsertResult{Job: job, UniqueSkippedAsDuplicate: result.Coalesced}, nil
		}
	}
	return nil, fmt.Errorf("jobq enqueue of %s into %s returned no matching River job", args.Kind(), queue)
}
