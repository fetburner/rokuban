package mysqlstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
)

// readLabelRuleHit は録画の当たりを返す。当たりが無ければ exists は false を返す。
func readLabelRuleHit(t *testing.T, db *sql.DB, recordingID int64) (ruleID int64, exists bool) {
	t.Helper()
	err := db.QueryRowContext(t.Context(), `SELECT label_rule_id FROM label_rule_hits WHERE recording_id = ?`, recordingID).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("reading label rule hit for recording %d: %v", recordingID, err)
	}
	return ruleID, true
}

// TestLabelRuleHitsFollowRecordingTitles は、録画のタイトルの変更で当たりが付け替わることを確かめる。
// 同期のトリガーの UPDATE 側が無いと、無関係だった録画が一致する場合でも当たりは作られない。
func TestLabelRuleHitsFollowRecordingTitles(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := t.Context()
	rule, err := NewLabelRules(db).Create(ctx, "series", "作品X", "作品X", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	recordingID := seedLabelRuleRecording(t, db, "無関係")
	if _, exists := readLabelRuleHit(t, db, recordingID); exists {
		t.Fatal("unrelated recording has a label rule hit after insert")
	}

	execFixture(t, db, `UPDATE recordings SET title = '作品X 第1話' WHERE id = ?`, recordingID)
	if got, exists := readLabelRuleHit(t, db, recordingID); !exists || got != rule.ID {
		t.Fatalf("hit after title change to a match = (%d, %t), want rule %d", got, exists, rule.ID)
	}
	var series string
	if err := db.QueryRowContext(ctx, `SELECT value FROM recording_series WHERE recording_id = ?`, recordingID).Scan(&series); err != nil {
		t.Fatalf("reading recording series: %v", err)
	}
	if series != "作品X" {
		t.Fatalf("recording_series value = %q, want the rule value 作品X", series)
	}

	execFixture(t, db, `UPDATE recordings SET title = '無関係 2' WHERE id = ?`, recordingID)
	if _, exists := readLabelRuleHit(t, db, recordingID); exists {
		t.Fatal("hit remains after the title stopped matching")
	}
}

// TestLabelRuleWinnerNormalizesWidth は、タイトルとキーワードの全角英数・全角空白と大文字を
// Postgres の normalize_search_text と同じく半角の小文字へ写して照合することを確かめる。
func TestLabelRuleWinnerNormalizesWidth(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := t.Context()
	rule, err := NewLabelRules(db).Create(ctx, "series", "作品Z", "ＡＢＣ", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	recordingID := seedLabelRuleRecording(t, db, "abc　第１話")
	if got, exists := readLabelRuleHit(t, db, recordingID); !exists || got != rule.ID {
		t.Fatalf("hit for fullwidth-normalized title = (%d, %t), want rule %d", got, exists, rule.ID)
	}
}

// TestLabelRuleValueKeyFollowsValue は、value_key が Go の series_key から書かれ、
// value を変えれば値も変わることを確かめる。キーの無い値は拒む。
func TestLabelRuleValueKeyFollowsValue(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := t.Context()
	labelRules := NewLabelRules(db)
	rule, err := labelRules.Create(ctx, "series", "NHK高校講座 数学I", "数学", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	if rule.ValueKey != "NHK高校講座" {
		t.Fatalf("created value key = %q, want NHK高校講座", rule.ValueKey)
	}
	updated, err := labelRules.Update(ctx, rule.ID, "作品Y", "作品Y", 0)
	if err != nil {
		t.Fatalf("Update(rule) error = %v", err)
	}
	if updated.ValueKey != "作品Y" {
		t.Fatalf("updated value key = %q, want 作品Y", updated.ValueKey)
	}
	if _, err := labelRules.Create(ctx, "series", "【】", "記号", 0); err == nil {
		t.Fatal("Create(value without a series key) succeeded, want error")
	}
}
