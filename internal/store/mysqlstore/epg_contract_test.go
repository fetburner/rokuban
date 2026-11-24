package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/serieskey"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestEPGContract(t *testing.T) {
	contracttest.RunEPG(t, func(t *testing.T) contracttest.EPGFixture {
		db := mysqltest.SetupDB(t)
		epg := NewEPG(db)
		return contracttest.EPGFixture{
			Sync:                epg,
			Reader:              epg,
			Search:              epg,
			ValidRegexPattern:   "^(NHK|ニュース)$",
			InvalidRegexPattern: "[",
			CreateRule: func(t *testing.T, options contracttest.EPGRuleOptions) int64 {
				t.Helper()
				return createEPGRule(t, db, options)
			},
			MarkStale: func(t *testing.T, site string, serviceIDs []int32, programIDs []int64) {
				t.Helper()
				for _, id := range serviceIDs {
					execEPGFixture(t, db, `
UPDATE epg_services SET observed_at = '1970-01-01 00:00:00' WHERE site = ? AND service_id = ?`, site, id)
				}
				for _, id := range programIDs {
					execEPGFixture(t, db, `
UPDATE epg_programs SET observed_at = '1970-01-01 00:00:00' WHERE site = ? AND program_id = ?`, site, id)
				}
			},
			ObserveNotification: func(t *testing.T, notify func()) string {
				t.Helper()
				notify()
				var topic string
				var count int
				if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*), COALESCE(MIN(payload), '') FROM notifications").
					Scan(&count, &topic); err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Fatalf("notifications = %d, want 1", count)
				}
				return topic
			},
			CreateRecording: func(t *testing.T, p store.EPGProgramUpsert, status string, labelRuleID *int64) int64 {
				t.Helper()
				var series any
				if key, ok := serieskey.SeriesKey(p.Name); ok {
					series = key
				}
				id := epgFixtureID(t, execEPGFixture(t, db, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
                        title, program_start_at, program_duration_ms, status, series_key)
VALUES ('manual', ?, ?, ?, ?, '局', 'GR', '27', ?, ?, ?, ?, ?)`,
					p.Site, p.NetworkID, p.ServiceID, p.EventID, p.Name, p.StartAt, p.DurationMs, status, series))
				if labelRuleID != nil {
					execEPGFixture(t, db, "INSERT INTO label_rule_hits (recording_id, label_rule_id) VALUES (?, ?)", id, *labelRuleID)
				}
				return id
			},
			CreateSeriesLabel: func(t *testing.T, keyword, value string, priority int) int64 {
				t.Helper()
				key, ok := serieskey.SeriesKey(value)
				if !ok {
					t.Fatalf("invalid series label %q", value)
				}
				return epgFixtureID(t, execEPGFixture(t, db,
					"INSERT INTO label_rules (`key`, keyword, value, priority, value_key) VALUES ('series', ?, ?, ?, ?)",
					keyword, value, priority, key))
			},
			SetIntent: func(t *testing.T, site string, programID int64, action string) {
				t.Helper()
				execEPGFixture(t, db, `
INSERT INTO program_snapshots (site, program_id, start_at, duration_ms, network_id, service_id, event_id,
                               service_name, channel_type, channel)
SELECT site, program_id, start_at, duration_ms, network_id, service_id, event_id, '局', 'GR', '27'
FROM epg_programs WHERE site = ? AND program_id = ?`, site, programID)
				execEPGFixture(t, db, "INSERT INTO program_intents (site, program_id, action) VALUES (?, ?, ?)", site, programID, action)
			},
		}
	})
}

func execEPGFixture(t *testing.T, db *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	result, err := db.ExecContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("writing EPG fixture: %v", err)
	}
	return result
}

func epgFixtureID(t *testing.T, result sql.Result) int64 {
	t.Helper()
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("reading fixture ID: %v", err)
	}
	return id
}

func createEPGRule(t *testing.T, db *sql.DB, options contracttest.EPGRuleOptions) int64 {
	t.Helper()
	id := epgFixtureID(t, execEPGFixture(t, db, `
INSERT INTO rules (name, is_free, duration_min_ms, duration_max_ms) VALUES (?, ?, ?, ?)`,
		options.Name, options.IsFree, options.DurationMinMs, options.DurationMaxMs))
	for seq, match := range options.TextMatches {
		execEPGFixture(t, db, `
INSERT INTO rule_text_matches (rule_id, seq, target, mode, value, case_sensitive, negate)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, seq, match.Target, match.Mode, match.Value, match.CaseSensitive, match.Negate)
	}
	for _, service := range options.Services {
		execEPGFixture(t, db, "INSERT INTO rule_services (rule_id, network_id, service_id) VALUES (?, ?, ?)",
			id, service.NetworkID, service.ServiceID)
	}
	for _, channelType := range options.ChannelTypes {
		execEPGFixture(t, db, "INSERT INTO rule_channel_types (rule_id, channel_type) VALUES (?, ?)", id, channelType)
	}
	for _, genre := range options.Genres {
		execEPGFixture(t, db, "INSERT INTO rule_genres (rule_id, genre_lv1) VALUES (?, ?)", id, genre)
	}
	for seq, window := range options.Times {
		execEPGFixture(t, db, "INSERT INTO rule_times (rule_id, seq, weekdays, start_sec, end_sec) VALUES (?, ?, ?, ?, ?)",
			id, seq, window.Weekdays, window.StartSec, window.EndSec)
	}
	for _, site := range options.Sites {
		execEPGFixture(t, db, "INSERT INTO rule_sites (rule_id, site) VALUES (?, ?)", id, site)
	}
	return id
}

func TestEPGUpsertWritesSeriesKey(t *testing.T) {
	db := mysqltest.SetupDB(t)
	epg := NewEPG(db)
	ctx := context.Background()
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	insert := func(t *testing.T, id int64, name string) {
		t.Helper()
		program := store.EPGProgramUpsert{
			Site: "default", ProgramID: id, NetworkID: 1, ServiceID: 1, EventID: int32(id),
			StartAt: start, EndAt: start.Add(time.Minute), DurationMs: 60_000, Name: name,
		}
		if err := epg.UpsertPrograms(ctx, []store.EPGProgramUpsert{program}); err != nil {
			t.Fatalf("upserting %q: %v", name, err)
		}
	}
	assertKey := func(t *testing.T, id int64, name string) {
		t.Helper()
		want, wantValid := serieskey.SeriesKey(name)
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `
SELECT series_key FROM epg_programs WHERE site = 'default' AND program_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.Valid != wantValid || got.String != want {
			t.Fatalf("series_key for %q = %+v, want %q (valid=%t)", name, got, want, wantValid)
		}
	}
	t.Run("insert derives the key", func(t *testing.T) {
		const name = "［新］ＡＢＣ 第1話"
		insert(t, 1, name)
		assertKey(t, 1, name)
	})
	t.Run("renaming replaces and clears the key", func(t *testing.T) {
		insert(t, 2, "旧番組 第1話")
		const renamed = "新番組 第2話"
		insert(t, 2, renamed)
		assertKey(t, 2, renamed)
		insert(t, 2, "★")
		assertKey(t, 2, "★")
	})
}

func TestRulesNotificationTriggers(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	const ruleID = 100
	statements := []struct {
		name  string
		query string
	}{
		{"insert", "INSERT INTO rules (id, name) VALUES (?, 'initial')"},
		{"update", "UPDATE rules SET name = 'updated' WHERE id = ?"},
		{"delete", "DELETE FROM rules WHERE id = ?"},
	}
	assertNotifications := func(t *testing.T, want int) {
		t.Helper()
		var count, rulesCount int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*), COUNT(CASE WHEN payload = 'rules' THEN 1 END) FROM notifications").
			Scan(&count, &rulesCount); err != nil {
			t.Fatal(err)
		}
		if count != want || rulesCount != want {
			t.Fatalf("notifications = %d (%d rules), want %d rules", count, rulesCount, want)
		}
	}
	rollback := errors.New("rollback notification probe")
	for i, stmt := range statements {
		t.Run(stmt.name, func(t *testing.T) {
			err := runTx(ctx, db, func(tx *sql.Tx) error {
				result, err := tx.ExecContext(ctx, stmt.query, ruleID)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if count != 1 {
					return fmt.Errorf("rule rows affected = %d, want 1", count)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("rolling back rule %s: %v", stmt.name, err)
			}
			assertNotifications(t, i)
			execEPGFixture(t, db, stmt.query, ruleID)
			assertNotifications(t, i+1)
		})
	}
}
