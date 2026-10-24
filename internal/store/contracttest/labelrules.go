package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// LabelRuleFixture は分類ルール契約の実装と、永続状態を作るためのフックをまとめる。
type LabelRuleFixture struct {
	Store        store.LabelRules
	Reevaluation store.LabelRuleReevaluation

	NewRecording func(t *testing.T, title string) int64
	// BeginReevaluationLock は store の実装と同じ再評価ロックを取る tx を返す。
	BeginReevaluationLock func(t *testing.T) (commit func())
	// WaitForReevaluationLock は再評価がロック待ちになったことを確認する。
	WaitForReevaluationLock func(t *testing.T)
	// LabelRuleHit は録画の当たりと行バージョンを返す。同じ当たりを再評価しても永続行が書き直されないことを、行バージョンで確かめる。行バージョンを返す読み取りが契約に無いので、フックのまま残す。
	LabelRuleHit func(t *testing.T, recordingID int64) (ruleID int64, rowVersion string, exists bool)
}

// NewLabelRuleStore は分類ルール契約の実装とフィクスチャを作る。
type NewLabelRuleStore func(t *testing.T) LabelRuleFixture

// RunLabelRules は実在の store 実装に対して分類ルールの契約を実行する。
func RunLabelRules(t *testing.T, newStore NewLabelRuleStore) {
	t.Helper()
	t.Run("CRUD and series key", func(t *testing.T) { runLabelRuleCRUD(t, newStore) })
	t.Run("reevaluation applies the committed rule set", func(t *testing.T) { runLabelRuleChange(t, newStore) })
	t.Run("unchanged hits are not rewritten", func(t *testing.T) { runUnchangedLabelRuleHits(t, newStore) })
	t.Run("reevaluation waits for the current rule set", func(t *testing.T) { runLabelRuleReevaluationSerialization(t, newStore) })
}

func runLabelRuleCRUD(t *testing.T, newStore NewLabelRuleStore) {
	f := newStore(t)
	ctx := context.Background()
	if _, err := f.Store.Get(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := f.Store.Update(ctx, -1, "作品X", "keyword", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Update(missing) error = %v, want ErrNotFound", err)
	}
	if err := f.Store.Delete(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete(missing) error = %v, want ErrNotFound", err)
	}

	key, err := f.Store.SeriesKey(ctx, "NHK高校講座 数学I")
	if err != nil || key == nil || *key != "NHK高校講座" {
		t.Fatalf("SeriesKey = %v, %v, want NHK高校講座", key, err)
	}
	if key, err := f.Store.SeriesKey(ctx, "【】"); err != nil || key != nil {
		t.Fatalf("SeriesKey(symbols) = %v, %v, want nil", key, err)
	}

	first, err := f.Store.Create(ctx, "series", "NHK高校講座 数学I", "数学", 0)
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := f.Store.Create(ctx, "series", "作品Y", "作品Y", 5)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if first.ValueKey != *mustSeriesKey(t, f.Store, ctx, first.Value) {
		t.Errorf("created value key = %q, want series_key(%q)", first.ValueKey, first.Value)
	}
	got, err := f.Store.Get(ctx, first.ID)
	if err != nil || got.ID != first.ID || got.Keyword != "数学" {
		t.Fatalf("Get(first) = %+v, %v", got, err)
	}
	listed, err := f.Store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 2 || listed[0].ID != second.ID || listed[1].ID != first.ID {
		t.Fatalf("List() IDs = %v, want priority order [%d %d]", labelRuleIDs(listed), second.ID, first.ID)
	}

	updated, err := f.Store.Update(ctx, first.ID, "作品X", "作品X", 8)
	if err != nil || updated.ID != first.ID || updated.Value != "作品X" || updated.Priority != 8 {
		t.Fatalf("Update(first) = %+v, %v", updated, err)
	}
	if err := f.Store.Delete(ctx, second.ID); err != nil {
		t.Fatalf("Delete(second) error = %v", err)
	}
	if _, err := f.Store.Get(ctx, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func runLabelRuleChange(t *testing.T, newStore NewLabelRuleStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID := f.NewRecording(t, "target title")
	rule, err := f.Store.Create(ctx, "series", "Target", "no match", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	if changed, err := f.Reevaluation.ReevaluateAll(ctx); err != nil || changed != 0 {
		t.Fatalf("ReevaluateAll before edit = %d, %v, want 0, nil", changed, err)
	}
	if _, _, exists := f.LabelRuleHit(t, recordingID); exists {
		t.Fatal("recording matched the nonmatching label rule before the edit")
	}
	if _, err := f.Store.Update(ctx, rule.ID, rule.Value, "target", rule.Priority); err != nil {
		t.Fatalf("Update(rule) error = %v", err)
	}
	if changed, err := f.Reevaluation.ReevaluateAll(ctx); err != nil || changed != 1 {
		t.Fatalf("ReevaluateAll after edit = %d, %v, want 1, nil", changed, err)
	}
	if gotRuleID, _, exists := f.LabelRuleHit(t, recordingID); !exists || gotRuleID != rule.ID {
		t.Fatalf("recording hit = (%d, %t), want rule %d", gotRuleID, exists, rule.ID)
	}
}

func runUnchangedLabelRuleHits(t *testing.T, newStore NewLabelRuleStore) {
	f := newStore(t)
	ctx := context.Background()
	rule, err := f.Store.Create(ctx, "series", "Target", "target", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	recordingID := f.NewRecording(t, "target title")
	gotRuleID, versionBefore, exists := f.LabelRuleHit(t, recordingID)
	if !exists || gotRuleID != rule.ID {
		t.Fatalf("initial recording hit = (%d, %t), want rule %d", gotRuleID, exists, rule.ID)
	}
	changed, err := f.Reevaluation.ReevaluateAll(ctx)
	if err != nil || changed != 0 {
		t.Fatalf("ReevaluateAll unchanged = %d, %v, want 0, nil", changed, err)
	}
	gotRuleID, versionAfter, exists := f.LabelRuleHit(t, recordingID)
	if !exists || gotRuleID != rule.ID || versionAfter != versionBefore {
		t.Fatalf("recording hit after unchanged pass = (%d, %q, %t), want (%d, %q, true)",
			gotRuleID, versionAfter, exists, rule.ID, versionBefore)
	}
}

func runLabelRuleReevaluationSerialization(t *testing.T, newStore NewLabelRuleStore) {
	f := newStore(t)
	ctx := context.Background()
	recordingID := f.NewRecording(t, "target title")
	rule, err := f.Store.Create(ctx, "series", "Target", "no match", 0)
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	release := f.BeginReevaluationLock(t)
	done := make(chan struct {
		changed int64
		err     error
	}, 1)
	go func() {
		changed, err := f.Reevaluation.ReevaluateAll(ctx)
		done <- struct {
			changed int64
			err     error
		}{changed: changed, err: err}
	}()
	f.WaitForReevaluationLock(t)
	if _, err := f.Store.Update(ctx, rule.ID, rule.Value, "target", rule.Priority); err != nil {
		release()
		t.Fatalf("Update(rule while re-evaluation waits) error = %v", err)
	}
	release()

	select {
	case result := <-done:
		if result.err != nil || result.changed != 1 {
			t.Fatalf("ReevaluateAll after lock release = %d, %v, want 1, nil", result.changed, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReevaluateAll did not finish after the lock was released")
	}
	if gotRuleID, _, exists := f.LabelRuleHit(t, recordingID); !exists || gotRuleID != rule.ID {
		t.Fatalf("recording hit = (%d, %t), want rule %d after committed rule update", gotRuleID, exists, rule.ID)
	}
}

func mustSeriesKey(t *testing.T, s store.LabelRules, ctx context.Context, value string) *string {
	t.Helper()
	key, err := s.SeriesKey(ctx, value)
	if err != nil || key == nil {
		t.Fatalf("SeriesKey(%q) = %v, %v", value, key, err)
	}
	return key
}

func labelRuleIDs(rules []store.LabelRule) []int64 {
	ids := make([]int64, len(rules))
	for i, rule := range rules {
		ids[i] = rule.ID
	}
	return ids
}
