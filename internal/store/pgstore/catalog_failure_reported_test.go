package pgstore

import (
	"encoding/json"
	"testing"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogFailureReportedRoundTrip(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	q := sqlcgen.New(pool)
	id := dbtest.Recording(t, pool)
	for _, reported := range []bool{true, false} {
		if _, err := pool.Exec(ctx, `UPDATE recordings SET failure_reported=$2 WHERE id=$1`, id, reported); err != nil {
			t.Fatal(err)
		}
		rows, err := q.CatalogListRecordings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var record catalogdoc.Recording
		for _, row := range rows {
			if row.ID == id {
				record = recordingFromRow(row)
			}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var decoded catalogdoc.Recording
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.FailureReported == nil || *decoded.FailureReported != reported {
			t.Fatalf("export lost reported=%t: %s", reported, raw)
		}
		if _, err := pool.Exec(ctx, `UPDATE recordings SET failure_reported=$2 WHERE id=$1`, id, !reported); err != nil {
			t.Fatal(err)
		}
		if err := applyRecordings(ctx, q, []catalogdoc.Recording{decoded}, &store.RestoreResult{}); err != nil {
			t.Fatal(err)
		}
		var got bool
		if err := pool.QueryRow(ctx, `SELECT failure_reported FROM recordings WHERE id=$1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != reported {
			t.Fatalf("restored reported=%t want=%t", got, reported)
		}
		for _, legacy := range []struct {
			events string
			want   bool
		}{{`[{"event":"recording.failed"}]`, true}, {`[{"event":"recording.record-broken"}]`, false}} {
			decoded.FailureReported = nil
			decoded.QualityEvents = []byte(legacy.events)
			if err := applyRecordings(ctx, q, []catalogdoc.Recording{decoded}, &store.RestoreResult{}); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT failure_reported FROM recordings WHERE id=$1`, id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != legacy.want {
				t.Fatalf("legacy restored reported=%t want=%t", got, legacy.want)
			}
		}
	}
}
