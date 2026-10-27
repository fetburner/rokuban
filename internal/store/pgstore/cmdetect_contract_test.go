package pgstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCMDetectionContract(t *testing.T) {
	contracttest.RunCMDetection(t, newCMDetectionContractFixture)
}

func newCMDetectionContractFixture(t *testing.T) contracttest.CMDetectionFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	analysis := NewAnalysis(pool)
	return contracttest.CMDetectionFixture{
		Store:          analysis,
		ChapterStore:   analysis,
		CMStationStore: analysis,
		TrashStore:     NewRecordings(pool),
		NewRecording: func(t *testing.T) int64 {
			return seedCMDetectionContractRecording(t, pool)
		},
		SeedLogo: func(t *testing.T, station store.CMStationRef, recordingID int64) {
			seedCMDetectionContractLogo(t, pool, station, recordingID)
		},
		BeginChapterWrite: func(t *testing.T, recordingID int64) func() {
			return beginCMDetectionChapterWrite(t, pool, recordingID)
		},
		BeginLogoAreaWrite: func(t *testing.T, station store.CMStationRef, input store.CMLogoAreaInput) func() {
			return beginCMDetectionLogoAreaWrite(t, pool, station, input)
		},
		WaitForRecordingWriteWait: func(t *testing.T, recordingID int64, name string) {
			waitForCMDetectionRecordingLock(t, pool, recordingID, name)
		},
		WaitForStationWriteWait: func(t *testing.T, station store.CMStationRef, name string) {
			waitForCMDetectionStationLock(t, pool, station, name)
		},
		MarkOriginalMissing: func(t *testing.T, recordingID int64) {
			markCMDetectionOriginalMissing(t, pool, recordingID)
		},
		AdvancePolicyGeneration: func(t *testing.T, recordingID int64) {
			advanceCMDetectionPolicyGeneration(t, pool, recordingID)
		},
	}
}

func seedCMDetectionContractRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Title = "CM detection store contract fixture"
	})

	dbtest.MediaAsset(t, pool, id, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = fmt.Sprintf("test/%d.ts", id)
		fixtureParams.SizeBytes = 1000
	})
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)`, id); err != nil {
		t.Fatalf("enabling CM detection: %v", err)
	}
	return id
}

func seedCMDetectionContractLogo(t *testing.T, pool *pgxpool.Pool, station store.CMStationRef, recordingID int64) {
	t.Helper()
	learnedFrom := recordingID
	if err := sqlcgen.New(pool).UpsertCMLogo(context.Background(), sqlcgen.UpsertCMLogoParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		Lgd: []byte("contract logo"), LearnedFrom: &learnedFrom, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatalf("seeding station logo: %v", err)
	}
}

func beginCMDetectionChapterWrite(t *testing.T, pool *pgxpool.Pool, recordingID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning held chapter write: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	})
	q := sqlcgen.New(tx)
	if _, err := q.LockRecording(ctx, recordingID); err != nil {
		t.Fatalf("locking recording for chapter write: %v", err)
	}
	if err := q.UpsertRecordingChapterOwnership(ctx, recordingID); err != nil {
		t.Fatalf("writing chapter ownership: %v", err)
	}
	return func() { commitCMDetectionFixtureTransaction(t, tx, ctx, &committed) }
}

func beginCMDetectionLogoAreaWrite(t *testing.T, pool *pgxpool.Pool, station store.CMStationRef, input store.CMLogoAreaInput) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning held logo area write: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	})
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		t.Fatalf("locking station for logo area write: %v", err)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		X: input.X, Y: input.Y, W: input.W, H: input.H,
		CodedWidth: input.CodedWidth, CodedHeight: input.CodedHeight,
	}); err != nil {
		t.Fatalf("updating logo area: %v", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE cm_logo_areas SET updated_at = updated_at + interval '1 second'
WHERE network_id = $1 AND service_id = $2`, station.NetworkID, station.ServiceID); err != nil {
		t.Fatalf("advancing logo area version: %v", err)
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
	}); err != nil {
		t.Fatalf("clearing old logo candidate: %v", err)
	}
	return func() { commitCMDetectionFixtureTransaction(t, tx, ctx, &committed) }
}

func commitCMDetectionFixtureTransaction(t *testing.T, tx interface{ Commit(context.Context) error }, ctx context.Context, committed *bool) {
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("committing held fixture write: %v", err)
	}
	*committed = true
}

func waitForCMDetectionRecordingLock(t *testing.T, pool *pgxpool.Pool, recordingID int64, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
    WHERE datname = current_database()
      AND pid <> pg_backend_pid()
      AND wait_event_type = 'Lock'
      AND query ILIKE '%FROM recordings%'
      AND query ILIKE '%FOR UPDATE%'
)`).Scan(&waiting)
		if err != nil {
			t.Fatalf("checking for a wait on the recording row: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not wait on recording %d", name, recordingID)
}

func waitForCMDetectionStationLock(t *testing.T, pool *pgxpool.Pool, station store.CMStationRef, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity a
    JOIN pg_locks l ON l.pid = a.pid
    WHERE a.datname = current_database()
      AND a.pid <> pg_backend_pid()
      AND a.wait_event_type = 'Lock'
      AND a.query LIKE '%pg_advisory_xact_lock%'
      AND l.locktype = 'advisory'
      AND NOT l.granted
      AND l.classid = $1::oid
      AND l.objid = $2::oid
      AND l.objsubid = 2
)`, station.NetworkID, station.ServiceID).Scan(&waiting)
		if err != nil {
			t.Fatalf("checking for a wait on the station lock: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not wait on station %d/%d", name, station.NetworkID, station.ServiceID)
}

func advanceCMDetectionPolicyGeneration(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE recording_encode_policy SET cm_detect_generation = cm_detect_generation + 1
WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatalf("advancing CM detection generation: %v", err)
	}
}

func markCMDetectionOriginalMissing(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	ctx := context.Background()
	var assetID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM media_assets WHERE recording_id = $1 AND kind = $2`,
		recordingID, store.AssetKindOriginal).Scan(&assetID); err != nil {
		t.Fatalf("finding original media asset: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMissingMediaAsset(ctx, sqlcgen.UpsertMissingMediaAssetParams{MediaAssetID: assetID, Store: ""}); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}
}
