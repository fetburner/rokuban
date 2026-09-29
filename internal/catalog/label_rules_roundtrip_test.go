package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestExportRescue_RoundTripsLabelRules は、ユーザーが書いた分類ルールが export →
// rescue の往復で id・値・優先度ごと戻り、rescue が同じ tx で当たりの表
// （label_rule_hits。catalog には入れない導出値）も作り直すことを確かめる。
//
// 当たりの再評価を rescue が持たないと、タイトルが変わらない既存録画の当たりは
// worker の定期再評価まで空のままになる（トリガーは新規行と title 変更しか見ない）。
func TestExportRescue_RoundTripsLabelRules(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()

	recID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           500,
		ServiceName:       "NHK総合",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "NHK高校講座　数学I　第1回",
		ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}

	// 値に空白を含む（棚キーは最初の空白で切れる）ルールと、優先度が違うルール。
	low, err := q.CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: "series", Value: "数学", Keyword: "数学", Priority: 1,
	})
	if err != nil {
		t.Fatalf("CreateLabelRule low: %v", err)
	}
	high, err := q.CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: "series", Value: "高校数学 I", Keyword: "数学I", Priority: 7,
	})
	if err != nil {
		t.Fatalf("CreateLabelRule high: %v", err)
	}

	doc, err := Export(ctx, pool)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.LabelRules) != 2 {
		t.Fatalf("exported label rules = %d, want 2", len(doc.LabelRules))
	}
	if _, err := Write(mediaDir, doc, DefaultKeep); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 分類ルールだけを失った状況（recordings は残る。当たりは CASCADE で消える）。
	if _, err := pool.Exec(ctx, `DELETE FROM label_rules`); err != nil {
		t.Fatalf("simulating loss: %v", err)
	}
	var hits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM label_rule_hits`).Scan(&hits); err != nil || hits != 0 {
		t.Fatalf("hits after losing the rules = %d (err %v), want 0", hits, err)
	}

	result, err := RescueLatest(ctx, pool, mediaDir, []string{"default"})
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.LabelRules != 2 {
		t.Fatalf("rescued label rules = %d, want 2", result.LabelRules)
	}

	rows, err := q.ListLabelRules(ctx)
	if err != nil {
		t.Fatalf("ListLabelRules: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("restored rules = %d, want 2", len(rows))
	}
	// priority DESC の順（high が先）。id・値・優先度が保たれ、生成列は作り直される。
	if rows[0].ID != high.ID || rows[0].Value != "高校数学 I" || rows[0].Priority != 7 ||
		rows[0].ValueKey == nil || *rows[0].ValueKey != "高校数学" {
		t.Errorf("restored high rule = %+v, want id %d value %q priority 7 value_key 高校数学", rows[0], high.ID, "高校数学 I")
	}
	if rows[1].ID != low.ID || rows[1].Keyword != "数学" || rows[1].Priority != 1 {
		t.Errorf("restored low rule = %+v, want id %d keyword 数学 priority 1", rows[1], low.ID)
	}
	if !rows[0].CreatedAt.Equal(high.CreatedAt) {
		t.Errorf("created_at = %s, want %s", rows[0].CreatedAt, high.CreatedAt)
	}

	// 当たりは rescue の再評価で戻る（録画の title は変わっていないのでトリガーは走らない）。
	var winner int64
	if err := pool.QueryRow(ctx,
		`SELECT label_rule_id FROM label_rule_hits WHERE recording_id = $1`, recID).Scan(&winner); err != nil {
		t.Fatalf("reading the restored hit: %v", err)
	}
	if winner != high.ID {
		t.Errorf("restored winner = %d, want %d (priority 7 beats 1)", winner, high.ID)
	}

	// IDENTITY は max(id) に揃う（次の作成が復元した id と衝突しない）。
	next, err := q.CreateLabelRule(ctx, sqlcgen.CreateLabelRuleParams{
		Key: "series", Value: "次", Keyword: "次",
	})
	if err != nil {
		t.Fatalf("creating a rule after rescue: %v", err)
	}
	if next.ID <= high.ID || next.ID <= low.ID {
		t.Errorf("next id = %d, want greater than %d and %d", next.ID, high.ID, low.ID)
	}
}
