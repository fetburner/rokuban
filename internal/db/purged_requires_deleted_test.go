package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecordingsPurgedRequiresDeleted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	for _, operation := range []string{"insert", "update", "clear_deleted_at"} {
		t.Run(operation, func(t *testing.T) {
			id := dbtest.Recording(t, pool)
			var err error
			switch operation {
			case "insert":
				_, err = pool.Exec(ctx, `INSERT INTO recordings
     (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
      program_start_at, program_duration_ms, status, purged_at)
     VALUES ('manual', 'test', 1, 1, 1, 'test', 'GR', '1', now(), 1000, 'finished', now())`)
			case "update":
				_, err = pool.Exec(ctx, `UPDATE recordings SET purged_at = now() WHERE id = $1`, id)
			case "clear_deleted_at":
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now(), purged_at = now() WHERE id = $1`, id); err != nil {
					t.Fatal(err)
				}
				_, err = pool.Exec(ctx, `UPDATE recordings SET deleted_at = NULL WHERE id = $1`, id)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "recordings_purged_requires_deleted" {
				t.Fatalf("%s: want recordings_purged_requires_deleted check violation, got %v", operation, err)
			}
		})
	}
	for _, state := range []string{"active", "trashed", "purged", "superseded"} {
		t.Run(state, func(t *testing.T) {
			id := dbtest.Recording(t, pool)
			_, err := pool.Exec(ctx, `UPDATE recordings SET
    deleted_at = CASE WHEN $2 IN ('trashed', 'purged') THEN now() END,
    purged_at = CASE WHEN $2 = 'purged' THEN now() END,
    superseded_at = CASE WHEN $2 = 'superseded' THEN now() END WHERE id = $1`, id, state)
			if err != nil {
				t.Fatalf("valid %s: %v", state, err)
			}
		})
	}
}
