package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRuleImportKeysConstraints(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	var first, second int64
	for _, id := range []*int64{&first, &second} {
		if err := pool.QueryRow(ctx, "INSERT INTO rules (name) VALUES ('test') RETURNING id").Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '42')", first); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '42')",
		"INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '')",
		"INSERT INTO rule_import_keys VALUES ($1, 'unknown', '43')",
	} {
		if _, err := pool.Exec(ctx, query, second); err == nil {
			t.Fatalf("accepted invalid key: %s", query)
		}
	}
	if _, err := pool.Exec(ctx, "INSERT INTO rule_import_keys VALUES ($1, 'epgstation', '43')", second); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM rules WHERE id = $1", first); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM rule_import_keys").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("keys after cascade = %d, want 1", count)
	}
}

func TestCatalogRuleImportKeys(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			catalog := NewCatalog(pool, []string{"default"}, "default")
			doc := &catalogdoc.Document{Version: catalogdoc.Version}
			for _, id := range []int64{100, 101} {
				doc.Rules = append(doc.Rules, catalogdoc.Rule{ID: id, Name: "original", Enabled: true, Priority: 10, KeepOriginal: "always", EncodeProfiles: []string{}, Metadata: json.RawMessage(`{"epgstation":{"ruleId":42},"user":"kept"}`), CreatedAt: time.Now(), UpdatedAt: time.Now()})
			}
			// 新形式では metadata の数値も不透明なまま。同定は配列だけで決まる。
			if legacy {
				doc.Rules[1].Metadata = json.RawMessage(`{"epgstation":{"ruleId":"42"}}`)
			} else {
				doc.RuleImportKeys = []catalogdoc.RuleImportKey{{RuleID: 100, Source: "epgstation", ExternalID: "42"}}
			}
			if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
				t.Fatal(err)
			}
			exported, err := catalog.Export(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := []catalogdoc.RuleImportKey{{RuleID: 100, Source: "epgstation", ExternalID: "42"}}
			if !reflect.DeepEqual(exported.RuleImportKeys, want) {
				t.Fatalf("keys = %+v, want %+v", exported.RuleImportKeys, want)
			}
			expectedMetadata := `{"epgstation":{"ruleId":42},"user":"kept"}`
			if legacy {
				expectedMetadata = `{"user":"kept"}`
			}
			assertImportMetadata(t, exported.Rules[0].Metadata, expectedMetadata)
			assertImportMetadata(t, exported.Rules[1].Metadata, string(doc.Rules[1].Metadata))
			// JSON の往復を含めて別 DB に復元し、再適用でも行数を増やさない。
			raw, err := json.Marshal(exported)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip catalogdoc.Document
			if err := json.Unmarshal(raw, &roundTrip); err != nil {
				t.Fatal(err)
			}
			destination := NewCatalog(testutil.SetupDB(t), []string{"default"}, "default")
			for range 2 {
				if _, err := destination.Restore(ctx, &roundTrip, store.RestoreOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			after, err := destination.Export(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.RuleImportKeys, want) {
				t.Fatalf("round trip keys = %+v", after.RuleImportKeys)
			}
			assertImportMetadata(t, after.Rules[0].Metadata, expectedMetadata)

			// 先行するルール更新も含め、重複キー document 全体が戻る。
			doc.Rules[0].Name = "must roll back"
			if legacy {
				doc.Rules[1].Metadata = json.RawMessage(`{"epgstation":{"ruleId":42}}`)
			} else {
				doc.RuleImportKeys = append(doc.RuleImportKeys, catalogdoc.RuleImportKey{RuleID: 101, Source: "epgstation", ExternalID: "42"})
			}
			if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate restore error = %v, want ErrConflict", err)
			}
			unchanged, err := catalog.Export(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(unchanged.RuleImportKeys, want) || unchanged.Rules[0].Name != "original" {
				t.Fatalf("duplicate restore left partial changes: keys=%+v name=%q", unchanged.RuleImportKeys, unchanged.Rules[0].Name)
			}
		})
	}
}

func TestCatalogEmptyRuleImportKeys(t *testing.T) {
	ctx := t.Context()
	doc := &catalogdoc.Document{
		Version:        catalogdoc.Version,
		RuleImportKeys: []catalogdoc.RuleImportKey{},
		Rules:          []catalogdoc.Rule{{ID: 100, Name: "opaque", KeepOriginal: "always", Metadata: json.RawMessage(`{"epgstation":{"ruleId":42}}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}},
	}
	source := NewCatalog(testutil.SetupDB(t), []string{"default"}, "default")
	if _, err := source.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	exported, err := source.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exported.RuleImportKeys == nil || len(exported.RuleImportKeys) != 0 {
		t.Fatalf("exported keys = %#v, want non-nil empty array", exported.RuleImportKeys)
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var restored catalogdoc.Document
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	destination := NewCatalog(testutil.SetupDB(t), []string{"default"}, "default")
	if _, err := destination.Restore(ctx, &restored, store.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := destination.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RuleImportKeys) != 0 {
		t.Fatalf("opaque metadata became import key: %+v", after.RuleImportKeys)
	}
	assertImportMetadata(t, after.Rules[0].Metadata, string(doc.Rules[0].Metadata))
}

func TestRuleImportCreationRollsBackKey(t *testing.T) {
	pool := testutil.SetupDB(t)
	rules := NewRules(pool, []string{"default"})
	if _, err := rules.ImportEPGStationRule(t.Context(), 42, store.RuleInput{Name: "invalid", Sites: []string{"default", "default"}}); err == nil {
		t.Fatal("invalid child accepted")
	}
	var rulesCount, keyCount int
	if err := pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM rules), (SELECT count(*) FROM rule_import_keys)").Scan(&rulesCount, &keyCount); err != nil {
		t.Fatal(err)
	}
	if rulesCount != 0 || keyCount != 0 {
		t.Fatalf("partial import: rules=%d keys=%d", rulesCount, keyCount)
	}
	created, err := rules.ImportEPGStationRule(t.Context(), 42, store.RuleInput{Name: "valid"})
	if err != nil || !created {
		t.Fatalf("retry = %t, %v, want creation", created, err)
	}
}

func TestRuleImportPreservesConcurrentMetadata(t *testing.T) {
	pool := testutil.SetupDB(t)
	rules := NewRules(pool, []string{"default"})
	ctx := t.Context()
	if _, err := rules.ImportEPGStationRule(ctx, 42, store.RuleInput{Name: "original"}); err != nil {
		t.Fatal(err)
	}
	rows, err := rules.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `UPDATE rules SET metadata='{"manual":"concurrent"}'::jsonb WHERE id=$1`, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := rules.ImportEPGStationRule(ctx, 42, store.RuleInput{Name: "updated"}); done <- err }()
	waitForRuleWriters(t, pool, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("import did not complete")
	}
	after, err := rules.Get(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata["manual"] != "concurrent" {
		t.Fatalf("metadata after concurrent write = %v", after.Metadata)
	}
}

func assertImportMetadata(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("metadata = %s, want %s", got, want)
	}
}
