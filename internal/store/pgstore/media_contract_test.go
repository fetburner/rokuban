package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
		Storage:      NewMedia(pool),
		Publisher:    NewMedia(pool),
		Site:         db.DefaultSite,
		NewRecording: func(t *testing.T) int64 { return newMediaContractRecording(t, pool) },
		NewAsset: func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64 {
			return newMediaContractAsset(t, pool, recordingID, kind, profile, relPath, state)
		},
	}
	configureMediaPublishFixture(t, pool, &fixture)
	configureMediaObservationFixture(t, pool, &fixture)
	configureMediaStorageFixture(t, pool, &fixture)
	return fixture
}

func newMediaContractRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              db.DefaultSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           mediaContractEventID.Add(1),
		ServiceName:       "store contract fixture",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "store media contract fixture",
		ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
		ProgramDurationMs: 60_000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	return id
}

func newMediaContractAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind string, profile *string, relPath, state string) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        kind,
		Profile:     profile,
		RelPath:     relPath,
		SizeBytes:   1,
	})
	if err != nil {
		t.Fatalf("seeding %s media asset: %v", kind, err)
	}
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
				Source: "manual", Site: db.DefaultSite, NetworkID: 32678, ServiceID: 5168,
				EventID: mediaContractEventID.Add(1), ServiceName: "in-place contract fixture",
				ChannelType: "GR", Channel: "27", Title: "in-place contract fixture",
				ProgramStartAt: time.Now().UTC().Truncate(time.Second), ProgramDurationMs: 60_000, Status: "finished",
			},
			Assets: []store.InPlaceAsset{{Kind: db.AssetKindOriginal, RelPath: relPath, SizeBytes: 1}},
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
