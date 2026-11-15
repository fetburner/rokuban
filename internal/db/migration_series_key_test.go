package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

// TestSeriesKeyMigrations_RecomputeStoredValues は生成列導入後に作った行を
// 最新まで移行し、関数だけを差し替えて既存行を置き去りにする migration を検出する。
func TestSeriesKeyMigrations_RecomputeStoredValues(t *testing.T) {
	ctx := context.Background()
	dbURL := createScratchDatabase(t, testDatabaseURL(t), "series_key", "TEMPLATE template0")
	// 00015_epg_series.sql で 3 表の生成列が揃った時点で止める。
	// 最新まで適用してから INSERT すると、
	// 関数を差し替える migration に再計算がなくても通ってしまう。
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, 15)
		return err
	}); err != nil {
		t.Fatalf("migrating to series generated columns: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening series migration database: %v", err)
	}
	defer pool.Close()

	// 枠名・先頭記号・話数・NFKC・大小文字・NULL を含める。
	// 3 表とも version 15 のスキーマに対する生 INSERT を使い、
	// 現在の生成クエリやスキーマを前提にする fixture には依存させない。
	titles := []string{
		"アニメ　烏は主を選ばない　第05話", "TVアニメ 呪術廻戦 #12", "日5 機動戦士ガンダム",
		"[新]鬼滅の刃 第2話", "【再】アイドルマスター シャイニーカラーズ",
		"▽ニュース", "◇特集", "作品(3)", "作品4話", "作品第2回", "作品第3週",
		"ＡＢＣ番組", "ABC番組", "アニメ", "【特集】", "　　　", "第5話", "", "アニメ　",
	}
	ruleCount := 0
	for i, title := range titles {
		if _, err := pool.Exec(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name,
                        channel_type, channel, title, program_start_at, program_duration_ms, status,
                        deleted_at, purged_at)
VALUES ('manual', 'default', 1, 1, $1, 'test', 'GR', '27', $2, now(), 1000, 'finished',
        CASE WHEN $1 % 2 = 0 THEN now() END, CASE WHEN $1 % 2 = 0 AND $1 % 3 = 0 THEN now() END)`, i+1, title); err != nil {
			t.Fatalf("seeding recording %q: %v", title, err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id,
                          start_at, duration_ms, end_at, is_free, name, description)
VALUES ('default', $1, 1, 1, 1, now(), 1000, now() + interval '1 second', true, $2, '')`, i+1, title); err != nil {
			t.Fatalf("seeding EPG program %q: %v", title, err)
		}
		// 旧定義で有効な値だけをルールにする。新定義で NULL になる場合は、
		// CHECK を外したりユーザー設定を消したりせず移行を失敗させる。
		if seriesKey(t, pool, title) != nil {
			if _, err := pool.Exec(ctx, `
INSERT INTO label_rules (key, value, keyword, priority)
VALUES ('series', $1, 'keyword', 0)`, title); err != nil {
				t.Fatalf("seeding label rule %q: %v", title, err)
			}
			ruleCount++
		}
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying migrations to existing series values: %v", err)
	}

	for _, tc := range []struct {
		table, column, input string
		wantRows             int
	}{
		{"recordings", "series_key", "title", len(titles)},
		{"epg_programs", "series_key", "name", len(titles)},
		{"label_rules", "value_key", "value", ruleCount},
	} {
		t.Run(tc.table, func(t *testing.T) {
			var total, stale int
			// 識別子は上の固定リテラルだけ。NULL 同士も一致として検査する。
			err := pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE NOT ("+
				tc.column+" IS NOT DISTINCT FROM public.series_key("+tc.input+"))) FROM public."+tc.table).
				Scan(&total, &stale)
			if err != nil {
				t.Fatalf("checking stored series values: %v", err)
			}
			if total != tc.wantRows {
				t.Errorf("row count = %d, want %d (migration must preserve existing rows)", total, tc.wantRows)
			}
			if stale != 0 {
				t.Errorf("%s.%s: %d stale stored values; want every row IS NOT DISTINCT FROM series_key(%s)",
					tc.table, tc.column, stale, tc.input)
			}
		})
	}
}
