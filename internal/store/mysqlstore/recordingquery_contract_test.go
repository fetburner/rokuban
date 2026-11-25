package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

// recordingQueryContractStore は契約テストの置き場名。media_asset_copies.store の形（^[a-z0-9-]+$）に合わせる。
const recordingQueryContractStore = "recording-query-store"

func TestRecordingQueryContract(t *testing.T) {
	contracttest.RunRecordingQuery(t, func(t *testing.T) contracttest.RecordingQueryFixture {
		db := mysqltest.SetupDB(t)
		ctx := context.Background()
		newRule := func(name string) int64 {
			t.Helper()
			id, err := execFixture(t, db, `INSERT INTO rules (name) VALUES (?)`, name).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture rule ID: %v", err)
			}
			return id
		}
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, o contracttest.RecordingQueryRecording) int64 {
			t.Helper()
			if o.Title == "" {
				o.Title = "recording query fixture"
			}
			if o.Site == "" {
				o.Site = "contract-site"
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
			genres, genreLv1, err := recordingQueryGenres(o.Genres)
			if err != nil {
				t.Fatalf("encoding fixture genres: %v", err)
			}
			var description any
			if o.Description != "" {
				description = o.Description
			}
			var seriesKey any
			if key, ok := serieskey.SeriesKey(o.Title); ok {
				seriesKey = key
			}
			var deletedAt, supersededAt, purgedAt any
			if o.Trashed || o.Purged {
				deletedAt = time.Now().UTC()
			}
			if o.Superseded {
				supersededAt = time.Now().UTC()
			}
			if o.Purged {
				purgedAt = time.Now().UTC()
			}
			id, err := execFixture(t, db, `
INSERT INTO recordings (rule_id, source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, description, genres, is_free, program_start_at, program_duration_ms,
    status, series_key, genre_lv1, deleted_at, superseded_at, purged_at)
VALUES (?, ?, ?, ?, ?, ?, 'contract service', ?, 'contract channel', ?, ?, ?, TRUE, ?, 1800000, ?, ?, ?, ?, ?, ?)`,
				o.RuleID, o.Source, o.Site, o.NetworkID, o.ServiceID, o.EventID,
				o.ChannelType, o.Title, description, genres, o.StartAt, o.Status, seriesKey, genreLv1,
				deletedAt, supersededAt, purgedAt).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture recording ID: %v", err)
			}
			if o.WatchedAt != nil {
				execFixture(t, db, `INSERT INTO recording_watched (recording_id, watched_at) VALUES (?, ?)`, id, *o.WatchedAt)
			} else if o.Watched {
				execFixture(t, db, `INSERT INTO recording_watched (recording_id) VALUES (?)`, id)
			}
			if o.Playable {
				assetID, err := execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 100)`, id).LastInsertId()
				if err != nil {
					t.Fatalf("reading fixture original ID: %v", err)
				}
				execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation)
VALUES (?, ?, ?, 'active', 0)`, assetID, recordingQueryContractStore, fmt.Sprintf("contract/%s/%d.ts", o.Site, id))
			}
			if o.PlaybackPositionMs != nil {
				updatedAt := time.Now().UTC()
				if o.PlaybackUpdatedAt != nil {
					updatedAt = *o.PlaybackUpdatedAt
				}
				execFixture(t, db, `
INSERT INTO recording_playback_positions (recording_id, position_ms, updated_at) VALUES (?, ?, ?)`,
					id, *o.PlaybackPositionMs, updatedAt)
			}
			if o.KeepOriginal != "" || o.EncodeProfiles != nil {
				keepOriginal := o.KeepOriginal
				if keepOriginal == "" {
					keepOriginal = "always"
				}
				profiles, err := json.Marshal(o.EncodeProfiles)
				if err != nil {
					t.Fatalf("encoding fixture encode profiles: %v", err)
				}
				execFixture(t, db, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles) VALUES (?, ?, ?)`,
					id, keepOriginal, string(profiles))
			}
			for _, attempt := range o.EncodeAttempts {
				execFixture(t, db, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempt_count) VALUES (?, ?, ?, 1)`,
					id, attempt.Profile, attempt.State)
			}
			if o.IngestWrittenBytes != nil {
				execFixture(t, db, `
INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store)
VALUES (?, ?, ?, 'sites/contract-site/.rokuban-ingest-query-contract', 1, ?)`,
					id, *o.IngestWrittenBytes, o.IngestExpectedBytes, recordingQueryContractStore)
			}
			seedRecordingQueryShapes(t, db, id, o)
			return id
		}
		arrangeUntilEncoded := func(t *testing.T, recordingID int64) {
			t.Helper()
			var originalID int64
			if err := db.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE recording_id = ? AND kind = 'original'`, recordingID).Scan(&originalID); err != nil {
				t.Fatalf("finding fixture original: %v", err)
			}
			execFixture(t, db, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = UTC_TIMESTAMP(6) WHERE media_asset_id = ? AND store = ?`,
				originalID, recordingQueryContractStore)
			encodedID, err := execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, profile, size_bytes) VALUES (?, 'encoded', 'api-only', 5678)`, recordingID).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture encoded ID: %v", err)
			}
			execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation)
VALUES (?, 's3-derived', 'api-only/encoded.mp4', 'active', 0)`, encodedID)
		}
		return contracttest.RecordingQueryFixture{
			Store:               NewRecordingQueries(db, recordingQueryContractStore),
			NewRecording:        newRecording,
			RuleID:              newRule("recording query contract rule"),
			OtherRuleID:         newRule("other recording query contract rule"),
			StoreWithoutName:    NewRecordingQueries(db, ""),
			ArrangeUntilEncoded: arrangeUntilEncoded,
		}
	})
}

// recordingQueryGenres は契約の genres（大分類の値）から、genres 列の JSON と genre_lv1 を作る。
// 値が無ければ genres は NULL、genre_lv1 は空配列になる（録画を作る 3 経路と同じ規則）。
func recordingQueryGenres(lv1s []int16) (any, string, error) {
	if len(lv1s) == 0 {
		return nil, epgGenreLevels(json.RawMessage("[]")), nil
	}
	type genre struct {
		LV1 int `json:"lv1"`
		LV2 int `json:"lv2"`
		UN1 int `json:"un1"`
		UN2 int `json:"un2"`
	}
	values := make([]genre, len(lv1s))
	for i, lv1 := range lv1s {
		values[i] = genre{LV1: int(lv1)}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, "", err
	}
	return string(encoded), epgGenreLevels(encoded), nil
}

// seedRecordingQueryShapes は CM 区間・チャプター・encoded 版（保持区間つき）を仕込む。
// 区間は仕込み順のまま JSON にする。読み手が並べ直すことを契約が検査する。
func seedRecordingQueryShapes(t *testing.T, db *sql.DB, id int64, o contracttest.RecordingQueryRecording) {
	t.Helper()
	if len(o.CMRanges) > 0 {
		execFixture(t, db, `INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES (?, ?)`, id, rangesJSON(t, o.CMRanges))
	}
	if len(o.ChapterSpans) > 0 {
		execFixture(t, db, `INSERT INTO recording_chapter_ownership (recording_id) VALUES (?)`, id)
		for _, span := range o.ChapterSpans {
			var label any
			if span.Label != "" {
				label = span.Label
			}
			execFixture(t, db, `INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut) VALUES (?, ?, ?, ?, ?)`,
				id, span.StartMs, span.EndMs, label, span.Cut)
		}
	}
	for _, asset := range o.EncodedAssets {
		assetID, err := execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, profile, size_bytes) VALUES (?, 'encoded', ?, ?)`,
			id, asset.Profile, asset.SizeBytes).LastInsertId()
		if err != nil {
			t.Fatalf("reading fixture encoded asset ID: %v", err)
		}
		execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation)
VALUES (?, ?, ?, 'active', 0)`, assetID, recordingQueryContractStore, fmt.Sprintf("contract/%d/%s.mp4", id, asset.Profile))
		if asset.KeepRanges != nil {
			execFixture(t, db, `INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES (?, ?)`, assetID, rangesJSON(t, asset.KeepRanges))
		}
	}
}

// rangesJSON は [start, end) の区間の並びを {"startMs", "endMs"} の JSON 配列にする（並びは仕込み順のまま）。
func rangesJSON(t *testing.T, ranges [][2]int64) string {
	t.Helper()
	type interval struct {
		StartMs int64 `json:"startMs"`
		EndMs   int64 `json:"endMs"`
	}
	values := make([]interval, len(ranges))
	for i, r := range ranges {
		values[i] = interval{StartMs: r[0], EndMs: r[1]}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("encoding fixture ranges: %v", err)
	}
	return string(encoded)
}
