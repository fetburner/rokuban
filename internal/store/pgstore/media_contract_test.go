package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

func TestRecordingHLSOriginalTargetPreservesCommittedGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	startedAt := time.Date(2026, 10, 25, 3, 4, 5, 0, time.UTC)
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.StartedAt = &startedAt
	})
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "sites/default/recording-hls-generation.m2ts"
	})
	selectedCopyPath := "sites/default/store-copy-recording-hls-generation.m2ts"
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET rel_path = $2 WHERE media_asset_id = $1 AND store = $3`,
		assetID, selectedCopyPath, dbtest.StoreName); err != nil {
		t.Fatalf("setting selected-store copy path: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: "secondary", RelPath: "sites/default/secondary-copy-recording-hls-generation.m2ts", State: "active",
	}); err != nil {
		t.Fatalf("creating secondary-store copy: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET ingest_generation = 19 WHERE id = $1`, assetID); err != nil {
		t.Fatalf("setting committed ingest generation: %v", err)
	}

	target, err := NewMedia(pool, dbtest.StoreName).GetRecordingHLSOriginalTarget(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetRecordingHLSOriginalTarget(%d): %v", recordingID, err)
	}
	if target.ID != assetID || target.RelPath != selectedCopyPath || target.IngestGeneration != 19 || target.StartedAt == nil || !target.StartedAt.Equal(startedAt) {
		t.Fatalf("target = %+v, want asset %d at %q, generation 19 and start %s", target, assetID, selectedCopyPath, startedAt)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = $2`, assetID, dbtest.StoreName); err != nil {
		t.Fatalf("deleting selected-store copy: %v", err)
	}
	if _, err := NewMedia(pool, dbtest.StoreName).GetRecordingHLSOriginalTarget(context.Background(), recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingHLSOriginalTarget(without selected-store active copy) error = %v, want ErrNotFound", err)
	}

	unfinishedID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Status = "recording"
	})
	dbtest.MediaAsset(t, pool, unfinishedID)
	if _, err := NewMedia(pool, dbtest.StoreName).GetRecordingHLSOriginalTarget(context.Background(), unfinishedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingHLSOriginalTarget(unfinished) error = %v, want ErrNotFound", err)
	}
}

func TestOriginalVODTargetUsesOnlyConfiguredStoreCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = "finished" })
	assetID := dbtest.MediaAsset(t, pool, recordingID)

	for _, copy := range []struct {
		store string
		path  string
	}{
		{store: "z-store", path: "sites/default/vod-z-copy.m2ts"},
		{store: "a-store", path: "sites/default/vod-a-copy.m2ts"},
	} {
		if err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
			MediaAssetID: assetID, Store: copy.store, RelPath: copy.path, State: "active",
		}); err != nil {
			t.Fatalf("creating %s copy: %v", copy.store, err)
		}
	}

	target, err := NewMedia(pool, "z-store").GetOriginalVODTarget(ctx, recordingID, "default")
	if err != nil {
		t.Fatalf("GetOriginalVODTarget(z-store): %v", err)
	}
	if target.ID != assetID || target.RelPath != "sites/default/vod-z-copy.m2ts" {
		t.Fatalf("target = %+v, want the copy in z-store, not the lexically first a-store", target)
	}
	for _, name := range []string{"", "missing-store"} {
		if _, err := NewMedia(pool, name).GetOriginalVODTarget(ctx, recordingID, "default"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetOriginalVODTarget(store %q) error = %v, want ErrNotFound", name, err)
		}
	}
}

func TestIngestTempGenerationIsStoredAndCommitted(t *testing.T) {
	pool := testutil.SetupDB(t)
	media := NewMedia(pool, "tokyo")
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	tempRelPath := fmt.Sprintf("sites/%s/test/.rokuban-ingest-generation-test", store.DefaultSite)
	generation, err := media.NextIngestTempGeneration(ctx)
	if err != nil {
		t.Fatalf("allocating temp generation: %v", err)
	}
	if generation <= 0 {
		t.Fatalf("temp generation = %d, want a positive sequence value", generation)
	}
	if err := media.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 17,
		TempRelPath: tempRelPath, TempGeneration: generation,
	}); err != nil {
		t.Fatalf("writing ingest progress: %v", err)
	}
	state, err := media.GetIngestTempState(ctx, recordingID)
	if err != nil || state == nil {
		t.Fatalf("GetIngestTempState() = (%+v, %v), want a row", state, err)
	}
	if state.TempRelPath != tempRelPath || state.TempGeneration != generation {
		t.Fatalf("ingest temp state = %+v, want path %q and generation %d", state, tempRelPath, generation)
	}
	var tempStore string
	if err := pool.QueryRow(ctx, `SELECT temp_store FROM recording_ingest_progress WHERE recording_id = $1`, recordingID).Scan(&tempStore); err != nil {
		t.Fatalf("reading ingest temp store: %v", err)
	}
	if tempStore != "tokyo" {
		t.Fatalf("ingest temp store = %q, want tokyo", tempStore)
	}

	if err := media.CommitOriginal(ctx, store.CommitOriginalInput{
		RecordingID: recordingID, RelPath: "sites/default/generation-test.m2ts", SizeBytes: 17,
	}, func(store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
		return store.EncodePolicySnapshot{KeepOriginal: "always", EncodeProfiles: []string{}}, nil
	}, func() error { return nil }); err != nil {
		t.Fatalf("committing original: %v", err)
	}
	var committedGeneration int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(ingest_generation, 0) FROM media_assets WHERE recording_id = $1 AND kind = 'original'`,
		recordingID).Scan(&committedGeneration); err != nil {
		t.Fatalf("reading original generation: %v", err)
	}
	if committedGeneration != generation {
		t.Fatalf("original ingest_generation = %d, want progress generation %d", committedGeneration, generation)
	}
	var copyStore, copyRelPath, copyState string
	if err := pool.QueryRow(ctx, `
SELECT c.store, c.rel_path, c.state
FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
WHERE a.recording_id = $1 AND a.kind = 'original'`, recordingID,
	).Scan(&copyStore, &copyRelPath, &copyState); err != nil {
		t.Fatalf("reading committed original copy: %v", err)
	}
	if copyStore != "tokyo" || copyRelPath != "sites/default/generation-test.m2ts" || copyState != "active" {
		t.Fatalf("committed original copy = %q/%q/%q, want tokyo active copy", copyStore, copyRelPath, copyState)
	}
	state, err = media.GetIngestTempState(ctx, recordingID)
	if err != nil || state != nil {
		t.Fatalf("GetIngestTempState() after commit = (%+v, %v), want no progress row", state, err)
	}
	nextGeneration, err := media.NextIngestTempGeneration(ctx)
	if err != nil {
		t.Fatalf("allocating next temp generation: %v", err)
	}
	if nextGeneration <= generation {
		t.Fatalf("next temp generation = %d, want greater than %d", nextGeneration, generation)
	}
}

func newMediaStoreContractFixture(t *testing.T) contracttest.MediaFixture {
	pool := testutil.SetupDB(t)
	fixture := contracttest.MediaFixture{
		Serving:   NewMedia(pool, "tokyo"),
		Reconcile: NewMedia(pool, "tokyo"),
		Precheck:  NewMedia(pool, "tokyo"),
		Storage:   NewMedia(pool, "tokyo"),
		Reader:    NewMedia(pool, "tokyo"),
		Query:     NewRecordings(pool, "tokyo"),
		Catalog:   NewCatalog(pool, []string{store.DefaultSite}, "tokyo"),
		Publisher: NewMedia(pool, "tokyo"),
		Progress:  NewMedia(pool, "tokyo"),
		Chapters:  NewAnalysis(pool, dbtest.StoreName),
		Site:      store.DefaultSite,
		NewRecording: func(t *testing.T) int64 {
			return dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
				p.Title = "store media contract fixture"
				p.Status = "finished"
				p.ProgramDurationMs = 60_000
			})
		},
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
		SetRecordingRemovalTimes: func(t *testing.T, recordingID int64, deletedAt, purgedAt, supersededAt *time.Time) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = $2, purged_at = $3, superseded_at = $4 WHERE id = $1`, recordingID, deletedAt, purgedAt, supersededAt); err != nil {
				t.Fatalf("setting removal times for recording %d: %v", recordingID, err)
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

func newMediaContractAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, profile *string, relPath, state string) int64 {
	t.Helper()
	id := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.Kind = kind
		fixtureParams.Profile = profile
		fixtureParams.RelPath = relPath
	})
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, id); err != nil {
		t.Fatalf("setting media asset copy store to tokyo: %v", err)
	}

	if state != "active" {
		if _, err := pool.Exec(context.Background(), `
UPDATE media_assets SET state = $1, deleted_at = CASE WHEN $1 = 'deleted' THEN now() ELSE NULL END WHERE id = $2`, state, id); err != nil {
			t.Fatalf("setting media asset state to %s: %v", state, err)
		}
		if _, err := pool.Exec(context.Background(), `
UPDATE media_asset_copies
SET state = $1, deleted_at = CASE WHEN $1 = 'deleted' THEN now() ELSE NULL END
WHERE media_asset_id = $2`, state, id); err != nil {
			t.Fatalf("setting media asset copy state to %s: %v", state, err)
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
			TempRelPath: fmt.Sprintf("sites/%s/.rokuban-ingest-contract", store.DefaultSite), TempGeneration: 1,
		}); err != nil {
			t.Fatalf("seeding ingest progress for recording %d: %v", recordingID, err)
		}
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
		dbtest.SetMediaAssetState(t, pool, assetID, state)
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
