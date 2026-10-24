package pgstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var encodePolicyContractEventID atomic.Int32

func TestEncodePolicyContract(t *testing.T) {
	contracttest.RunEncodePolicy(t, newEncodePolicyContractFixture)
}

type encodePolicyContractDB struct {
	pool *pgxpool.Pool
}

func newEncodePolicyContractFixture(t *testing.T) contracttest.EncodePolicyFixture {
	t.Helper()
	db := &encodePolicyContractDB{pool: testutil.SetupDB(t)}
	media := NewMedia(db.pool)
	return contracttest.EncodePolicyFixture{
		Policy: media, Attempts: media,
		Recordings: NewRecordings(db.pool), Reconcile: media, Removals: media,
		CMDetection:  NewAnalysis(db.pool),
		NewRecording: db.newRecording, NewAsset: db.newAsset,
		SetRecordingTrashed: func(t *testing.T, recordingID int64) {
			t.Helper()
			if _, err := db.pool.Exec(context.Background(), "UPDATE recordings SET deleted_at = now() WHERE id = $1", recordingID); err != nil {
				t.Fatalf("trashing recording %d: %v", recordingID, err)
			}
		},
		SetDesired: db.setDesired, SetRemovalRequest: db.setRemovalRequest,
		HasRemovalRequest: db.hasRemovalRequest,
		SetAttempt:        db.setAttempt, SetChapterSpans: db.setChapterSpans,
		BeginChapterEdit:  db.beginChapterEdit,
		BeginPolicyUpdate: db.beginPolicyUpdate, BeginRemoval: db.beginRemoval,
		BeginRecordingLock: db.beginRecordingLock, TryPolicyLock: db.tryPolicyLock,
		WaitRecordingLock: db.waitRecordingLock, WaitPolicyLock: db.waitPolicyLock,
	}
}

func (f *encodePolicyContractDB) newRecording(t *testing.T) int64 {
	t.Helper()
	id := dbtest.Recording(t, f.pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.EventID = encodePolicyContractEventID.Add(1)
		fixtureParams.ServiceName = "store encode policy contract fixture"
		fixtureParams.Title = "store encode policy contract fixture"
		fixtureParams.ProgramDurationMs = 20_000
	})

	return id
}

func (f *encodePolicyContractDB) newAsset(t *testing.T, recordingID int64, kind string, profile *string) int64 {
	t.Helper()
	id := dbtest.MediaAsset(t, f.pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.Kind = kind
		fixtureParams.Profile = profile
		fixtureParams.RelPath = fmt.Sprintf("sites/home/%d/%s-%v", recordingID, kind, profile)
	})

	return id
}

func (f *encodePolicyContractDB) setDesired(t *testing.T, recordingID int64, profiles []string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', $2, false)
ON CONFLICT (recording_id) DO UPDATE SET
    keep_original = 'always', encode_profiles = EXCLUDED.encode_profiles`, recordingID, profiles); err != nil {
		t.Fatalf("setting desired profiles: %v", err)
	}
}

func (f *encodePolicyContractDB) setRemovalRequest(t *testing.T, recordingID int64, profile string) {
	t.Helper()
	if err := sqlcgen.New(f.pool).InsertEncodedAssetRemovalRequest(context.Background(), sqlcgen.InsertEncodedAssetRemovalRequestParams{
		RecordingID: recordingID,
		Profile:     profile,
	}); err != nil {
		t.Fatalf("seeding removal request: %v", err)
	}
}

func (f *encodePolicyContractDB) hasRemovalRequest(t *testing.T, recordingID int64, profile string) bool {
	t.Helper()
	var exists bool
	if err := f.pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = $2)", recordingID, profile).Scan(&exists); err != nil {
		t.Fatalf("reading encoded asset removal request: %v", err)
	}
	return exists
}

func (f *encodePolicyContractDB) setAttempt(t *testing.T, recordingID int64, profile, state string, count int32) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempted_at, attempt_count)
VALUES ($1, $2, $3, now(), $4)`, recordingID, profile, state, count); err != nil {
		t.Fatalf("seeding encode attempt: %v", err)
	}
}

func (f *encodePolicyContractDB) setChapterSpans(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE recordings SET program_duration_ms = $2 WHERE id = $1`, recordingID, durationMs); err != nil {
		t.Fatalf("setting program duration: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	for _, span := range spans {
		if _, err := f.pool.Exec(ctx, `
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES ($1, int8range($2, $3), $4, $5)`, recordingID, span.StartMs, span.EndMs, span.Label, span.Cut); err != nil {
			t.Fatalf("seeding chapter span: %v", err)
		}
	}
}

func (f *encodePolicyContractDB) beginChapterEdit(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning chapter edit fixture: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	q := sqlcgen.New(tx)
	if _, err := q.LockRecording(ctx, recordingID); err != nil {
		t.Fatalf("locking recording before chapter edit fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE recordings SET program_duration_ms = $2 WHERE id = $1`, recordingID, durationMs); err != nil {
		t.Fatalf("updating program duration in chapter edit fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1) ON CONFLICT DO NOTHING`, recordingID); err != nil {
		t.Fatalf("adopting chapters in edit fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM recording_chapter_spans WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatalf("clearing chapter spans in edit fixture: %v", err)
	}
	for _, span := range spans {
		if _, err := tx.Exec(ctx, `
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES ($1, int8range($2, $3), $4, $5)`, recordingID, span.StartMs, span.EndMs, span.Label, span.Cut); err != nil {
			t.Fatalf("writing edited chapter span: %v", err)
		}
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("committing chapter edit fixture: %v", err)
		}
	}
}

func (f *encodePolicyContractDB) beginPolicyUpdate(t *testing.T, recordingID int64, profiles []string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning policy update fixture: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := sqlcgen.New(tx).LockRecordingEncodePolicy(ctx, recordingID); err != nil {
		t.Fatalf("locking encode policy in update fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE recording_encode_policy SET encode_profiles = $2 WHERE recording_id = $1`, recordingID, profiles); err != nil {
		t.Fatalf("updating desired profiles in fixture: %v", err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("committing policy update fixture: %v", err)
		}
	}
}

func (f *encodePolicyContractDB) beginRemoval(t *testing.T, recordingID int64, profile string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning removal fixture: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	q := sqlcgen.New(tx)
	if err := lockRecording(ctx, q, recordingID); err != nil {
		t.Fatalf("locking recording before removal fixture: %v", err)
	}
	if _, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{RecordingID: recordingID, Profile: &profile}); err != nil {
		t.Fatalf("loading active encoded asset in removal fixture: %v", err)
	}
	if err := q.FreezeRecordingEncodePolicyIfMissing(ctx, recordingID); err != nil {
		t.Fatalf("freezing policy in removal fixture: %v", err)
	}
	if _, err := q.LockRecordingEncodePolicy(ctx, recordingID); err != nil {
		t.Fatalf("locking policy in removal fixture: %v", err)
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: recordingID, Profile: profile}); err != nil {
		t.Fatalf("removing desired profile in fixture: %v", err)
	}
	if err := q.DeleteEncodedAssetRebuildRequest(ctx, sqlcgen.DeleteEncodedAssetRebuildRequestParams{RecordingID: recordingID, Profile: profile}); err != nil {
		t.Fatalf("deleting rebuild request in fixture: %v", err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: recordingID, Profile: profile}); err != nil {
		t.Fatalf("inserting removal request in fixture: %v", err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("committing removal fixture: %v", err)
		}
	}
}

func (f *encodePolicyContractDB) beginRecordingLock(t *testing.T, recordingID int64) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning recording lock fixture: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := sqlcgen.New(tx).LockRecording(ctx, recordingID); err != nil {
		t.Fatalf("locking recording in fixture: %v", err)
	}
	return func() {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("committing recording lock fixture: %v", err)
		}
	}
}

func (f *encodePolicyContractDB) tryPolicyLock(t *testing.T, recordingID int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning policy lock probe: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM recording_encode_policy WHERE recording_id = $1 FOR UPDATE NOWAIT`, recordingID); err != nil {
		t.Fatalf("policy row was locked before recording row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing policy lock probe: %v", err)
	}
}

func (f *encodePolicyContractDB) waitRecordingLock(t *testing.T) {
	waitForEncodePolicyLockWaiter(t, f.pool, "%FROM recordings%")
}

func (f *encodePolicyContractDB) waitPolicyLock(t *testing.T) {
	waitForEncodePolicyLockWaiter(t, f.pool, "%recording_encode_policy%")
}

func waitForEncodePolicyLockWaiter(t *testing.T, pool *pgxpool.Pool, queryPattern string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND pid <> pg_backend_pid()
  AND query LIKE $1`, queryPattern).Scan(&n); err != nil {
			t.Fatalf("polling pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("lock waiter did not appear for query %q", queryPattern)
}
