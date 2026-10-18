package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestEPGContract(t *testing.T) {
	contracttest.RunEPG(t, func(t *testing.T) contracttest.EPGFixture {
		pool := testutil.SetupDB(t)
		epg := NewEPG(pool)
		return contracttest.EPGFixture{
			Sync:   epg,
			Reader: epg,
			Search: epg,
			CreateRule: func(t *testing.T, options contracttest.EPGRuleOptions) int64 {
				t.Helper()
				var isFree, durationMin, durationMax any
				if options.IsFree != nil {
					isFree = *options.IsFree
				}
				if options.DurationMinMs != nil {
					durationMin = *options.DurationMinMs
				}
				if options.DurationMaxMs != nil {
					durationMax = *options.DurationMaxMs
				}
				var ruleID int64
				if err := pool.QueryRow(context.Background(), `
INSERT INTO rules (name, is_free, duration_min_ms, duration_max_ms)
VALUES ($1, $2, $3, $4)
RETURNING id`, options.Name, isFree, durationMin, durationMax).Scan(&ruleID); err != nil {
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
		}
	})
}

func TestEPGNotifyProgramsUpdated(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	listener, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN rokuban"); err != nil {
		t.Fatalf("listening for notifications: %v", err)
	}
	if err := NewEPG(pool).NotifyProgramsUpdated(ctx); err != nil {
		t.Fatalf("notifying programs updated: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	notification, err := listener.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("waiting for EPG notification: %v", err)
	}
	if notification.Channel != "rokuban" || notification.Payload != "epg" {
		t.Fatalf("notification = %s/%s, want rokuban/epg", notification.Channel, notification.Payload)
	}
}
