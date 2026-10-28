package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRecordingQueryContract(t *testing.T) {
	contracttest.RunRecordingQuery(t, func(t *testing.T) contracttest.RecordingQueryFixture {
		pool := testutil.SetupDB(t)
		ctx := context.Background()
		newRule := func(name string) int64 {
			t.Helper()
			var id int64
			if err := pool.QueryRow(ctx, `INSERT INTO rules (name) VALUES ($1) RETURNING id`, name).Scan(&id); err != nil {
				t.Fatalf("creating fixture rule: %v", err)
			}
			return id
		}
		ruleID := newRule("recording query contract rule")
		otherRuleID := newRule("other recording query contract rule")
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, o contracttest.RecordingQueryRecording) int64 {
			t.Helper()
			if o.Title == "" {
				o.Title = "recording query fixture"
			}
			if o.Site == "" {
				o.Site = store.DefaultSite
			}
			if o.ChannelType == "" {
				o.ChannelType = "GR"
			}
			if o.NetworkID == 0 {
				o.NetworkID = 32678
			}
			if o.ServiceID == 0 {
				o.ServiceID = 5168
			}
			if o.EventID == 0 {
				o.EventID = eventSequence.Add(1)
			}
			if o.StartAt.IsZero() {
				o.StartAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			}
			if o.Status == "" {
				o.Status = "finished"
			}
			if o.Source == "" {
				o.Source = "manual"
			}
			var description *string
			if o.Description != "" {
				description = &o.Description
			}
			var genres json.RawMessage
			if len(o.Genres) > 0 {
				type genre struct {
					LV1 int `json:"lv1"`
					LV2 int `json:"lv2"`
					UN1 int `json:"un1"`
					UN2 int `json:"un2"`
				}
				values := make([]genre, len(o.Genres))
				for i, lv1 := range o.Genres {
					values[i] = genre{LV1: int(lv1)}
				}
				encoded, err := json.Marshal(values)
				if err != nil {
					t.Fatalf("encoding fixture genres: %v", err)
				}
				genres = encoded
			}
			id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
				fixtureParams.RuleID = o.RuleID
				fixtureParams.Source = o.Source
				fixtureParams.Site = o.Site
				fixtureParams.NetworkID = o.NetworkID
				fixtureParams.ServiceID = o.ServiceID
				fixtureParams.EventID = o.EventID
				fixtureParams.ServiceName = "契約テスト局"
				fixtureParams.ChannelType = o.ChannelType
				fixtureParams.Title = o.Title
				fixtureParams.Description = description
				fixtureParams.Genres = genres
				fixtureParams.ProgramStartAt = o.StartAt
				fixtureParams.Status = o.Status
			})

			if o.Trashed || o.Purged {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("trashing fixture recording: %v", err)
				}
			}
			if o.Purged {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET purged_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("purging fixture recording: %v", err)
				}
			}
			if o.Superseded {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET superseded_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("superseding fixture recording: %v", err)
				}
			}
			if o.WatchedAt == nil && o.Watched {
				if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, id); err != nil {
					t.Fatalf("marking fixture recording watched: %v", err)
				}
			}
			if o.Playable {
				dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
					p.RelPath = fmt.Sprintf("contract/%s/%d.ts", o.Site, id)
					p.SizeBytes = 100
				})
			}
			if o.PlaybackPositionMs != nil {
				updatedAt := time.Now().UTC()
				if o.PlaybackUpdatedAt != nil {
					updatedAt = *o.PlaybackUpdatedAt
				}
				if _, err := pool.Exec(ctx, `
INSERT INTO recording_playback_positions (recording_id, position_ms, updated_at)
VALUES ($1, $2, $3)`, id, *o.PlaybackPositionMs, updatedAt); err != nil {
					t.Fatalf("creating fixture playback position: %v", err)
				}
			}
			seedRecordingQueryReadbackFields(t, ctx, pool, id, o)
			return id
		}
		return contracttest.RecordingQueryFixture{
			Store:        NewRecordings(pool, dbtest.StoreName),
			NewRecording: newRecording,
			RuleID:       ruleID,
			OtherRuleID:  otherRuleID,
		}
	})
}

func TestRecordingQueryWithoutStoreNameUsesActiveCopies(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.Title = "API only copy visibility fixture"
	})
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "api-only/original.ts"
		p.SizeBytes = 1234
	})
	profile := "api-only"
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "api-only/encoded.mp4"
		p.SizeBytes = 5678
	})

	query := NewRecordings(pool, "")
	row, err := query.Get(ctx, recordingID)
	if err != nil {
		t.Fatalf("Get(%d) without a configured store: %v", recordingID, err)
	}
	if row.OriginalSizeBytes == nil || *row.OriginalSizeBytes != 1234 {
		t.Fatalf("original size = %v, want 1234 from an active copy in any store", row.OriginalSizeBytes)
	}
	var available []struct {
		Profile   string `json:"profile"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(row.AvailableEncodedAssets, &available); err != nil {
		t.Fatalf("decoding available encoded assets %s: %v", row.AvailableEncodedAssets, err)
	}
	if len(available) != 1 || available[0].Profile != profile || available[0].SizeBytes != 5678 {
		t.Fatalf("available encoded assets = %+v, want %s at 5678 bytes", available, profile)
	}

	rows, err := query.List(ctx, store.RecordingFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List() without a configured store: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != recordingID || rows[0].OriginalSizeBytes == nil || *rows[0].OriginalSizeBytes != 1234 {
		t.Fatalf("List() = %+v, want recording %d with original size 1234", rows, recordingID)
	}

	shelves, err := query.Shelves(ctx, store.RecordingFilter{})
	if err != nil {
		t.Fatalf("Shelves() without a configured store: %v", err)
	}
	if len(shelves) != 1 || shelves[0].PlayableCount != 1 {
		t.Fatalf("Shelves() = %+v, want one playable recording in its shelf", shelves)
	}
}

func seedRecordingQueryReadbackFields(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, options contracttest.RecordingQueryRecording) {
	t.Helper()
	if options.WatchedAt != nil {
		if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id, watched_at) VALUES ($1, $2)`, id, *options.WatchedAt); err != nil {
			t.Fatalf("marking fixture recording watched at a fixed time: %v", err)
		}
	}
	if options.KeepOriginal != "" || options.EncodeProfiles != nil {
		keepOriginal := options.KeepOriginal
		if keepOriginal == "" {
			keepOriginal = "always"
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles)
VALUES ($1, $2, $3)`, id, keepOriginal, options.EncodeProfiles); err != nil {
			t.Fatalf("creating fixture encode policy: %v", err)
		}
	}
	for _, attempt := range options.EncodeAttempts {
		if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempt_count)
VALUES ($1, $2, $3, 1)`, id, attempt.Profile, attempt.State); err != nil {
			t.Fatalf("creating fixture encode attempt for %q: %v", attempt.Profile, err)
		}
	}
	if options.IngestWrittenBytes != nil {
		if _, err := pool.Exec(ctx, `
		INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation)
VALUES ($1, $2, $3, 'sites/default/.rokuban-ingest-query-contract', 1)`, id, *options.IngestWrittenBytes, options.IngestExpectedBytes); err != nil {
			t.Fatalf("creating fixture ingest progress: %v", err)
		}
	}
}
