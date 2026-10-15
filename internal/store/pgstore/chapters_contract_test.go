package pgstore

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var contractEventID atomic.Int32

func TestChapterEditsContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T) (store.ChapterEdits, contracttest.NewRecording) {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T, options contracttest.RecordingOptions) int64 {
			t.Helper()
			ctx := context.Background()
			eventID := contractEventID.Add(1)
			id, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				Source:            "manual",
				Site:              db.DefaultSite,
				NetworkID:         32678,
				ServiceID:         5168,
				EventID:           eventID,
				ServiceName:       "ＯＨＫ",
				ChannelType:       "GR",
				Channel:           "27",
				Title:             "store contract fixture",
				ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
				ProgramDurationMs: 30 * time.Minute.Milliseconds(),
				Status:            "finished",
			})
			if err != nil {
				t.Fatalf("seeding recording: %v", err)
			}
			if options.CMDetectionEnabled {
				if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)`, id); err != nil {
					t.Fatalf("enabling CM detection: %v", err)
				}
			}
			if options.Trashed {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("trashing recording: %v", err)
				}
			}
			if options.Purged {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET purged_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("purging recording: %v", err)
				}
			}
			return id
		}
		return New(pool), newRecording
	})
}

func TestMapDatabaseErrorMapsUniqueAndRangeConflicts(t *testing.T) {
	for _, code := range []string{"23505", "23P01"} {
		t.Run(code, func(t *testing.T) {
			got := mapDatabaseError(&pgconn.PgError{Code: code})
			if !errors.Is(got, store.ErrConflict) {
				t.Fatalf("mapped error = %v, want ErrConflict", got)
			}
		})
	}
}
