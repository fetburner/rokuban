package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEPGContract(t *testing.T) {
	contracttest.RunEPG(t, func(t *testing.T) contracttest.EPGFixture {
		pool := testutil.SetupDB(t)
		epg := NewEPG(pool)
		return contracttest.EPGFixture{
			Sync:                epg,
			Reader:              epg,
			Search:              epg,
			ValidRegexPattern:   "^(NHK|ニュース)$",
			InvalidRegexPattern: "(?<name>news)",
			CreateRule: func(t *testing.T, options contracttest.EPGRuleOptions) int64 {
				t.Helper()
				var ruleID int64
				if err := pool.QueryRow(context.Background(), `
INSERT INTO rules (name, is_free, duration_min_ms, duration_max_ms)
VALUES ($1, $2, $3, $4)
RETURNING id`, options.Name, options.IsFree, options.DurationMinMs, options.DurationMaxMs).Scan(&ruleID); err != nil {
					t.Fatalf("creating rule fixture: %v", err)
				}
				for seq, match := range options.TextMatches {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, ruleID, seq, match.Target, match.Mode, match.Value, match.CaseSensitive, match.Negate); err != nil {
						t.Fatalf("creating rule text match: %v", err)
					}
				}
				for _, service := range options.Services {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_services (rule_id, network_id, service_id)
VALUES ($1, $2, $3)`, ruleID, service.NetworkID, service.ServiceID); err != nil {
						t.Fatalf("creating rule service: %v", err)
					}
				}
				for _, channelType := range options.ChannelTypes {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_channel_types (rule_id, channel_type)
VALUES ($1, $2)`, ruleID, channelType); err != nil {
						t.Fatalf("creating rule channel type: %v", err)
					}
				}
				for _, genre := range options.Genres {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_genres (rule_id, genre_lv1)
VALUES ($1, $2)`, ruleID, genre); err != nil {
						t.Fatalf("creating rule genre: %v", err)
					}
				}
				for seq, window := range options.Times {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec)
VALUES ($1, $2, $3, $4, $5)`, ruleID, seq, window.Weekdays, window.StartSec, window.EndSec); err != nil {
						t.Fatalf("creating rule time: %v", err)
					}
				}
				for _, site := range options.Sites {
					if _, err := pool.Exec(context.Background(), `
INSERT INTO rule_sites (rule_id, site)
VALUES ($1, $2)`, ruleID, site); err != nil {
						t.Fatalf("creating rule site: %v", err)
					}
				}
				return ruleID
			},
			MarkStale: func(t *testing.T, site string, serviceIDs []int32, programIDs []int64) {
				t.Helper()
				if len(serviceIDs) > 0 {
					if _, err := pool.Exec(context.Background(), `
UPDATE epg_services SET observed_at = '1970-01-01T00:00:00Z'
WHERE site = $1 AND service_id = ANY($2::integer[])`, site, serviceIDs); err != nil {
						t.Fatalf("backdating EPG services: %v", err)
					}
				}
				if len(programIDs) > 0 {
					if _, err := pool.Exec(context.Background(), `
UPDATE epg_programs SET observed_at = '1970-01-01T00:00:00Z'
WHERE site = $1 AND program_id = ANY($2::bigint[])`, site, programIDs); err != nil {
						t.Fatalf("backdating EPG programs: %v", err)
					}
				}
			},
			ObserveNotification: func(t *testing.T, notify func()) string {
				t.Helper()
				ctx := context.Background()
				listener, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Release()
				if _, err := listener.Exec(ctx, "LISTEN rokuban"); err != nil {
					t.Fatal(err)
				}
				notify()
				waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				notification, err := listener.Conn().WaitForNotification(waitCtx)
				if err != nil {
					t.Fatal(err)
				}
				if notification.Channel != "rokuban" {
					t.Fatalf("notification channel = %q, want rokuban", notification.Channel)
				}
				return notification.Payload
			},
			CreateRecording: func(t *testing.T, p store.EPGProgramUpsert, status string, labelRuleID *int64) int64 {
				t.Helper()
				ctx := context.Background()
				var id int64
				if err := pool.QueryRow(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
                        title, program_start_at, program_duration_ms, status)
VALUES ('manual', $1, $2, $3, $4, '局', 'GR', '27', $5, $6, $7, $8)
RETURNING id`, p.Site, p.NetworkID, p.ServiceID, p.EventID, p.Name, p.StartAt, p.DurationMs, status).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if labelRuleID != nil {
					if _, err := pool.Exec(ctx, `
INSERT INTO label_rule_hits (recording_id, label_rule_id) VALUES ($1, $2)
ON CONFLICT (recording_id) DO UPDATE SET label_rule_id = EXCLUDED.label_rule_id`, id, *labelRuleID); err != nil {
						t.Fatal(err)
					}
				}
				return id
			},
			CreateSeriesLabel: func(t *testing.T, keyword, value string, priority int) int64 {
				t.Helper()
				var id int64
				if err := pool.QueryRow(context.Background(), `
INSERT INTO label_rules (key, keyword, value, priority) VALUES ('series', $1, $2, $3) RETURNING id`,
					keyword, value, priority).Scan(&id); err != nil {
					t.Fatal(err)
				}
				return id
			},
			SetIntent: func(t *testing.T, site string, programID int64, action string) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `
INSERT INTO program_snapshots (site, program_id, start_at, duration_ms, network_id, service_id, event_id,
                               service_name, channel_type, channel)
SELECT site, program_id, start_at, duration_ms, network_id, service_id, event_id, '局', 'GR', '27'
FROM epg_programs WHERE site = $1 AND program_id = $2`, site, programID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `
INSERT INTO program_intents (site, program_id, action) VALUES ($1, $2, $3)`, site, programID, action); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}
