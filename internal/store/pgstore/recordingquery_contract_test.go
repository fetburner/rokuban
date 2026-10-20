package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
				o.Site = db.DefaultSite
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
			id, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
				RuleID:            o.RuleID,
				Source:            o.Source,
				Site:              o.Site,
				NetworkID:         o.NetworkID,
				ServiceID:         o.ServiceID,
				EventID:           o.EventID,
				ServiceName:       "契約テスト局",
				ChannelType:       o.ChannelType,
				Channel:           "27",
				Title:             o.Title,
				Description:       description,
				Genres:            genres,
				ProgramStartAt:    o.StartAt,
				ProgramDurationMs: 30 * time.Minute.Milliseconds(),
				Status:            o.Status,
			})
			if err != nil {
				t.Fatalf("creating fixture recording: %v", err)
			}
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
			if o.Watched {
				if _, err := pool.Exec(ctx, `INSERT INTO recording_watched (recording_id) VALUES ($1)`, id); err != nil {
					t.Fatalf("marking fixture recording watched: %v", err)
				}
			}
			if o.Playable {
				if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
VALUES ($1, 'original', $2, 100)`, id, fmt.Sprintf("contract/%s/%d.ts", o.Site, id)); err != nil {
					t.Fatalf("creating fixture original asset: %v", err)
				}
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
			return id
		}
		return contracttest.RecordingQueryFixture{
			Store:        NewRecordings(pool),
			NewRecording: newRecording,
			RuleID:       ruleID,
			OtherRuleID:  otherRuleID,
		}
	})
}
