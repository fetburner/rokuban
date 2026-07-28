package ruler_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/testutil"
)

// M2-6 履歴ベース重複排除（issue #24）のテスト。
//
// 判定条件はすべて「マッチする側」と「マッチしない側」の両方向で押さえる。片側だけ
// 見ると条件を反転させても気付かない（CLAUDE.md「分岐は両方向で確認する」）。

// dedupeEventCounter は recordings の event_id をテスト間で一意にする。
// recordings_unique_active_event（site, network_id, service_id, event_id の部分
// ユニークインデックス）があるため、同じ event_id を複数回入れると失敗する。
var dedupeEventCounter int32 = 5000

// insertRecording は重複排除の比較対象になる録画履歴を 1 行作る。
//
// event_id は自動で採番して衝突を避ける（テスト対象の番組とは必ず別イベントに
// なるので、自己一致の除外に引っかからない）。番組と event_id を揃えたい
// 自己一致のテストは insertRecordingForEvent を使う。
func insertRecording(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	ruleID *int64, title, status string, startAt time.Time, deleted bool,
) (recordingID int64) {
	t.Helper()
	dedupeEventCounter++
	eventID := dedupeEventCounter
	var deletedAt *time.Time
	if deleted {
		now := time.Now()
		deletedAt = &now
	}
	err := pool.QueryRow(ctx, `
INSERT INTO recordings (
  rule_id, source, site, network_id, service_id, event_id,
  service_name, channel_type, channel, title,
  program_start_at, program_duration_ms, status, deleted_at
) VALUES ($1, 'rule', $2, $3, $4, $5, 'テスト局', 'GR', '27', $6, $7, $8, $9, $10)
RETURNING id`,
		ruleID, testSite, testNetworkID, testServiceID, eventID,
		title, startAt, testDurationMs, status, deletedAt,
	).Scan(&recordingID)
	if err != nil {
		t.Fatalf("inserting recordings fixture: %v", err)
	}
	return recordingID
}

// insertRecordingForEvent は event_id を明示して録画履歴を作る（自己一致のテスト用）。
func insertRecordingForEvent(
	t *testing.T, pool *pgxpool.Pool, ctx context.Context,
	ruleID *int64, title string, startAt time.Time, eventID int32,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `
INSERT INTO recordings (
  rule_id, source, site, network_id, service_id, event_id,
  service_name, channel_type, channel, title,
  program_start_at, program_duration_ms, status
) VALUES ($1, 'rule', $2, $3, $4, $5, 'テスト局', 'GR', '27', $6, $7, $8, 'finished')
RETURNING id`,
		ruleID, testSite, testNetworkID, testServiceID, eventID,
		title, startAt, testDurationMs,
	).Scan(&id)
	if err != nil {
		t.Fatalf("inserting recordings fixture: %v", err)
	}
	return id
}

// enableDedupe はルールの重複排除を有効にする。windowSeconds が nil なら
// dedupe_window は NULL（時間窓なし = 全履歴が対象）。
func enableDedupe(t *testing.T, pool *pgxpool.Pool, ctx context.Context, ruleID int64, threshold float32, windowSeconds *int64) {
	t.Helper()
	var window any
	if windowSeconds != nil {
		window = time.Duration(*windowSeconds) * time.Second
	}
	_, err := pool.Exec(ctx, `
UPDATE rules SET dedupe_enabled = true, dedupe_threshold = $2, dedupe_window = $3
WHERE id = $1`, ruleID, threshold, window)
	if err != nil {
		t.Fatalf("enabling dedupe on rule %d: %v", ruleID, err)
	}
}

// insertProgramWithEvent は event_id を指定して EPG プロジェクションに番組を作る
// （自己一致のテストで録画側と event_id を揃えるため）。
func insertProgramWithEvent(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64, title string, startAt time.Time, eventID int32) {
	t.Helper()
	endAt := startAt.Add(time.Duration(testDurationMs) * time.Millisecond)
	_, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO UPDATE SET
  event_id = EXCLUDED.event_id, name = EXCLUDED.name`,
		testSite, programID, testNetworkID, testServiceID, eventID,
		startAt, testDurationMs, endAt, title)
	if err != nil {
		t.Fatalf("inserting epg_programs fixture: %v", err)
	}
}

// baseSkip は base jsonb の skip を返す（載っていなければ false）。
func baseSkip(t *testing.T, raw []byte) bool {
	t.Helper()
	if len(raw) == 0 {
		return false
	}
	var opts db.ReservationOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		t.Fatalf("unmarshalling base %s: %v", raw, err)
	}
	return opts.Skip != nil && *opts.Skip
}

// assertDedupeEvidence は根拠 2 列とマッチの有無を突き合わせる。
// 2 列は必ず揃って設定/解除される（reservations_dedup_evidence_check）。
func assertDedupeEvidence(t *testing.T, r reservationRow, wantRecordingID *int64) {
	t.Helper()
	if wantRecordingID == nil {
		if r.DedupMatchRecordingID != nil {
			t.Errorf("dedup_match_recording_id = %d, want NULL", *r.DedupMatchRecordingID)
		}
		if r.DedupSimilarity != nil {
			t.Errorf("dedup_similarity = %v, want NULL", *r.DedupSimilarity)
		}
		return
	}
	if r.DedupMatchRecordingID == nil {
		t.Fatalf("dedup_match_recording_id = NULL, want %d", *wantRecordingID)
	}
	if *r.DedupMatchRecordingID != *wantRecordingID {
		t.Errorf("dedup_match_recording_id = %d, want %d", *r.DedupMatchRecordingID, *wantRecordingID)
	}
	if r.DedupSimilarity == nil {
		t.Fatal("dedup_similarity = NULL, want a similarity value")
	}
	if *r.DedupSimilarity <= 0 || *r.DedupSimilarity > 1 {
		t.Errorf("dedup_similarity = %v, want (0, 1]", *r.DedupSimilarity)
	}
}

// dedupeFixtureThreshold は setupDedupeFixture が使う類似度の閾値。題名が完全に
// 一致する録画を用意するので、0.5 なら余裕を持ってマッチする。
const dedupeFixtureThreshold = 0.5

// dedupeFixture は「同じルールで既に録れている番組がある」状態を用意する共通の下地。
// 対象番組は programID 4001 の「再放送テスト 第1話」で、過去に同題名の録画がある。
type dedupeFixture struct {
	ruleID      int64
	recordingID int64
	programID   int64
	start       time.Time
}

func setupDedupeFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context) dedupeFixture {
	t.Helper()
	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 4001
	insertProgram(t, pool, ctx, programID, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, dedupeFixtureThreshold, nil)
	recordingID := insertRecording(t, pool, ctx, &ruleID,
		"再放送テスト 第1話", db.RecordingStatusFinished, time.Now().Add(-7*24*time.Hour), false)
	return dedupeFixture{ruleID: ruleID, recordingID: recordingID, programID: programID, start: start}
}

// 1. 同じルールで録れている番組があれば base.skip が立ち、根拠 2 列が埋まる。
func TestRunPass_DedupeSkipsRerunWithEvidence(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !baseSkip(t, res.Base) {
		t.Errorf("base.skip = false, want true (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, &f.recordingID)
}

// 2. 題名が似ていなければ skip も根拠も付かない（1 の反対方向）。
func TestRunPass_DedupeNoMatchLeavesEvidenceNull(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4002, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.5, nil)
	// 全く違う題名の録画。trgm の類似度が閾値に届かない。
	insertRecording(t, pool, ctx, &ruleID, "ニュース天気予報", db.RecordingStatusFinished,
		time.Now().Add(-7*24*time.Hour), false)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4002)
	if !ok {
		t.Fatal("reservation not created")
	}
	if baseSkip(t, res.Base) {
		t.Errorf("base.skip = true, want false (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, nil)
}

// 3. dedupe_enabled = false のルールは題名が一致していても判定しない。
func TestRunPass_DedupeDisabledIsNoOp(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4003, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	// enableDedupe を呼ばない（dedupe_enabled は DEFAULT false）。
	insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第1話", db.RecordingStatusFinished,
		time.Now().Add(-7*24*time.Hour), false)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4003)
	if !ok {
		t.Fatal("reservation not created")
	}
	if baseSkip(t, res.Base) {
		t.Errorf("base.skip = true with dedupe_enabled = false (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, nil)
}

// 4. 比較対象は同じ rule_id の録画だけ。別ルールの録画とは誤マッチしない
//
//	（「同じルールが同じ番組シリーズを指している」前提に乗るための条件）。
func TestRunPass_DedupeIgnoresOtherRulesRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4004, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.5, nil)

	// 題名は完全に一致するが、別のルール由来の録画。
	otherRuleID := insertRule(t, pool, ctx, "other", 1)
	insertRecording(t, pool, ctx, &otherRuleID, "再放送テスト 第1話", db.RecordingStatusFinished,
		time.Now().Add(-7*24*time.Hour), false)
	// rule_id が NULL の録画（手動予約由来）も対象外。
	insertRecording(t, pool, ctx, nil, "再放送テスト 第1話", db.RecordingStatusFinished,
		time.Now().Add(-7*24*time.Hour), false)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4004)
	if !ok {
		t.Fatal("reservation not created")
	}
	if baseSkip(t, res.Base) {
		t.Errorf("base.skip = true from another rule's recording (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, nil)
}

// 5. status = 'finished' 以外は「録れた」とみなさない。
func TestRunPass_DedupeIgnoresUnfinishedRecordings(t *testing.T) {
	for _, status := range []string{db.RecordingStatusRecording, db.RecordingStatusFailed} {
		t.Run(status, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()

			insertService(t, pool, ctx)
			start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
			insertProgram(t, pool, ctx, 4005, "再放送テスト 第1話", start)
			ruleID := insertRule(t, pool, ctx, "rerun", 10)
			insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
			enableDedupe(t, pool, ctx, ruleID, 0.5, nil)
			insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第1話", status,
				time.Now().Add(-7*24*time.Hour), false)

			if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
				t.Fatalf("RunPass: %v", err)
			}

			res, ok := getReservation(t, pool, ctx, 4005)
			if !ok {
				t.Fatal("reservation not created")
			}
			if baseSkip(t, res.Base) {
				t.Errorf("base.skip = true from a %s recording (base = %s)", status, res.Base)
			}
			assertDedupeEvidence(t, res, nil)
		})
	}
}

// 6. deleted_at では絞らない。ごみ箱に入れても物理削除しても recordings 行は
// tombstone として残り、重複排除はそれでも機能する契約（docs/schema.md §5
// 「ごみ箱を空にしても録画履歴・ドロップ統計・重複排除は壊れない」）。
// `deleted_at IS NULL` を足す実装だとここが落ちる。
func TestRunPass_DedupeCountsDeletedRecordings(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4006, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.5, nil)
	recordingID := insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第1話",
		db.RecordingStatusFinished, time.Now().Add(-7*24*time.Hour), true /* deleted_at */)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4006)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !baseSkip(t, res.Base) {
		t.Errorf("base.skip = false for a trashed recording; deleted_at must not be filtered (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, &recordingID)
}

// 7. dedupe_window の外の録画は無視する（両方向: 窓内はマッチ、窓外はしない）。
func TestRunPass_DedupeWindowExcludesOldRecordings(t *testing.T) {
	tests := []struct {
		name            string
		recordedDaysAgo int
		wantMatch       bool
	}{
		{"inside window", 3, true},
		{"outside window", 30, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()

			insertService(t, pool, ctx)
			start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
			insertProgram(t, pool, ctx, 4007, "再放送テスト 第1話", start)
			ruleID := insertRule(t, pool, ctx, "rerun", 10)
			insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
			window := int64(7 * 24 * 60 * 60) // 7 日
			enableDedupe(t, pool, ctx, ruleID, 0.5, &window)
			recordingID := insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第1話",
				db.RecordingStatusFinished,
				time.Now().Add(-time.Duration(tt.recordedDaysAgo)*24*time.Hour), false)

			if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
				t.Fatalf("RunPass: %v", err)
			}

			res, ok := getReservation(t, pool, ctx, 4007)
			if !ok {
				t.Fatal("reservation not created")
			}
			if got := baseSkip(t, res.Base); got != tt.wantMatch {
				t.Errorf("base.skip = %v, want %v (base = %s)", got, tt.wantMatch, res.Base)
			}
			if tt.wantMatch {
				assertDedupeEvidence(t, res, &recordingID)
			} else {
				assertDedupeEvidence(t, res, nil)
			}
		})
	}
}

// 8. 閾値を超える録画が複数あれば類似度が最も高いものを採る。
func TestRunPass_DedupePicksBestMatch(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertProgram(t, pool, ctx, 4008, "再放送テスト 第1話", start)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.3, nil)

	// 部分一致（低い類似度）を先に入れ、完全一致を後に入れる。id 順ではなく
	// 類似度順で選ばれることを見る。
	insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第2話", db.RecordingStatusFinished,
		time.Now().Add(-7*24*time.Hour), false)
	exact := insertRecording(t, pool, ctx, &ruleID, "再放送テスト 第1話",
		db.RecordingStatusFinished, time.Now().Add(-6*24*time.Hour), false)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, 4008)
	if !ok {
		t.Fatal("reservation not created")
	}
	assertDedupeEvidence(t, res, &exact)
}

// 9. 根拠は毎パス作り直す。前のパスでマッチしていても、似た録画が無くなったら
// skip も根拠 2 列も消える（CLAUDE.md 不変条件 9「導出値は毎パス作り直す」）。
// 参照先の録画が物理削除された場合の孤立もこの経路で自己修復する（FK は無い）。
func TestRunPass_DedupeEvidenceClearedWhenMatchDisappears(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created on first pass")
	}
	if !baseSkip(t, first.Base) {
		t.Fatalf("precondition: base.skip should be true after first pass (base = %s)", first.Base)
	}
	assertDedupeEvidence(t, first, &f.recordingID)

	// 参照先の録画を消す（ごみ箱ではなく物理削除。FK が無いので DELETE は通る）。
	if _, err := pool.Exec(ctx, `DELETE FROM recordings WHERE id = $1`, f.recordingID); err != nil {
		t.Fatalf("deleting recording: %v", err)
	}

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation missing after second pass")
	}
	if baseSkip(t, second.Base) {
		t.Errorf("base.skip still true after the matching recording disappeared (base = %s)", second.Base)
	}
	assertDedupeEvidence(t, second, nil)
}

// 10. マッチしたままの 2 回目のパスは no-op（updated_at が動かない）。
// 類似度は real 列 → float32 → JSON → real と往復するので、IS DISTINCT FROM の
// 差分判定が毎パス真になると NOTIFY が鳴り続ける（docs/recording.md §3.1
// 「書き込みは差分」）。
func TestRunPass_DedupeSecondPassIsNoOp(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created on first pass")
	}
	assertDedupeEvidence(t, first, &f.recordingID)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation missing after second pass")
	}
	if !first.UpdatedAt.Equal(second.UpdatedAt) {
		t.Errorf("updated_at changed on a no-diff second pass: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
}

// 11. ルールが外れて rule_id が NULL になったら、base と一緒に根拠 2 列も凍結する。
// base だけ凍結して根拠を消すと「なぜ skip なのか説明できない base」が残る。
func TestRunPass_DedupeEvidenceFrozenWhenRuleUnmatches(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	// 上書きを置いて、ルールが外れても予約行が消えず detached で残るようにする
	// （docs/recording.md §4.3）。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_overrides (site, program_id, overrides, program_start_at, program_duration_ms)
VALUES ($1, $2, '{"priority":7}'::jsonb, $3, $4)`,
		testSite, f.programID, f.start, testDurationMs); err != nil {
		t.Fatalf("inserting program_overrides fixture: %v", err)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created on first pass")
	}
	assertDedupeEvidence(t, first, &f.recordingID)

	// ルールを無効化してマッチしなくする。
	if _, err := pool.Exec(ctx, `UPDATE rules SET enabled = false WHERE id = $1`, f.ruleID); err != nil {
		t.Fatalf("disabling rule: %v", err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	second, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation should be kept (detached) because overrides exist")
	}
	if second.RuleID != nil {
		t.Fatalf("rule_id = %d, want NULL after the rule stopped matching", *second.RuleID)
	}
	if second.State != db.ReservationStateDetached {
		t.Errorf("state = %s, want detached", second.State)
	}
	if !baseSkip(t, second.Base) {
		t.Errorf("frozen base lost skip (base = %s)", second.Base)
	}
	assertDedupeEvidence(t, second, &f.recordingID)
}

// 12. 重複排除が base.skip を立てても、ユーザーの action='record' が勝つ
// （EPGStation#473「この番組は重複扱いにしない」。docs/recording.md §4.2
// 「M2-6 の dedup skip」）。db.EffectiveOptions の分岐との結線を見る。
func TestRunPass_DedupeRecordIntentWins(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !baseSkip(t, res.Base) {
		t.Fatalf("precondition: base.skip should be true (base = %s)", res.Base)
	}

	// 意図が無い時点では effective.skip は true（base 由来）。
	eff, err := db.EffectiveOptions(res.Base, nil, nil)
	if err != nil {
		t.Fatalf("EffectiveOptions without intent: %v", err)
	}
	if eff.Skip == nil || !*eff.Skip {
		t.Errorf("effective skip without intent = %v, want true", eff.Skip)
	}

	// ユーザーが「録れ」と指定すると base.skip を上書きする。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_intents (site, program_id, action, program_start_at, program_duration_ms)
VALUES ($1, $2, 'record', $3, $4)`,
		testSite, f.programID, f.start, testDurationMs); err != nil {
		t.Fatalf("inserting program_intents fixture: %v", err)
	}
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	res, ok = getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation missing after second pass")
	}
	// ruler は base.skip を立て続ける（判定結果は変わらない）。効力の合成は
	// db.EffectiveOptions が担うので、base 側に record 意図を焼き込まない。
	if !baseSkip(t, res.Base) {
		t.Errorf("ruler should keep base.skip regardless of the intent (base = %s)", res.Base)
	}
	action := db.IntentRecord
	eff, err = db.EffectiveOptions(res.Base, nil, &action)
	if err != nil {
		t.Fatalf("EffectiveOptions with record intent: %v", err)
	}
	if eff.Skip != nil && *eff.Skip {
		t.Error("effective skip = true; the user's record intent must beat the dedupe skip")
	}
	// 根拠は残す（「重複と判定されたが録る」を UI で説明できる必要がある）。
	assertDedupeEvidence(t, res, &f.recordingID)
}

// 13. その番組自身の録画は重複とみなさない。放送済みの番組の予約は GC まで残るため、
// 自己一致を許すと似た題名が無くても similarity = 1.0 でマッチしてしまい、
// 「録画済みの番組」が「重複としてスキップ」と説明される。さらに
// effective.skip = true になることで reconciler.markOrphaned /
// detectStartDelays の入力（listDesired の出力）からも外れ、重複排除が
// 無関係な状態機械の DB 状態を変えてしまう。
func TestRunPass_DedupeIgnoresOwnRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(-2 * time.Hour).Truncate(time.Second) // 放送済み
	const programID = 4009
	const eventID int32 = 777
	insertProgramWithEvent(t, pool, ctx, programID, "再放送テスト 第1話", start, eventID)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.5, nil)
	// この番組自身の録画（同じ network_id / service_id / event_id）。
	insertRecordingForEvent(t, pool, ctx, &ruleID, "再放送テスト 第1話", start, eventID)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation not created")
	}
	if baseSkip(t, res.Base) {
		t.Errorf("base.skip = true from the program's own recording (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, nil)
}

// 13b. ただし別イベントの録画なら（同じサービスの再放送でも）マッチする。
// 13 の除外が event_id の一致だけを見ていて、サービス単位で丸ごと除外して
// いないことの確認（13 だけだと「同じ局の録画を全部無視する」実装でも通る）。
func TestRunPass_DedupeMatchesSameServiceDifferentEvent(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	insertService(t, pool, ctx)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	const programID = 4010
	const eventID int32 = 888
	insertProgramWithEvent(t, pool, ctx, programID, "再放送テスト 第1話", start, eventID)
	ruleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, ruleID, "再放送テスト")
	enableDedupe(t, pool, ctx, ruleID, 0.5, nil)
	// 同じ局（network_id / service_id）だが event_id が違う = 別放送の録画。
	other := insertRecordingForEvent(t, pool, ctx, &ruleID, "再放送テスト 第1話",
		time.Now().Add(-7*24*time.Hour), eventID+1)

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	res, ok := getReservation(t, pool, ctx, programID)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !baseSkip(t, res.Base) {
		t.Errorf("base.skip = false for a different event on the same service (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, &other)
}
