package rulequery

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestMatchProgramIDs_Integration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	_ = sqlcgen.New(pool)

	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ('default', 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*3600))
	inserts := []struct {
		id   int64
		off  time.Duration
		free bool
		name string
		g    string
	}{
		{1001, 0, true, "ニュース7", "{0}"},
		{1002, time.Hour, true, "アニメスペシャル", "{7}"},
		{1003, 2 * time.Hour, false, "映画", "{6}"},
	}
	for _, row := range inserts {
		st := start.Add(row.off)
		end := st.Add(30 * time.Minute)
		_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', $1::bigint, 32736, 1024, $2::integer, $3::timestamptz, 1800000, $4::timestamptz, $5::boolean, $6::text, '', $7::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`,
			row.id, int32(row.id), st, end, row.free, row.name, row.g)
		if err != nil {
			t.Fatal(err)
		}
	}

	// keyword ニュース
	ids, err := MatchProgramIDs(ctx, pool, "default", Conditions{
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "ニュース"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1001 {
		t.Fatalf("keyword match = %v, want [1001]", ids)
	}

	// genre 7
	ids, err = MatchProgramIDs(ctx, pool, "default", Conditions{Genres: []int16{7}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1002 {
		t.Fatalf("genre match = %v, want [1002]", ids)
	}

	// is_free false
	f := false
	ids, err = MatchProgramIDs(ctx, pool, "default", Conditions{IsFree: &f})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1003 {
		t.Fatalf("is_free match = %v, want [1003]", ids)
	}

	// channel type GR via join
	ids, err = MatchProgramIDs(ctx, pool, "default", Conditions{ChannelTypes: []string{"GR"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("channel type GR = %v, want 3 programs", ids)
	}

	// BS → none
	ids, err = MatchProgramIDs(ctx, pool, "default", Conditions{ChannelTypes: []string{"BS"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("channel type BS = %v, want empty", ids)
	}

	// same conditions twice → identical sets (検索と ruler の一致の核)
	c := Conditions{
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "アニメ"}},
		Genres:      []int16{7},
	}
	a, err := MatchProgramIDs(ctx, pool, "default", c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MatchProgramIDs(ctx, pool, "default", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || (len(a) == 1 && a[0] != b[0]) {
		t.Fatalf("idempotent match diverged: %v vs %v", a, b)
	}

	// 全角キーワードが半角番組名にマッチする（normalize_search_text）
	_, err = pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ('default', 1004, 32736, 1024, 4, $1::timestamptz, 1800000, $2::timestamptz, true, 'NHKニュース', '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`, start.Add(3*time.Hour), start.Add(3*time.Hour+30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ids, err = MatchProgramIDs(ctx, pool, "default", Conditions{
		TextMatches: []TextMatch{{Target: "name", Mode: "keyword", Value: "ＮＨＫ"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1004 {
		t.Fatalf("fullwidth keyword match = %v, want [1004]", ids)
	}
}
