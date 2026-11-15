package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogDuplicateEPGStationKey(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	doc := &catalogdoc.Document{}
	for _, id := range []int64{100, 101} {
		doc.Rules = append(doc.Rules, catalogdoc.Rule{ID: id, Name: "duplicate", Enabled: true, Priority: 10, KeepOriginal: "always", EncodeProfiles: []string{}, Metadata: json.RawMessage(`{"epgstation":{"ruleId":42}}`), CreatedAt: time.Now(), UpdatedAt: time.Now()})
	}
	_, err := NewCatalog(pool, []string{"default"}, "default").Restore(ctx, doc, store.RestoreOptions{})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore error = %v, want conflict", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rules").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rules = %d, want complete rollback", count)
	}
}

func TestRulesEPGStationKeyUnique(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	for i, metadata := range []string{`{}`, `{}`, `{"epgstation":{"ruleId":null}}`, `{"epgstation":{"ruleId":null}}`, `{"epgstation":{"ruleId":42}}`, `{"epgstation":{"ruleId":43}}`} {
		if _, err := pool.Exec(ctx, "INSERT INTO rules (name, metadata) VALUES ('test', $1::jsonb)", metadata); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	for _, metadata := range []string{`{"epgstation":{"ruleId":42}}`, `{"epgstation":{"ruleId":"42"}}`} {
		_, err := pool.Exec(ctx, "INSERT INTO rules (name, metadata) VALUES ('duplicate', $1::jsonb)", metadata)
		if !errors.Is(mapDatabaseError(err), store.ErrConflict) {
			t.Fatalf("duplicate insert error = %v, want conflict", err)
		}
	}
}

func TestImportEPGStationRuleStringKey(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	rules := NewRules(pool, []string{"default"})
	metadata := map[string]any{"epgstation": map[string]any{"ruleId": "42"}}
	original, err := rules.Create(ctx, store.RuleInput{Name: "original", Metadata: &metadata})
	if err != nil {
		t.Fatal(err)
	}

	created, err := rules.ImportEPGStationRule(ctx, 42, store.RuleInput{Name: "imported"})
	if err != nil {
		t.Fatalf("import string key: %v", err)
	}
	if created {
		t.Fatal("import created a rule, want update")
	}
	rows, err := rules.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rules = %d, want 1", len(rows))
	}
	if rows[0].ID != original.ID || rows[0].Name != "imported" {
		t.Fatalf("imported rule = %+v, want original ID %d with updated name", rows[0], original.ID)
	}
}
