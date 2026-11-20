package dbtest

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ObservedScheduleParams は ObservedSchedule の上書き対象。
type ObservedScheduleParams struct {
	Site         string
	State        string
	Options      json.RawMessage
	Tags         []string
	FailedReason json.RawMessage
}

// ObservedSchedule は Mirakc から観測した schedule を schedule_sync に作る。既定値は site = "default"、state = "scheduled"、options = {"priority":10}、tags と failed_reason は空。snapshot marker は作らない。opts で既定値を上書きする。
func ObservedSchedule(t testing.TB, db sqlcgen.DBTX, programID int64, opts ...func(*ObservedScheduleParams)) {
	t.Helper()
	params := ObservedScheduleParams{
		Site:    "default",
		State:   "scheduled",
		Options: json.RawMessage(`{"priority":10}`),
		Tags:    []string{},
	}
	for _, opt := range opts {
		opt(&params)
	}

	batch := sqlcgen.New(db).UpsertScheduleSync(t.Context(), []sqlcgen.UpsertScheduleSyncParams{{
		Site: params.Site, ProgramID: programID, State: params.State,
		Options: params.Options, Tags: params.Tags, FailedReason: params.FailedReason,
	}})
	var batchErr error
	batch.Exec(func(i int, err error) {
		if err != nil && batchErr == nil {
			batchErr = fmt.Errorf("batch item %d: %w", i, err)
		}
	})
	if err := batch.Close(); err != nil && batchErr == nil {
		batchErr = err
	}
	if batchErr != nil {
		t.Fatalf("upserting observed schedule for program %d at site %q: %v", programID, params.Site, batchErr)
	}
}
