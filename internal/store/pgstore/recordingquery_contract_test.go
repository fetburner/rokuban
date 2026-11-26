package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
			seedRecordingQueryShapes(t, ctx, pool, id, o)
			return id
		}
		arrangeUntilEncoded := func(t *testing.T, recordingID int64) {
			arrangeRecordingQueryUntilEncoded(t, pool, recordingID)
		}
		return contracttest.RecordingQueryFixture{
			Store:               NewRecordings(pool, dbtest.StoreName),
			NewRecording:        newRecording,
			RuleID:              ruleID,
			OtherRuleID:         otherRuleID,
			StoreWithoutName:    NewRecordings(pool, ""),
			ArrangeUntilEncoded: arrangeUntilEncoded,
		}
	})
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
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles)
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
		INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store)
VALUES ($1, $2, $3, 'sites/default/.rokuban-ingest-query-contract', 1, 'test-store')`, id, *options.IngestWrittenBytes, options.IngestExpectedBytes); err != nil {
			t.Fatalf("creating fixture ingest progress: %v", err)
		}
	}
}

// seedRecordingQueryShapes は CM 区間・チャプター・encoded 版（保持区間つき）を仕込む。
// int8multirange の literal は整数だけから作るので、埋め込んでも注入の余地は無い。
func seedRecordingQueryShapes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, options contracttest.RecordingQueryRecording) {
	t.Helper()
	if len(options.CMRanges) > 0 {
		if _, err := pool.Exec(ctx, `INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES ($1, '`+multirangeLiteral(options.CMRanges)+`'::int8multirange)`, id); err != nil {
			t.Fatalf("creating fixture CM detection: %v", err)
		}
	}
	if len(options.ChapterSpans) > 0 {
		if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, id); err != nil {
			t.Fatalf("creating fixture chapter ownership: %v", err)
		}
		for _, span := range options.ChapterSpans {
			var label *string
			if span.Label != "" {
				label = &span.Label
			}
			if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), $4, $5)`,
				id, span.StartMs, span.EndMs, label, span.Cut); err != nil {
				t.Fatalf("creating fixture chapter span: %v", err)
			}
		}
	}
	for _, asset := range options.EncodedAssets {
		profile := asset.Profile
		assetID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) {
			p.Kind = "encoded"
			p.Profile = &profile
			p.RelPath = fmt.Sprintf("contract/%d/%s.mp4", id, profile)
			p.SizeBytes = asset.SizeBytes
		})
		if asset.KeepRanges != nil {
			if _, err := pool.Exec(ctx, `INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, '`+multirangeLiteral(asset.KeepRanges)+`'::int8multirange)`, assetID); err != nil {
				t.Fatalf("creating fixture keep ranges for %q: %v", profile, err)
			}
		}
	}
}

// multirangeLiteral は [start, end) の区間の並びを int8multirange の文字列にする。
func multirangeLiteral(ranges [][2]int64) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = fmt.Sprintf("[%d,%d)", r[0], r[1])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// arrangeRecordingQueryUntilEncoded は原本のコピーを外し、別の置き場に encoded 版だけを残す（契約の ArrangeUntilEncoded）。
func arrangeRecordingQueryUntilEncoded(t *testing.T, pool *pgxpool.Pool, recordingID int64) {
	t.Helper()
	ctx := context.Background()
	var originalID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, recordingID).Scan(&originalID); err != nil {
		t.Fatalf("finding fixture original: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = $2`, originalID, dbtest.StoreName); err != nil {
		t.Fatalf("deactivating original copy after until_encoded: %v", err)
	}
	profile := "api-only"
	encodedID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = &profile
		p.RelPath = "api-only/encoded.mp4"
		p.SizeBytes = 5678
	})
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 's3-derived' WHERE media_asset_id = $1 AND store = $2`, encodedID, dbtest.StoreName); err != nil {
		t.Fatalf("moving encoded copy to S3 derived store: %v", err)
	}
}
