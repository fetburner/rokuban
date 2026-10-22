package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/serverevent"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var mediaContractEventID atomic.Int32

func TestMediaStoreContract(t *testing.T) {
	contracttest.RunMedia(t, newMediaStoreContractFixture)
	contracttest.RunMediaPublish(t, newMediaStoreContractFixture)
}

func newMediaStoreContractFixture(t *testing.T) contracttest.MediaFixture {
	pool := testutil.SetupDB(t)
	fixture := contracttest.MediaFixture{
		Serving:      NewMedia(pool),
		Reconcile:    NewMedia(pool),
		Precheck:     NewMedia(pool),
		Storage:      NewMedia(pool),
		Reader:       NewMedia(pool),
		Publisher:    NewMedia(pool),
		Progress:     NewMedia(pool),
		Chapters:     NewAnalysis(pool),
		Site:         store.DefaultSite,
		NewRecording: func(t *testing.T) int64 { return newMediaContractRecording(t, pool) },
		SetRecordingEncodeTimes: func(t *testing.T, recordingID int64, startedAt, endedAt *time.Time) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE recordings SET started_at = $2, ended_at = $3 WHERE id = $1`, recordingID, startedAt, endedAt); err != nil {
				t.Fatalf("setting encode times for recording %d: %v", recordingID, err)
			}
		},
		SetRecordingPurged: func(t *testing.T, recordingID int64) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = COALESCE(deleted_at, now()), purged_at = now() WHERE id = $1`, recordingID); err != nil {
				t.Fatalf("purging recording %d: %v", recordingID, err)
			}
		},
		NewAsset: func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64 {
			return newMediaContractAsset(t, pool, recordingID, kind, profile, relPath, state)
		},
	}
	configureMediaPublishFixture(t, pool, &fixture)
	configureMediaReaderFixture(t, pool, &fixture)
	configureMediaObservationFixture(t, pool, &fixture)
	configureMediaStorageFixture(t, pool, &fixture)
	return fixture
}

func newMediaContractRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
		fixtureParams.Status = "finished"
		fixtureParams.EventID = mediaContractEventID.Add(1)
		fixtureParams.ServiceName = "store contract fixture"
		fixtureParams.Title = "store media contract fixture"
		fixtureParams.ProgramStartAt = time.Now().UTC().Truncate(time.Second)
		fixtureParams.ProgramDurationMs = 60_000
	})

	return id
}

func newMediaContractAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, profile *string, relPath, state string) int64 {
	t.Helper()
	id := dbtest.OriginalAsset(t, pool, recordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
		fixtureParams.Kind = kind
		fixtureParams.Profile = profile
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = 1
	})

	if state != "active" {
		if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET state = $1 WHERE id = $2", state, id); err != nil {
			t.Fatalf("setting media asset state to %s: %v", state, err)
		}
	}
	return id
}

func configureMediaPublishFixture(t *testing.T, pool *pgxpool.Pool, fixture *contracttest.MediaFixture) {
	t.Helper()
	fixture.SetEncodeProfiles = func(t *testing.T, recordingID int64, profiles []string) {
		t.Helper()
		if profiles == nil {
			profiles = []string{}
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
			VALUES ($1, 'always', $2::text[], false)
			ON CONFLICT (recording_id) DO UPDATE SET encode_profiles = EXCLUDED.encode_profiles`, recordingID, profiles); err != nil {
			t.Fatalf("setting desired encode profiles for recording %d: %v", recordingID, err)
		}
	}
	fixture.BeginEncodeAttempt = func(t *testing.T, recordingID int64, profile string) int32 {
		t.Helper()
		if _, err := sqlcgen.New(pool).CreateRecordingEncodeAttemptRunning(context.Background(), sqlcgen.CreateRecordingEncodeAttemptRunningParams{
			RecordingID: recordingID,
			Profile:     profile,
		}); err != nil {
			t.Fatalf("creating encode attempt for recording %d profile %q: %v", recordingID, profile, err)
		}
		var attempt int32
		if err := pool.QueryRow(context.Background(), `
			SELECT attempt_count FROM recording_encode_attempts
			WHERE recording_id = $1 AND profile = $2`, recordingID, profile).Scan(&attempt); err != nil {
			t.Fatalf("reading encode attempt for recording %d profile %q: %v", recordingID, profile, err)
		}
		return attempt
	}
	fixture.HasEncodePolicy = func(t *testing.T, recordingID int64) (store.EncodePolicySnapshot, bool) {
		t.Helper()
		var policy store.EncodePolicySnapshot
		if err := pool.QueryRow(context.Background(), `
			SELECT keep_original, encode_profiles, cm_detect
			FROM recording_encode_policy WHERE recording_id = $1`, recordingID).Scan(
			&policy.KeepOriginal, &policy.EncodeProfiles, &policy.CMDetect,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.EncodePolicySnapshot{}, false
			}
			t.Fatalf("reading encode policy for recording %d: %v", recordingID, err)
		}
		return policy, true
	}
	fixture.BeginChapterEdit = func(t *testing.T, id int64, spans []chapters.Span) func() {
		t.Helper()
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("beginning chapter edit tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		// チャプター編集と同じ順序（行ロック、区間の書き込み）。
		q := sqlcgen.New(tx)
		if _, err := q.LockRecording(ctx, id); err != nil {
			t.Fatalf("locking recording: %v", err)
		}
		if err := q.UpsertRecordingChapterOwnership(ctx, id); err != nil {
			t.Fatalf("adopting chapters: %v", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM recording_chapter_spans WHERE recording_id = $1", id); err != nil {
			t.Fatalf("clearing chapter spans: %v", err)
		}
		for _, sp := range spans {
			if _, err := tx.Exec(ctx,
				"INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), $4, $5)",
				id, sp.StartMs, sp.EndMs, sp.Label, sp.Cut); err != nil {
				t.Fatalf("writing chapter span: %v", err)
			}
		}
		return func() {
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("committing chapter edit tx: %v", err)
			}
		}
	}
	fixture.BeginEncodedRemoval = func(t *testing.T, id int64, profile string) func() {
		t.Helper()
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("beginning encoded removal tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		// 版の削除と同じ順序（録画の行、方針の行、desired から外す）。
		q := sqlcgen.New(tx)
		if _, err := q.LockRecording(ctx, id); err != nil {
			t.Fatalf("locking recording: %v", err)
		}
		if _, err := q.LockRecordingEncodePolicy(ctx, id); err != nil {
			t.Fatalf("locking encode policy: %v", err)
		}
		if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: id, Profile: profile}); err != nil {
			t.Fatalf("removing encode profile: %v", err)
		}
		return func() {
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("committing encoded removal tx: %v", err)
			}
		}
	}
	fixture.LoseTransactionDuringPublish = func(t *testing.T) {
		t.Helper()
		testutil.TerminateOpenTransactions(t, pool)
	}
	fixture.SetIngestProgress = func(t *testing.T, recordingID int64, written int64, expected *int64) {
		t.Helper()
		if err := sqlcgen.New(pool).UpsertRecordingIngestProgress(context.Background(), sqlcgen.UpsertRecordingIngestProgressParams{
			RecordingID: recordingID, WrittenBytes: written, ExpectedBytes: expected,
		}); err != nil {
			t.Fatalf("seeding ingest progress for recording %d: %v", recordingID, err)
		}
	}
	fixture.HasIngestProgress = func(t *testing.T, recordingID int64) bool {
		t.Helper()
		var exists bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM recording_ingest_progress WHERE recording_id = $1)`, recordingID).Scan(&exists); err != nil {
			t.Fatalf("checking ingest progress for recording %d: %v", recordingID, err)
		}
		return exists
	}
	fixture.NewInPlaceInput = func(t *testing.T, relPath string) store.InPlaceInput {
		t.Helper()
		return store.InPlaceInput{
			Recording: store.InPlaceRecording{
				Source: "manual", Site: store.DefaultSite, NetworkID: 32678, ServiceID: 5168,
				EventID: mediaContractEventID.Add(1), ServiceName: "in-place contract fixture",
				ChannelType: "GR", Channel: "27", Title: "in-place contract fixture",
				ProgramStartAt: time.Now().UTC().Truncate(time.Second), ProgramDurationMs: 60_000, Status: "finished",
			},
			Assets: []store.InPlaceAsset{{Kind: store.AssetKindOriginal, RelPath: relPath, SizeBytes: 1}},
		}
	}
}

func configureMediaReaderFixture(t *testing.T, pool *pgxpool.Pool, fixture *contracttest.MediaFixture) {
	t.Helper()
	fixture.FailEncodeAttempt = func(t *testing.T, recordingID int64, profile string, attemptCount int32) {
		t.Helper()
		tag, err := pool.Exec(context.Background(), `
UPDATE recording_encode_attempts
SET state = 'failed', attempt_count = $3
WHERE recording_id = $1 AND profile = $2`, recordingID, profile, attemptCount)
		if err != nil {
			t.Fatalf("failing encode attempt for recording %d profile %q: %v", recordingID, profile, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("failing encode attempt for recording %d profile %q changed %d rows; want 1", recordingID, profile, tag.RowsAffected())
		}
	}
	fixture.SetRecordingDropStats = func(t *testing.T, assetID int64, stats []store.RecordingDropStat) {
		t.Helper()
		ctx := context.Background()
		var sizeBytes int64
		if err := pool.QueryRow(ctx, `SELECT size_bytes FROM media_assets WHERE id = $1`, assetID).Scan(&sizeBytes); err != nil {
			t.Fatalf("reading size of media asset %d: %v", assetID, err)
		}
		if err := sqlcgen.New(pool).UpsertMediaAssetTSScan(ctx, sqlcgen.UpsertMediaAssetTSScanParams{
			MediaAssetID: assetID, ScannedSizeBytes: sizeBytes,
		}); err != nil {
			t.Fatalf("marking media asset %d as scanned: %v", assetID, err)
		}
		for _, stat := range stats {
			if _, err := pool.Exec(ctx, `
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, assetID, stat.Pid, stat.Packets, stat.Drops, stat.Errors, stat.Scrambled, stat.PidType); err != nil {
				t.Fatalf("seeding drop stats for media asset %d PID %d: %v", assetID, stat.Pid, err)
			}
		}
	}
	fixture.SetRecordingDropPositions = func(t *testing.T, assetID int64, positions []store.RecordingDropPosition) {
		t.Helper()
		for _, position := range positions {
			if _, err := pool.Exec(context.Background(), `
INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms)
VALUES ($1, $2, $3, $4)`, assetID, position.ByteOffset, position.Pid, position.ElapsedMs); err != nil {
				t.Fatalf("seeding drop position for media asset %d at byte %d: %v", assetID, position.ByteOffset, err)
			}
		}
	}
	fixture.StartEncodeProgressListener = func(t *testing.T) func() store.EncodeProgress {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := pool.Acquire(ctx)
		if err != nil {
			cancel()
			t.Fatalf("acquiring encode progress listener connection: %v", err)
		}
		t.Cleanup(func() {
			cancel()
			conn.Release()
		})
		if _, err := conn.Exec(ctx, "LISTEN rokuban"); err != nil {
			t.Fatalf("listening for encode progress: %v", err)
		}
		return func() store.EncodeProgress {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				t.Fatalf("waiting for encode progress: %v", err)
			}
			var event serverevent.EncodeProgressEvent
			if err := json.Unmarshal([]byte(notification.Payload), &event); err != nil {
				t.Fatalf("decoding encode progress notification: %v", err)
			}
			if event.Type != serverevent.EncodeProgressEventType {
				t.Fatalf("encode progress notification type = %q; want %q", event.Type, serverevent.EncodeProgressEventType)
			}
			return store.EncodeProgress{RecordingID: event.RecordingID, Profile: event.Profile, Progress: event.Progress}
		}
	}
}

func configureMediaObservationFixture(t *testing.T, pool *pgxpool.Pool, fixture *contracttest.MediaFixture) {
	t.Helper()
	fixture.SetRecordingTrashed = func(t *testing.T, recordingID int64) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), "UPDATE recordings SET deleted_at = now() WHERE id = $1", recordingID); err != nil {
			t.Fatalf("trashing recording %d: %v", recordingID, err)
		}
	}
	fixture.NewRecordSync = func(t *testing.T, recordingID int64, site, recordID, status string) {
		t.Helper()
		if err := sqlcgen.New(pool).UpsertRecordSync(context.Background(), sqlcgen.UpsertRecordSyncParams{
			Site:        site,
			RecordID:    recordID,
			RecordingID: &recordingID,
			ProgramID:   recordingID,
			Status:      status,
			Tags:        []string{},
		}); err != nil {
			t.Fatalf("seeding record_sync: %v", err)
		}
	}
	fixture.SetCMRanges = func(t *testing.T, recordingID int64, ranges []chapters.Range) {
		t.Helper()
		lit := "{"
		for i, r := range ranges {
			if i > 0 {
				lit += ","
			}
			lit += fmt.Sprintf("[%d,%d)", r.StartMs, r.EndMs)
		}
		lit += "}"
		if _, err := pool.Exec(context.Background(),
			"INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, $2::text::int8multirange)", recordingID, lit); err != nil {
			t.Fatalf("seeding CM ranges: %v", err)
		}
	}
	fixture.SetChapterSpans = func(t *testing.T, recordingID int64, spans []chapters.Span) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), "INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)", recordingID); err != nil {
			t.Fatalf("adopting chapters: %v", err)
		}
		for _, sp := range spans {
			if _, err := pool.Exec(context.Background(),
				"INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), $4, $5)",
				recordingID, sp.StartMs, sp.EndMs, sp.Label, sp.Cut); err != nil {
				t.Fatalf("seeding chapter span: %v", err)
			}
		}
	}
}

func configureMediaStorageFixture(t *testing.T, pool *pgxpool.Pool, fixture *contracttest.MediaFixture) {
	t.Helper()
	fixture.SetOrphanFirstSeenAgo = func(t *testing.T, relPath string, ago time.Duration) {
		t.Helper()
		if _, err := pool.Exec(context.Background(),
			"UPDATE orphan_files SET first_seen = now() - ($2::bigint * interval '1 microsecond') WHERE rel_path = $1",
			relPath, ago.Microseconds()); err != nil {
			t.Fatalf("aging orphan %q: %v", relPath, err)
		}
	}
	fixture.SetMissingFirstSeenAgo = func(t *testing.T, assetID int64, ago time.Duration) {
		t.Helper()
		if _, err := pool.Exec(context.Background(),
			"UPDATE missing_media_assets SET first_seen = now() - ($2::bigint * interval '1 microsecond') WHERE media_asset_id = $1",
			assetID, ago.Microseconds()); err != nil {
			t.Fatalf("aging missing asset %d: %v", assetID, err)
		}
	}
	fixture.SetAssetState = func(t *testing.T, assetID int64, state string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET state = $1 WHERE id = $2", state, assetID); err != nil {
			t.Fatalf("setting media asset state to %s: %v", state, err)
		}
	}
	fixture.SetMediaAssetKeepRanges = func(t *testing.T, assetID int64, keep []chapters.Range) {
		t.Helper()
		param, err := KeepRangesParam(keep)
		if err != nil {
			t.Fatalf("encoding frozen keep ranges: %v", err)
		}
		if err := sqlcgen.New(pool).InsertMediaAssetCuts(context.Background(), sqlcgen.InsertMediaAssetCutsParams{
			MediaAssetID: assetID,
			KeepRanges:   param,
		}); err != nil {
			t.Fatalf("seeding frozen keep ranges: %v", err)
		}
	}
	fixture.SetEncodedAssetRebuildRequest = func(t *testing.T, recordingID int64, profile string, keep []chapters.Range) {
		t.Helper()
		param, err := KeepRangesParam(keep)
		if err != nil {
			t.Fatalf("encoding rebuild request keep ranges: %v", err)
		}
		if err := sqlcgen.New(pool).UpsertEncodedAssetRebuildRequest(context.Background(), sqlcgen.UpsertEncodedAssetRebuildRequestParams{
			RecordingID:   recordingID,
			Profile:       profile,
			RequestedKeep: param,
		}); err != nil {
			t.Fatalf("seeding encoded asset rebuild request: %v", err)
		}
	}
}
