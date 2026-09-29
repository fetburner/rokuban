package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// createEpgProgram は EPG 番組を 1 行作る。series_key は生成列なので渡さない。
func createEpgProgram(t *testing.T, pool *pgxpool.Pool, programID int64, name string) {
	t.Helper()
	start := time.Now()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO epg_programs (site, program_id, network_id, service_id, event_id, start_at, duration_ms, end_at,
                          is_free, name, description)
VALUES ($1, $2, 1, 1, 1, $3, 1000, $4, true, $5, '')`,
		DefaultSite, programID, start, start.Add(time.Second), name); err != nil {
		t.Fatalf("creating epg program %q: %v", name, err)
	}
}

// epgSeriesValue は epg_program_series ビューから実効シリーズを読む。
func epgSeriesValue(t *testing.T, pool *pgxpool.Pool, programID int64) string {
	t.Helper()
	var got *string
	if err := pool.QueryRow(context.Background(),
		"SELECT value FROM epg_program_series WHERE site = $1 AND program_id = $2",
		DefaultSite, programID).Scan(&got); err != nil {
		t.Fatalf("reading epg_program_series for %d: %v", programID, err)
	}
	if got == nil {
		return "<NULL>"
	}
	return *got
}

// epg_program_series は録画側の recording_series と同じ規則（分類ルールが当たれば
// その値、当たらなければ自動キー）を EPG 側で表す。ハブの「次回」はこの値と
// recording_series の値を等号で比べるので、空間が一致していなければ何も返らない。
func TestEpgProgramSeries_MatchesTheRecordingSideRule(t *testing.T) {
	pool := setupTestDB(t)

	winnerID := createLabelRule(t, pool, "古典", "日本史", 1)
	createLabelRule(t, pool, "NHK高校講座", "日本史", 0)

	createEpgProgram(t, pool, 1, "NHK高校講座　日本史　第1回")
	createEpgProgram(t, pool, 2, "アニメ　作品X　第1話")
	createEpgProgram(t, pool, 3, "【特集】")

	if got := epgSeriesValue(t, pool, 1); got != "古典" {
		t.Errorf("series for a program a rule matches = %q, want the winner's value 古典", got)
	}
	if got := epgSeriesValue(t, pool, 2); got != "作品X" {
		t.Errorf("series for a program no rule matches = %q, want the automatic key 作品X", got)
	}
	if got := epgSeriesValue(t, pool, 3); got != "<NULL>" {
		t.Errorf("series for a title with no automatic key = %q, want NULL", got)
	}

	// 勝者を消すと自動キーへ落ちる（EPG 側は当たりの表を持たないので、次の
	// 問い合わせで即座に変わる）。
	if _, err := sqlcgen.New(pool).DeleteLabelRule(context.Background(), winnerID); err != nil {
		t.Fatalf("deleting the winner: %v", err)
	}
	if got := epgSeriesValue(t, pool, 1); got != "NHK高校講座" {
		t.Errorf("after deleting the winner, series = %q, want the remaining rule's value", got)
	}
}

// **ビューの絞り込み式（LIKE）は label_rule_winner の照合より狭くなっては
// ならない。** ビューは trgm 索引を使うために LIKE を join 条件として書き下して
// おり（式を label_rule_winner の中だけに置くと 136,053 行で 1.30 s かかる）、
// これが勝者の照合より狭いと、勝者が居るのに枝から漏れる番組が出る --- 結果が
// 減るだけで例外は出ない。広い側にずれる分には遅くなるだけで結果は変わらない。
//
// 正規化が効く例（全角の番組名に対する半角のキーワード）を入れてあるのは、
// この向きのずれを検出するためである。期待値はリテラルで書く。
func TestEpgProgramSeries_LikeCoversEveryMatchTheWinnerFunctionFinds(t *testing.T) {
	pool := setupTestDB(t)

	createLabelRule(t, pool, "進捗", "100%", 0)
	createLabelRule(t, pool, "アニメ枠", "アニメ_", 0)
	createLabelRule(t, pool, "英字", "ABC", 0)

	cases := []struct {
		programID int64
		name      string
		want      string
	}{
		{1, "特集 100% 達成", "進捗"},
		// % がメタ文字として効くと、当たってはならない番組が当たる。
		{2, "特集 100x 達成", "特集"},
		{3, "アニメ_特別編", "アニメ枠"},
		// _ がメタ文字として効くと、こちらの番組も当たってしまう。
		{4, "アニメX特別編", "アニメX特別編"},
		{5, "アニメ　進捗レポート　第1回", "進捗レポート"},
		// 番組名は全角、キーワードは半角。正規化を通さない照合では当たらない。
		{6, "ＡＢＣ特集", "英字"},
		// キーワード ABC は半角で、番組名は小文字。大文字小文字の畳み込みで当たる。
		{7, "abc特集", "英字"},
	}
	for _, tc := range cases {
		createEpgProgram(t, pool, tc.programID, tc.name)
		if got := epgSeriesValue(t, pool, tc.programID); got != tc.want {
			t.Errorf("series for %q = %q, want %q", tc.name, got, tc.want)
		}
	}

	// 勝者関数と突き合わせる（式のずれを検出するための交差検査）。
	for _, tc := range cases {
		var want *string
		err := pool.QueryRow(context.Background(), `
SELECT COALESCE(lr.value_key, p.series_key)
FROM epg_programs p
LEFT JOIN label_rules lr ON lr.id = public.label_rule_winner(p.name)
WHERE p.site = $1 AND p.program_id = $2`, DefaultSite, tc.programID).Scan(&want)
		if err != nil {
			t.Fatalf("reading the winner's value for %q: %v", tc.name, err)
		}
		if want == nil {
			t.Fatalf("the winner's value for %q is NULL", tc.name)
		}
		if got := epgSeriesValue(t, pool, tc.programID); got != *want {
			t.Errorf("epg_program_series for %q = %q, label_rule_winner says %q", tc.name, got, *want)
		}
	}
}
