package ruler_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/testutil"
)

// M2-6 履歴ベース重複排除（issue #24）のテスト。
//
// 判定条件はすべて「マッチする側」と「マッチしない側」の両方向で押さえる。片側だけ
// 見ると条件を反転させても気付かない（CLAUDE.md「分岐は両方向で確認する」）。

// dedupeEventCounter は recordings の event_id をテスト間で分ける。
// recordings_unique_active_event（program_start_at を含む部分ユニークインデックス）
// の衝突をテストデータ間で避けるため、event_id も一緒にずらしておく。
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
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, '')
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
	var opts reservation.Options
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
	// （docs/recording.md §4.3）。program_overrides への FK（#27）を満たすため、
	// ruler が動く前に program_snapshots 行を用意しておく。
	insertProgramSnapshotDirect(t, pool, ctx, f.programID, "再放送テスト 第1話", f.start)
	if _, err := pool.Exec(ctx, `
INSERT INTO program_overrides (site, program_id, overrides)
VALUES ($1, $2, '{"priority":7}'::jsonb)`,
		testSite, f.programID); err != nil {
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
// 「dedup skip（重複排除）」）。reservation.EffectiveOptions の分岐との結線を見る。
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
	eff, err := reservation.EffectiveOptions(res.Base, nil, nil)
	if err != nil {
		t.Fatalf("EffectiveOptions without intent: %v", err)
	}
	if eff.Skip == nil || !*eff.Skip {
		t.Errorf("effective skip without intent = %v, want true", eff.Skip)
	}

	// ユーザーが「録れ」と指定すると base.skip を上書きする。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_intents (site, program_id, action)
VALUES ($1, $2, 'record')`,
		testSite, f.programID); err != nil {
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
	// reservation.EffectiveOptions が担うので、base 側に record 意図を焼き込まない。
	if !baseSkip(t, res.Base) {
		t.Errorf("ruler should keep base.skip regardless of the intent (base = %s)", res.Base)
	}
	action := reservation.IntentRecord
	eff, err = reservation.EffectiveOptions(res.Base, nil, &action)
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

// 14. ルールを削除すると、そのルールで録れた履歴は比較のスコープから外れる
// （issue #215 の決定。docs/recording/ruler.md §3.1「ルールの削除は履歴の
// スコープを消す」）。3 段階で押さえる:
//
//   - ルールが生きている間は skip する（1 と同じ前提。ここが崩れたら以降の
//     アサーションは空虚になる）
//   - ルールを削除して**同じ条件で作り直す**と skip しない。作り直したルールは
//     新しい id を持つので、過去の録画は 1 件もマッチしない
//   - **新ルールの下で 1 本録れると、また skip する**（過剰録画が一過性である
//     ことの根拠。docs はこのテスト名を併記してその主張を書いている）
//
// 「FK を外して recordings.rule_id の値を残す」実装に変えると、**機構を見て
// いる段階 1 だけが落ちて、仕様を見ている段階 2・3 は通る**（実測: 一時
// マイグレーションで recordings_rule_id_fkey を DROP すると
// 「recordings.rule_id = 1 after deleting the rule, want NULL」の 1 件のみ
// FAIL）。症状（作り直したルールでは履歴が効かない）は値の保持では消えず、
// それが FK を外す案を採らなかった理由そのものである。
func TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete(t *testing.T) {
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
		t.Fatalf("base.skip = false while the rule is alive (base = %s)", res.Base)
	}
	assertDedupeEvidence(t, res, &f.recordingID)

	// ルールを削除する。DELETE /api/rules/{id} は予約の後片付けを足すが、
	// 履歴に効くのはこの 1 文（rules の行が消えること）だけ。
	if _, err := pool.Exec(ctx, `DELETE FROM rules WHERE id = $1`, f.ruleID); err != nil {
		t.Fatalf("deleting rule: %v", err)
	}

	// 段階 1: FK recordings_rule_id_fkey が ON DELETE SET NULL なので履歴の帰属が落ちる。
	var histRuleID *int64
	if err := pool.QueryRow(ctx,
		`SELECT rule_id FROM recordings WHERE id = $1`, f.recordingID,
	).Scan(&histRuleID); err != nil {
		t.Fatalf("querying recordings.rule_id: %v", err)
	}
	if histRuleID != nil {
		t.Errorf("recordings.rule_id = %d after deleting the rule, want NULL", *histRuleID)
	}

	// 段階 2: 同じ条件でルールを作り直す。id は新しくなる。
	newRuleID := insertRule(t, pool, ctx, "rerun", 10)
	insertRuleKeyword(t, pool, ctx, newRuleID, "再放送テスト")
	enableDedupe(t, pool, ctx, newRuleID, dedupeFixtureThreshold, nil)
	if newRuleID == f.ruleID {
		t.Fatalf("recreated rule reused id %d; rules.id must be GENERATED ALWAYS AS IDENTITY", newRuleID)
	}

	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass after recreating the rule: %v", err)
	}

	res2, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created after recreating the rule")
	}
	if baseSkip(t, res2.Base) {
		t.Errorf("base.skip = true after delete + recreate (base = %s); "+
			"履歴は削除で比較対象から外れ、作り直しても引き継がれないのが仕様", res2.Base)
	}
	assertDedupeEvidence(t, res2, nil)

	// 段階 3: 新ルールの下で 1 本録れると、以降はまた弾かれる。
	//
	// 過剰録画が**一過性**であること（= 被害の上限）の根拠はこの段階にしか
	// 無い。ここが偽なら帰結は「窓の中の再放送を全部録り直す」に戻り、
	// 決定 (a)（仕様として受け入れる）の結論自体が変わる。docs は
	// このテスト名を根拠に「1 本録れれば以降はまた弾かれる」と書いている。
	newRec := insertRecording(t, pool, ctx, &newRuleID,
		"再放送テスト 第1話", db.RecordingStatusFinished, time.Now().Add(-2*time.Hour), false)
	if err := ruler.New([]string{testSite}, pool, nil).RunPass(ctx); err != nil {
		t.Fatalf("RunPass after one recording under the new rule: %v", err)
	}
	res3, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created after recording under the new rule")
	}
	if !baseSkip(t, res3.Base) {
		t.Errorf("base.skip = false after one recording under the new rule (base = %s); "+
			"過剰録画が一過性であることの根拠が崩れている", res3.Base)
	}
	// 根拠 2 列は**新しい**録画を指す（旧ルールの履歴が復活したのではない）。
	assertDedupeEvidence(t, res3, &newRec)
}

// 15. 「履歴（recordings）側の除外印は作らない」（docs/recording/ruler.md §3.1）の
// 根拠になっているのは「1 本録れた時点でその録画が新しい抑制元になり、以降の
// 再放送はまた弾かれる」という一過性である。この経路 —— action='record' で
// 個別に勝たせた番組が実際に録れた結果、その録画が次の抑制元になること ——
// を測っているテストはこれまで無かった。TestRunPass_DedupeHistoryLeavesScopeOnRuleDelete
// の段階 3 が測っているのは「ルール削除→作り直し」の経路で、ここで測るのは
// 「意図で録らせた 1 本が次の抑制元になる」別の経路である。
func TestRunPass_DedupeRecordIntentThenNewRecordingSuppressesAgain(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	f := setupDedupeFixture(t, pool, ctx)

	// R1（フィクスチャの録画）の題名を、P2/P3 と完全一致ではなく閾値は超える
	// が 1.0 未満の類似度まで弱める。P3 は P2 と同一題名にする（下記）ので、
	// R1 の題名が完全一致のままだと、後で入れる R2（P3 と完全一致）と類似度
	// 1.0 で並び、tie-break の rec.id ASC で先に入った R1（古い録画）が勝って
	// しまう。それでは「新しい録画が抑制元になる」ことを検証できない。
	//
	// 実際の運用でも同じ tie-break が効く: 題名が毎回完全一致するシリーズは
	// 複数の録画が similarity 1.0 で並び、dedup_match_recording_id には
	// rec.id ASC で最古の録画が入る（実測）。「なぜスキップされたか」の説明に
	// 常に最新の録画が出るとは限らない。
	if _, err := pool.Exec(ctx,
		`UPDATE recordings SET title = '再放送テスト 第2話' WHERE id = $1`, f.recordingID,
	); err != nil {
		t.Fatalf("weakening R1's title: %v", err)
	}

	// precondition: R1 と「再放送テスト 第1話」（P2/P3 の題名）の類似度が
	// (閾値, 1.0) に収まることを直接測って固定する。この guard を外しても
	// テストは空虚には通らない（どちらの向きに崩れても別のアサーションで
	// 落ちることを実測済み）: 類似度を 1.0 まで戻す（tie）と
	// assertDedupeEvidence(res3, &newRec) が dedup_match_recording_id の不一致
	// で落ち、閾値未満まで下げると 1 パス目後の precondition（本関数の
	// 「precondition: base.skip should be true after first pass」）で落ちる。
	// guard が変えるのは「落ちるかどうか」ではなく「どこで・何の名前で落ちる
	// か」——2 つの既に検出可能な失敗を、この 1 箇所の早い失敗にまとめて
	// 潰す防御の重複であり、削ってよいという意味ではない（後で誰かが
	// このテストを読んで「evidence assertion が守っているから安全」と誤読し、
	// 上の 2 箇所を弱めることを防ぐ）。
	var r1Similarity float64
	if err := pool.QueryRow(ctx,
		`SELECT similarity(title, '再放送テスト 第1話') FROM recordings WHERE id = $1`, f.recordingID,
	).Scan(&r1Similarity); err != nil {
		t.Fatalf("querying R1 similarity: %v", err)
	}
	if r1Similarity <= float64(dedupeFixtureThreshold) || r1Similarity >= 1.0 {
		t.Fatalf("precondition: R1 similarity to the exact title = %v, want in (%v, 1.0) "+
			"so that R2 becomes the unique winner later", r1Similarity, dedupeFixtureThreshold)
	}

	r := ruler.New([]string{testSite}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	res, ok := getReservation(t, pool, ctx, f.programID)
	if !ok {
		t.Fatal("reservation not created")
	}
	if !baseSkip(t, res.Base) {
		t.Fatalf("precondition: base.skip should be true after first pass (base = %s)", res.Base)
	}

	// ユーザーが「録れ」と指定して個別に勝たせる（EPGStation#473 のうち予約側で
	// 実装済みの経路。docs/recording/ruler.md §3.1）。
	if _, err := pool.Exec(ctx, `
INSERT INTO program_intents (site, program_id, action)
VALUES ($1, $2, 'record')`,
		testSite, f.programID); err != nil {
		t.Fatalf("inserting program_intents fixture: %v", err)
	}
	action := reservation.IntentRecord
	eff, err := reservation.EffectiveOptions(res.Base, nil, &action)
	if err != nil {
		t.Fatalf("EffectiveOptions with record intent: %v", err)
	}
	if eff.Skip != nil && *eff.Skip {
		t.Fatalf("precondition: effective skip should be false once the user's record intent wins (base = %s)", res.Base)
	}

	// P2 が実際に録れた: 同一題名・別イベントの後続放送 P3 が EPG に現れ、P2 を
	// 録った実績 R2（(network_id, service_id, event_id) は P2 自身のもの。
	// insertProgram は event_id 0 で番組を作るので、それに合わせる）が積まれる。
	const programID3 = 4011
	const eventID3 int32 = 9001
	p3Start := f.start.Add(7 * 24 * time.Hour)
	insertProgramWithEvent(t, pool, ctx, programID3, "再放送テスト 第1話", p3Start, eventID3)
	newRec := insertRecordingForEvent(t, pool, ctx, &f.ruleID, "再放送テスト 第1話", f.start, 0)

	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	res3, ok := getReservation(t, pool, ctx, programID3)
	if !ok {
		t.Fatal("reservation not created for the later broadcast")
	}
	if !baseSkip(t, res3.Base) {
		t.Errorf("base.skip = false after a real recording landed on the record-intent path (base = %s); "+
			"1 本録れれば以降の再放送はまた弾かれるはずである", res3.Base)
	}
	// 根拠 2 列は**新しい**録画 R2 を指す（古い R1 が抑制元のままではない）。
	assertDedupeEvidence(t, res3, &newRec)
}
