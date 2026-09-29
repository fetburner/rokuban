package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// createSeriesRecording は棚のテスト用に録画行を 1 件作る。title だけが主張で、
// 他は CreateRecording が要求する最小の値。
func createSeriesRecording(t *testing.T, pool *pgxpool.Pool, title string, eventID int32, start time.Time) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              DefaultSite,
		NetworkID:         1,
		ServiceID:         1,
		EventID:           eventID,
		ServiceName:       "test",
		ChannelType:       "GR",
		Channel:           "1",
		Title:             title,
		ProgramStartAt:    start,
		ProgramDurationMs: 1000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("creating recording %q: %v", title, err)
	}
	return id
}

// activeMediaAsset は録画に active な media_asset を 1 件足す（棚の母集団の条件）。
func activeMediaAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind, profile string) {
	t.Helper()
	var profileArg any
	if profile != "" {
		profileArg = profile
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
VALUES ($1, $2, $3, $4, 1, 'active')`,
		recordingID, kind, profileArg, fmt.Sprintf("test/%s-%d", kind, recordingID)); err != nil {
		t.Fatalf("inserting %s media asset for recording %d: %v", kind, recordingID, err)
	}
}

// seriesKey は実際の SQL 関数を呼ぶ。テストが期待値を Go 側に写すと、関数の
// 本体を変えたときに写した側だけが古くなる（不変条件 8「実装の定数と比較する
// テストは何も主張していない」の裏返しで、ここは SQL の答えをリテラルと比べる）。
func seriesKey(t *testing.T, pool *pgxpool.Pool, title string) *string {
	t.Helper()
	var got *string
	if err := pool.QueryRow(context.Background(),
		"SELECT public.series_key($1)", title).Scan(&got); err != nil {
		t.Fatalf("calling series_key(%q): %v", title, err)
	}
	return got
}

func ptr[T any](v T) *T { return &v }

// series_key は記号の除去 → 話数の除去 → 枠名を 1 語飛ばしてから最初の空白で
// 切る、の順で棚のキーを作る。空・記号のみのタイトルでは NULL を返す。
//
// 期待値はリテラルで書く（実装と同じ式を書き写すと何も主張しなくなる）。
func TestSeriesKey_ShelfKey(t *testing.T) {
	pool := setupTestDB(t)

	cases := []struct {
		name  string
		title string
		want  *string
	}{
		{"枠名を飛ばして最初の空白で切る", "アニメ　烏は主を選ばない　第05話", ptr("烏は主を選ばない")},
		{"枠名 TVアニメ も飛ばす", "TVアニメ 呪術廻戦 #12", ptr("呪術廻戦")},
		{"枠名 日5 も飛ばす", "日5 機動戦士ガンダム", ptr("機動戦士ガンダム")},
		// 枠名を飛ばすのは「枠名 + 空白 + 作品名」の形だけ。空白が無ければ
		// 枠名は作品名の一部として残る（過剰併合の棚として目に見える形）。
		{"区切りの無い枠名は飛ばさない", "アニメ", ptr("アニメ")},
		{"角括弧を除去する", "[新]鬼滅の刃 第2話", ptr("鬼滅の刃")},
		{"隅付き括弧を除去する", "【再】アイドルマスター シャイニーカラーズ", ptr("アイドルマスター")},
		{"末尾の ー は落とさない", "アイドルマスター", ptr("アイドルマスター")},
		// 記号のみ・空は NULL。NULL を返すことで「何も主張しない棚」を作らない。
		{"記号のみは NULL", "【特集】", nil},
		{"空白のみは NULL", "　　　", nil},
		{"話数だけは NULL", "第5話", nil},
		{"空文字は NULL", "", nil},
		{"枠名だけは NULL", "アニメ　", nil},
		// LIKE の特殊文字は棚のキーとしてそのまま残る（エスケープは読み出し側）。
		{"% はそのまま残る", "ニュース 100%", ptr("ニュース")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := seriesKey(t, pool, tc.title)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("series_key(%q) = %q, want NULL", tc.title, *got)
			case tc.want != nil && got == nil:
				t.Errorf("series_key(%q) = NULL, want %q", tc.title, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("series_key(%q) = %q, want %q", tc.title, *got, *tc.want)
			}
		})
	}
}

// series_key の生成列は「INSERT した行にだけ」書かれる。関数の本体を差し替えても
// 既存行の値は古いまま残り、REINDEX では直らない。列を作り直すテーブル書き換え
// だけが全行を新しい定義へ移す（docs/data/series.md §8「評価結果の持ち方」）。
//
// **これは将来 series_key の本体を変えるマイグレーションの必須手順そのもの**なので、
// 「変えたら列も作り直す」をここで固定する。列を作り直す手順を落とした migration を
// 書いても、このテストは (2) で気付ける。
func TestSeriesKey_GeneratedColumnNeedsRecreateAfterFunctionChange(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	createSeriesRecording(t, pool, "アニメ　テスト作品　第1話", 1, time.Now())

	readKey := func() string {
		var got *string
		if err := pool.QueryRow(ctx,
			"SELECT series_key FROM recordings WHERE event_id = 1").Scan(&got); err != nil {
			t.Fatalf("reading series_key: %v", err)
		}
		if got == nil {
			return "<NULL>"
		}
		return *got
	}

	if got := readKey(); got != "テスト作品" {
		t.Fatalf("series_key = %q, want %q", got, "テスト作品")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	// ロールバックで元に戻すので、本物の関数を壊したままにしない。
	defer func() { _ = tx.Rollback(ctx) }()

	// 関数だけを差し替える（枠名を飛ばさない版に戻す）。
	if _, err := tx.Exec(ctx, `
CREATE OR REPLACE FUNCTION public.series_key(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$ SELECT t $$`); err != nil {
		t.Fatalf("replacing series_key: %v", err)
	}

	// (1) 差し替えただけでは既存行に効かない。
	var stored *string
	if err := tx.QueryRow(ctx,
		"SELECT series_key FROM recordings WHERE event_id = 1").Scan(&stored); err != nil {
		t.Fatalf("reading series_key after replace: %v", err)
	}
	if stored == nil || *stored != "テスト作品" {
		t.Fatalf("after replacing the function, stored series_key = %v, "+
			"want the old value テスト作品 (the generated column does not follow)", stored)
	}

	// (2) 列を作り直すと全行が新しい定義へ移る。これが migration の必須手順。
	//
	// 列を落とす前に recording_series を落とす必要がある（ビューが列を参照して
	// いる）。索引は列と一緒に落ちるので張り直す。
	if _, err := tx.Exec(ctx, "DROP VIEW public.recording_series"); err != nil {
		t.Fatalf("dropping recording_series: %v", err)
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE public.recordings DROP COLUMN series_key"); err != nil {
		t.Fatalf("dropping series_key: %v", err)
	}
	if _, err := tx.Exec(ctx, `
ALTER TABLE public.recordings
    ADD COLUMN series_key text GENERATED ALWAYS AS (public.series_key(title)) STORED`); err != nil {
		t.Fatalf("re-adding series_key: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"CREATE INDEX recordings_series_key_idx ON public.recordings (series_key)"); err != nil {
		t.Fatalf("recreating recordings_series_key_idx: %v", err)
	}
	if err := tx.QueryRow(ctx,
		"SELECT series_key FROM recordings WHERE event_id = 1").Scan(&stored); err != nil {
		t.Fatalf("reading series_key after recreate: %v", err)
	}
	if stored == nil || *stored != "アニメ　テスト作品　第1話" {
		t.Fatalf("after recreating the column, series_key = %v, want the new function's value", stored)
	}
}

// like_escape は LIKE の特殊文字（\ % _）を文字として扱えるようにする。
// これが無いと `100%` が 54 件の棚に当たり、`アニメ_` が全件に当たる。
func TestLikeEscape_EscapesLikeMetacharacters(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	cases := []struct {
		in   string
		want string
	}{
		{"100%", `100\%`},
		{"アニメ_", `アニメ\_`},
		{`C:\番組`, `C:\\番組`},
		{"ふつう", "ふつう"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var got string
			if err := pool.QueryRow(ctx, "SELECT public.like_escape($1)", tc.in).Scan(&got); err != nil {
				t.Fatalf("calling like_escape: %v", err)
			}
			if got != tc.want {
				t.Errorf("like_escape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// labelRuleWinner は実際の SQL 関数を呼ぶ。
func labelRuleWinner(t *testing.T, pool *pgxpool.Pool, title string) *int64 {
	t.Helper()
	var got *int64
	if err := pool.QueryRow(context.Background(),
		"SELECT public.label_rule_winner($1)", title).Scan(&got); err != nil {
		t.Fatalf("calling label_rule_winner(%q): %v", title, err)
	}
	return got
}

func createLabelRule(t *testing.T, pool *pgxpool.Pool, value, keyword string, priority int32) int64 {
	t.Helper()
	rule, err := sqlcgen.New(pool).CreateLabelRule(context.Background(), sqlcgen.CreateLabelRuleParams{
		Key:      "series",
		Value:    value,
		Keyword:  keyword,
		Priority: priority,
	})
	if err != nil {
		t.Fatalf("creating label rule %q/%q: %v", value, keyword, err)
	}
	return rule.ID
}

// 勝者は priority DESC, id ASC の先頭。同順位で勝者が不定だと、評価のたびに棚が
// 入れ替わる。
func TestLabelRuleWinner_PriorityThenIDOrder(t *testing.T) {
	pool := setupTestDB(t)

	// 同じキーワードに当たる 2 本。id の小さい方が勝つ。
	lowID := createLabelRule(t, pool, "作品A", "作品", 0)
	highID := createLabelRule(t, pool, "作品B", "作品", 0)
	if lowID > highID {
		t.Fatalf("label_rules ids = %d, %d; want the first created to have the smaller id", lowID, highID)
	}
	if got := labelRuleWinner(t, pool, "アニメ　作品　第1話"); got == nil || *got != lowID {
		t.Errorf("winner = %v, want the smaller id %d (same priority must resolve by id ASC)", got, lowID)
	}

	// priority が高い方が id に関わらず勝つ。
	newer := createLabelRule(t, pool, "作品C", "作品", 5)
	if got := labelRuleWinner(t, pool, "アニメ　作品　第1話"); got == nil || *got != newer {
		t.Errorf("winner = %v, want the higher priority rule %d", got, newer)
	}

	// 当たらないタイトルには勝者が居ない = 行を作らない（不変条件 10）。
	if got := labelRuleWinner(t, pool, "無関係な番組"); got != nil {
		t.Errorf("winner = %v, want nil for a title no rule matches", *got)
	}
}

// キーワード中の LIKE 特殊文字は文字として照合する。`100%` がメタ文字として
// 効くと、棚が壊れる（実データで 54 件に当たった）。
//
// **これは KeywordClause と同じ dialect であることの検査でもある**: Go 側だけ
// エスケープして SQL 側の like_escape を壊すと、ここが落ちる。
func TestLabelRuleWinner_KeywordMetacharactersAreLiteral(t *testing.T) {
	pool := setupTestDB(t)

	percent := createLabelRule(t, pool, "進捗", "100%", 0)
	underscore := createLabelRule(t, pool, "アニメ枠", "アニメ_", 0)

	if got := labelRuleWinner(t, pool, "特集 100% 達成"); got == nil || *got != percent {
		t.Errorf("winner for a literal %% keyword = %v, want %d", got, percent)
	}
	// `100x` は `100%` に当たってはならない（% がメタ文字なら当たる）。
	if got := labelRuleWinner(t, pool, "特集 100x 達成"); got != nil {
		t.Errorf("winner = %v, want nil: %% must not match 100x", *got)
	}
	if got := labelRuleWinner(t, pool, "アニメ_特別編"); got == nil || *got != underscore {
		t.Errorf("winner for a literal _ keyword = %v, want %d", got, underscore)
	}
	// `アニメX` は `アニメ_` に当たってはならない。
	if got := labelRuleWinner(t, pool, "アニメX特別編"); got != nil {
		t.Errorf("winner = %v, want nil: _ must not match アニメX", *got)
	}
}

// 正規化で NULL になる値は CHECK で弾く。何も主張しないルールを作らせない
// （不変条件 10）。
func TestLabelRules_ValueThatNormalizesToNullIsRejected(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	for _, value := range []string{"", "　", "【】", "第5話"} {
		t.Run(value, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				"INSERT INTO label_rules (key, value, keyword) VALUES ('series', $1, 'kw')", value)
			assertPgError(t, err, "23514") // check_violation
		})
	}

	// キーワードが空のルールも作らせない（全件に当たる棚になる）。
	_, err := pool.Exec(ctx,
		"INSERT INTO label_rules (key, value, keyword) VALUES ('series', '作品', '  ')")
	assertPgError(t, err, "23514")

	// key は series だけ（M8 のうちは表現不可能にする）。
	_, err = pool.Exec(ctx,
		"INSERT INTO label_rules (key, value, keyword) VALUES ('genre', '作品', 'kw')")
	assertPgError(t, err, "23514")
}

// recording_series は分類ルールが当たればその値、当たらなければ自動キー。
// ルールの値ではなく id を当たりに持つので、値の変更は JOIN で即座に効く。
func TestRecordingSeries_ViewPrefersWinner(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	ruleID := createLabelRule(t, pool, "NHK高校講座", "日本史", 0)
	later := createLabelRule(t, pool, "古典", "日本史", 1)

	matched := createSeriesRecording(t, pool, "NHK高校講座　日本史　第1回", 1, time.Now())
	unmatched := createSeriesRecording(t, pool, "無関係な番組", 2, time.Now())

	if got := recordingSeriesValue(t, pool, matched); got != "古典" {
		t.Errorf("matched recording series = %q, want the winner's value 古典", got)
	}
	if got := recordingSeriesValue(t, pool, unmatched); got != "無関係な番組" {
		t.Errorf("unmatched recording series = %q, want the automatic key", got)
	}

	// 勝者の値だけを変えると、当たりの行はそのままで実効の値が変わる。
	if _, err := sqlcgen.New(pool).UpdateLabelRule(ctx, sqlcgen.UpdateLabelRuleParams{
		ID: later, Value: "日本史", Keyword: "日本史", Priority: 1,
	}); err != nil {
		t.Fatalf("updating the winner's value: %v", err)
	}
	if got := recordingSeriesValue(t, pool, matched); got != "日本史" {
		t.Errorf("after changing the winner's value, series = %q, want 日本史", got)
	}

	// 勝者を消すと、次点に移らず自動キーへ落ちる（当たりは勝者しか持たない。
	// CASCADE は安全網でしかない）。全件再評価が次点に移す。
	if _, err := sqlcgen.New(pool).DeleteLabelRule(ctx, later); err != nil {
		t.Fatalf("deleting the winner: %v", err)
	}
	if got := recordingSeriesValue(t, pool, matched); got != "NHK高校講座" {
		t.Errorf("after deleting the winner, series = %q, want the automatic key", got)
	}

	// 再評価すると次点のルールに移る。
	if _, err := sqlcgen.New(pool).ApplyLabelRuleReevaluation(ctx); err != nil {
		t.Fatalf("re-evaluating after the winner was deleted: %v", err)
	}
	if got := recordingSeriesValue(t, pool, matched); got != "NHK高校講座" {
		t.Errorf("after re-evaluating, series = %q, want the remaining rule's value", got)
	}
	if hits := labelRuleHits(t, pool, matched); len(hits) != 1 || hits[0] != ruleID {
		t.Errorf("hits after re-evaluation = %v, want [%d] (the next rule must take over)", hits, ruleID)
	}
}

// トリガーは「既存の当たりを消す → 勝者がいれば入れる」の 2 段。upsert だけに
// すると、当たらなくなった録画に古い当たりが残る。
func TestRecordingsLabelRuleTrigger_FollowsTitleUpdates(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	createLabelRule(t, pool, "日本史", "日本史", 0)
	id := createSeriesRecording(t, pool, "NHK高校講座　日本史　第1回", 1, time.Now())

	if hits := labelRuleHits(t, pool, id); len(hits) != 1 {
		t.Fatalf("hits after insert = %v, want 1 row", hits)
	}

	// タイトルが当たらなくなったら、当たりは消える（行を作らない）。
	if _, err := pool.Exec(ctx, "UPDATE recordings SET title = '別の番組' WHERE id = $1", id); err != nil {
		t.Fatalf("updating the title: %v", err)
	}
	if hits := labelRuleHits(t, pool, id); len(hits) != 0 {
		t.Errorf("hits after the title stopped matching = %v, want none", hits)
	}

	// 当たるタイトルへ戻すと、また入る。
	if _, err := pool.Exec(ctx, "UPDATE recordings SET title = 'NHK高校講座　日本史　第2回' WHERE id = $1", id); err != nil {
		t.Fatalf("restoring a matching title: %v", err)
	}
	if hits := labelRuleHits(t, pool, id); len(hits) != 1 {
		t.Errorf("hits after the title matched again = %v, want 1 row", hits)
	}

	// title 以外の更新では当たりに触らない（UPDATE OF title だけに付ける）。
	if _, err := pool.Exec(ctx, "UPDATE recordings SET status = 'finished' WHERE id = $1", id); err != nil {
		t.Fatalf("updating an unrelated column: %v", err)
	}
	if hits := labelRuleHits(t, pool, id); len(hits) != 1 {
		t.Errorf("hits after an unrelated update = %v, want 1 row", hits)
	}
}

// 全件再評価は差分だけ適用する。変化が無ければ何も書かない（デッドタプルを
// 毎パス作らない）。
func TestApplyLabelRuleReevaluation_AppliesOnlyDifferences(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	createLabelRule(t, pool, "日本史", "日本史", 0)
	matched := createSeriesRecording(t, pool, "NHK高校講座　日本史　第1回", 1, time.Now())
	createSeriesRecording(t, pool, "無関係な番組", 2, time.Now())

	// トリガーが既に正しい状態を作っている。変化は無い。
	changed, err := q.ApplyLabelRuleReevaluation(ctx)
	if err != nil {
		t.Fatalf("re-evaluating: %v", err)
	}
	if changed != 0 {
		t.Errorf("changed = %d, want 0 (nothing differs)", changed)
	}

	// 新しいルールを足すと、当たる録画だけが変わる。
	createLabelRule(t, pool, "NHK", "NHK", 1)
	changed, err = q.ApplyLabelRuleReevaluation(ctx)
	if err != nil {
		t.Fatalf("re-evaluating after adding a rule: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1 (only the matching recording)", changed)
	}
	if got := recordingSeriesValue(t, pool, matched); got != "NHK" {
		t.Errorf("series = %q, want NHK", got)
	}

	// ルールを全部消すと、当たりの行が落ちて自動キーへ戻る。
	rules, err := q.ListLabelRules(ctx)
	if err != nil {
		t.Fatalf("listing label rules: %v", err)
	}
	for _, rule := range rules {
		if _, err := q.DeleteLabelRule(ctx, rule.ID); err != nil {
			t.Fatalf("deleting label rule %d: %v", rule.ID, err)
		}
	}
	if changed, err = q.ApplyLabelRuleReevaluation(ctx); err != nil {
		t.Fatalf("re-evaluating after deleting every rule: %v", err)
	} else if changed != 0 {
		t.Errorf("changed = %d, want 0 (CASCADE already removed the stale hit)", changed)
	}
	if got := recordingSeriesValue(t, pool, matched); got != "NHK高校講座" {
		t.Errorf("series = %q, want the automatic key after every rule was deleted", got)
	}
}

// 棚は生きていて再生できる録画だけを数える。ごみ箱・superseded・取り込めて
// いない録画は外れる。
func TestListRecordingShelves_PopulationAndRepresentative(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)

	// 同じ棚の 3 件。代表は新しい方。
	older := createSeriesRecording(t, pool, "アニメ　作品X　第1話", 1, base.Add(-2*time.Hour))
	newest := createSeriesRecording(t, pool, "アニメ　作品X　第2話", 2, base)
	also := createSeriesRecording(t, pool, "アニメ　作品X　第3話", 3, base.Add(-time.Hour))
	activeMediaAsset(t, pool, older, "original", "")
	activeMediaAsset(t, pool, newest, "encoded", "h264")
	activeMediaAsset(t, pool, also, "original", "")

	// 母集団から外れるもの: ごみ箱 / superseded / 再生できる資産が無い。
	trashed := createSeriesRecording(t, pool, "アニメ　作品X　第4話", 4, base.Add(time.Hour))
	activeMediaAsset(t, pool, trashed, "original", "")
	if _, err := pool.Exec(ctx, "UPDATE recordings SET deleted_at = now() WHERE id = $1", trashed); err != nil {
		t.Fatalf("trashing: %v", err)
	}
	superseded := createSeriesRecording(t, pool, "アニメ　作品X　第5話", 5, base.Add(2*time.Hour))
	activeMediaAsset(t, pool, superseded, "original", "")
	if _, err := pool.Exec(ctx, "UPDATE recordings SET superseded_at = now() WHERE id = $1", superseded); err != nil {
		t.Fatalf("superseding: %v", err)
	}
	noAsset := createSeriesRecording(t, pool, "アニメ　作品X　第6話", 6, base.Add(3*time.Hour))

	// 値の無い録画は NULL の棚として返す（UI が「その他」にまとめる材料）。
	nullKey := createSeriesRecording(t, pool, "【特集】", 7, base.Add(4*time.Hour))
	activeMediaAsset(t, pool, nullKey, "original", "")

	shelves, err := sqlcgen.New(pool).ListRecordingShelves(ctx)
	if err != nil {
		t.Fatalf("listing shelves: %v", err)
	}

	byValue := map[string]sqlcgen.ListRecordingShelvesRow{}
	for _, s := range shelves {
		key := "<NULL>"
		if s.Value != nil {
			key = *s.Value
		}
		byValue[key] = s
	}
	if len(shelves) != 2 {
		t.Fatalf("shelves = %d (%v), want 2 (作品X and the NULL shelf)", len(shelves), byValue)
	}

	got, ok := byValue["作品X"]
	if !ok {
		t.Fatalf("no 作品X shelf in %v", byValue)
	}
	if got.RecordingCount != 3 {
		t.Errorf("作品X count = %d, want 3 (deleted / superseded / assetless rows must be excluded)", got.RecordingCount)
	}
	if got.RepresentativeID != newest {
		t.Errorf("作品X representative = %d, want %d (newest program_start_at)", got.RepresentativeID, newest)
	}
	if got.Title != "アニメ　作品X　第2話" {
		t.Errorf("作品X title = %q, want the representative's raw title", got.Title)
	}
	_ = noAsset

	nullShelf, ok := byValue["<NULL>"]
	if !ok {
		t.Fatalf("no NULL shelf in %v (the UI needs it to size その他)", byValue)
	}
	if nullShelf.RecordingCount != 1 || nullShelf.RepresentativeID != nullKey {
		t.Errorf("NULL shelf = %+v, want count 1 and the 特集 recording", nullShelf)
	}
}

// recordingSeriesValue は recording_series ビューから実効シリーズを読む。
func recordingSeriesValue(t *testing.T, pool *pgxpool.Pool, recordingID int64) string {
	t.Helper()
	var got *string
	if err := pool.QueryRow(context.Background(),
		"SELECT value FROM recording_series WHERE recording_id = $1", recordingID).Scan(&got); err != nil {
		t.Fatalf("reading recording_series for %d: %v", recordingID, err)
	}
	if got == nil {
		return "<NULL>"
	}
	return *got
}

// labelRuleHits は録画の当たりのルール id を読む（順序は id 昇順）。
func labelRuleHits(t *testing.T, pool *pgxpool.Pool, recordingID int64) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT label_rule_id FROM label_rule_hits WHERE recording_id = $1 ORDER BY label_rule_id", recordingID)
	if err != nil {
		t.Fatalf("reading label_rule_hits for %d: %v", recordingID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning label_rule_hits: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating label_rule_hits: %v", err)
	}
	return out
}
