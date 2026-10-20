package contracttest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// RulesFixture はルール契約の実装と予約状態を作るフックをまとめる。
type RulesFixture struct {
	Store        store.Rules
	Import       store.RuleImport
	Reservations store.Reservations

	// SeedReservation は指定したルールに属する予約を作る。withIntent が true なら
	// record 意図を作り、false なら意図も override も無い予約にする。
	SeedReservation func(t *testing.T, ruleID int64, withIntent bool) (site string, programID int64)
	// HoldRule はルールへの書き込みを直列化する点を別 tx で握り、release で解放する。
	HoldRule func(t *testing.T, ruleID int64) (release func())
	// FailRuleDeletion は、以後のルール本体の削除を失敗させる。restore で元に戻す。
	// 意図の無い予約の削除がルールの削除と同じ tx かを測るために使う。
	FailRuleDeletion func(t *testing.T) (restore func())
	// WaitForRuleWriters は同一ルールへの書き込みが n 本待つまで待つ。
	WaitForRuleWriters func(t *testing.T, n int)
}

// NewRulesFixture はルール契約の実装とフィクスチャを作る。
type NewRulesFixture func(t *testing.T) RulesFixture

// RunRules はルールの PostgreSQL 実装に要求する共通の振る舞いを検証する。
func RunRules(t *testing.T, newFixture NewRulesFixture) {
	t.Helper()
	t.Run("CRUD and site lookup", func(t *testing.T) { runRuleCRUD(t, newFixture) })
	t.Run("create child failure rolls back", func(t *testing.T) { runRuleCreateRollback(t, newFixture) })
	t.Run("update replaces all children atomically", func(t *testing.T) { runRuleUpdateChildren(t, newFixture) })
	t.Run("delete preserves reservations with intent", func(t *testing.T) { runRuleDeleteIntent(t, newFixture) })
	t.Run("delete failure keeps reservations without intent", func(t *testing.T) { runRuleDeleteFailure(t, newFixture) })
	t.Run("same-rule updates serialize", func(t *testing.T) { runRuleUpdateSerialization(t, newFixture) })
	t.Run("EPGStation import uses stable identity atomically", func(t *testing.T) { runRuleImport(t, newFixture) })
}

func runRuleCRUD(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.Store.Get(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := f.Store.Update(ctx, -1, store.RuleInput{Name: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Update(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := f.Store.Delete(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete(missing) error = %v, want ErrNotFound", err)
	}
	if sites, err := f.Store.Sites(ctx, -1); err != nil || len(sites) != 0 {
		t.Fatalf("Sites(missing) = %v, %v, want empty, nil", sites, err)
	}

	low, err := f.Store.Create(ctx, ruleInput("lower priority", 4, "low"))
	if err != nil {
		t.Fatalf("Create(low) error = %v", err)
	}
	high, err := f.Store.Create(ctx, ruleInput("higher priority", 20, "high"))
	if err != nil {
		t.Fatalf("Create(high) error = %v", err)
	}
	got, err := f.Store.Get(ctx, low.ID)
	if err != nil || got.Name != low.Name || len(got.TextMatches) != 1 || got.TextMatches[0].Value != "low" {
		t.Fatalf("Get(low) = %+v, %v, want its text match", got, err)
	}
	listed, err := f.Store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed) != 2 || listed[0].ID != high.ID || listed[1].ID != low.ID {
		t.Fatalf("List() IDs = %v, want [%d %d]", ruleIDs(listed), high.ID, low.ID)
	}
	if sites, err := f.Store.Sites(ctx, low.ID); err != nil || len(sites) != 0 {
		t.Fatalf("Sites(all sites rule) = %v, %v, want empty, nil", sites, err)
	}
}

func runRuleCreateRollback(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	in := ruleInput("must roll back", 10, "child inserted first")
	in.Sites = []string{"duplicate-site", "duplicate-site"}
	if _, err := f.Store.Create(context.Background(), in); err == nil {
		t.Fatal("Create(duplicate child) error = nil, want child insertion failure")
	}
	rows, err := f.Store.List(context.Background())
	if err != nil {
		t.Fatalf("List(after failed Create) error = %v", err)
	}
	for _, row := range rows {
		if row.Name == "must roll back" {
			t.Fatalf("failed Create left rule row behind: %+v", row)
		}
	}
}

func runRuleUpdateChildren(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	created, err := f.Store.Create(ctx, store.RuleInput{
		Name: "before", TextMatches: []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: "before"}},
		Services: []store.RuleService{{NetworkID: 1, ServiceID: 2}}, ChannelTypes: []string{"GR"},
		Genres: []int16{3}, Times: []store.RuleTimeWindow{{Weekdays: 5, StartSec: 3600, EndSec: 7200}}, Sites: []string{"old-site"},
	})
	if err != nil {
		t.Fatalf("Create(before) error = %v", err)
	}
	updatedInput := store.RuleInput{
		Name: "after", TextMatches: []store.RuleTextMatch{{Target: "description", Mode: "regex", Value: "after"}},
		Services: []store.RuleService{{NetworkID: 4, ServiceID: 5}}, ChannelTypes: []string{"BS"},
		Genres: []int16{6}, Times: []store.RuleTimeWindow{{Weekdays: 2, StartSec: 1800, EndSec: 2700}}, Sites: []string{"new-site"},
	}
	if _, err := f.Store.Update(ctx, created.ID, updatedInput); err != nil {
		t.Fatalf("Update(after) error = %v", err)
	}
	got, err := f.Store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get(after Update) error = %v", err)
	}
	assertRuleChildren(t, got, updatedInput)

	failed := updatedInput
	failed.Name = "failed update"
	failed.TextMatches = []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: "partial"}}
	failed.Sites = []string{"duplicate-site", "duplicate-site"}
	if _, err := f.Store.Update(ctx, created.ID, failed); err == nil {
		t.Fatal("Update(duplicate child) error = nil, want child insertion failure")
	}
	afterFailure, err := f.Store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get(after failed Update) error = %v", err)
	}
	assertRuleChildren(t, afterFailure, updatedInput)
	if afterFailure.Name != "after" {
		t.Fatalf("Name after failed Update = %q, want %q", afterFailure.Name, "after")
	}
}

func runRuleDeleteIntent(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	rule, err := f.Store.Create(ctx, store.RuleInput{Name: "delete me", Sites: []string{"target-site"}})
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	intentSite, intentProgramID := f.SeedReservation(t, rule.ID, true)
	plainSite, plainProgramID := f.SeedReservation(t, rule.ID, false)
	result, err := f.Store.Delete(ctx, rule.ID)
	if err != nil {
		t.Fatalf("Delete(rule) error = %v", err)
	}
	if result.DeletedReservations != 1 || result.DetachedReservations != 1 {
		t.Fatalf("Delete result = %+v, want one deleted and one detached", result)
	}
	if len(result.Sites) != 1 || result.Sites[0] != "target-site" {
		t.Fatalf("Delete result sites = %v, want [target-site]", result.Sites)
	}
	if _, err := f.Reservations.GetReservation(ctx, plainSite, plainProgramID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetReservation(plain) error = %v, want ErrNotFound", err)
	}
	intent, err := f.Reservations.GetReservation(ctx, intentSite, intentProgramID)
	if err != nil {
		t.Fatalf("GetReservation(intent) error = %v", err)
	}
	if intent.IntentAction == nil || *intent.IntentAction != reservation.IntentRecord || intent.Reservation.RuleID != nil {
		t.Fatalf("intent reservation after Delete = %+v, want record intent and detached rule ID", intent)
	}
	if _, err := f.Store.Get(ctx, rule.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(deleted rule) error = %v, want ErrNotFound", err)
	}
}

func runRuleDeleteFailure(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	rule, err := f.Store.Create(ctx, store.RuleInput{Name: "undeletable", Sites: []string{"target-site"}})
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	plainSite, plainProgramID := f.SeedReservation(t, rule.ID, false)
	restore := f.FailRuleDeletion(t)
	_, err = f.Store.Delete(ctx, rule.ID)
	restore()
	if err == nil {
		t.Fatal("Delete(with failing rule deletion) error = nil, want failure")
	}
	if _, err := f.Reservations.GetReservation(ctx, plainSite, plainProgramID); err != nil {
		t.Fatalf("GetReservation(plain) after failed Delete error = %v, want the reservation kept", err)
	}
	if _, err := f.Store.Get(ctx, rule.ID); err != nil {
		t.Fatalf("Get(rule) after failed Delete error = %v, want the rule kept", err)
	}
}

func runRuleUpdateSerialization(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	rule, err := f.Store.Create(ctx, store.RuleInput{Name: "initial", TextMatches: []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: "initial"}}})
	if err != nil {
		t.Fatalf("Create(rule) error = %v", err)
	}
	release := f.HoldRule(t, rule.ID)
	type result struct{ err error }
	results := make(chan result, 2)
	for _, name := range []string{"first", "second"} {
		name := name
		go func() {
			_, err := f.Store.Update(ctx, rule.ID, store.RuleInput{
				Name: name, TextMatches: []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: name}},
			})
			results <- result{err: err}
		}()
	}
	f.WaitForRuleWriters(t, 2)
	release()
	for range 2 {
		if got := <-results; got.err != nil {
			t.Fatalf("Update(concurrent) error = %v", got.err)
		}
	}
	got, err := f.Store.Get(ctx, rule.ID)
	if err != nil {
		t.Fatalf("Get(after concurrent Update) error = %v", err)
	}
	if len(got.TextMatches) != 1 || got.TextMatches[0].Value != got.Name || (got.Name != "first" && got.Name != "second") {
		t.Fatalf("rule after concurrent updates = name %q and text matches %+v, want one complete update", got.Name, got.TextMatches)
	}
}

func runRuleImport(t *testing.T, newFixture NewRulesFixture) {
	f := newFixture(t)
	ctx := context.Background()
	in := store.RuleInput{
		Name: "epg initial", Enabled: boolValue(true), Sites: []string{"import-site"},
		TextMatches: []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: "initial"}},
	}
	created, err := f.Import.ImportEPGStationRule(ctx, 4321, in)
	if err != nil || !created {
		t.Fatalf("ImportEPGStationRule(first) = %t, %v, want true, nil", created, err)
	}
	in.Name = "epg updated"
	in.Sites = []string{"updated-site"}
	in.TextMatches = []store.RuleTextMatch{{Target: "description", Mode: "keyword", Value: "updated"}}
	created, err = f.Import.ImportEPGStationRule(ctx, 4321, in)
	if err != nil || created {
		t.Fatalf("ImportEPGStationRule(repeat) = %t, %v, want false, nil", created, err)
	}
	rows, err := f.Store.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List(after repeated import) = %d, %v, want one rule", len(rows), err)
	}
	if rows[0].Name != "epg updated" || len(rows[0].TextMatches) != 1 || rows[0].TextMatches[0].Value != "updated" ||
		len(rows[0].Sites) != 1 || rows[0].Sites[0] != "updated-site" {
		t.Fatalf("rule after repeated import = %+v, want updated row with replaced children", rows[0])
	}
	description := "kept manual description"
	priority := int32(37)
	dedupeEnabled := true
	dedupeThreshold := float32(0.73)
	dedupeWindow := int64(86400)
	keepOriginal := "until_encoded"
	encodeProfiles := []string{"h264"}
	metadata := rows[0].Metadata
	if _, ok := metadata["epgstation"]; !ok {
		t.Fatalf("metadata after import = %v, want EPGStation identity", metadata)
	}
	if _, err := f.Store.Update(ctx, rows[0].ID, store.RuleInput{
		Name: rows[0].Name, Description: &description, Priority: &priority,
		DedupeEnabled: &dedupeEnabled, DedupeThreshold: &dedupeThreshold, DedupeWindowSeconds: &dedupeWindow,
		KeepOriginal: &keepOriginal, EncodeProfiles: &encodeProfiles, Metadata: &metadata,
		TextMatches: rows[0].TextMatches, Sites: rows[0].Sites,
	}); err != nil {
		t.Fatalf("Store.Update(manual fields) error = %v", err)
	}
	in.Name = "epg after manual edit"
	if _, err := f.Import.ImportEPGStationRule(ctx, 4321, in); err != nil {
		t.Fatalf("ImportEPGStationRule(after manual edit) error = %v", err)
	}
	rows, err = f.Store.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List(after manual field preservation) = %d, %v, want one rule", len(rows), err)
	}
	got := rows[0]
	if got.Description != description || got.Priority != priority || !got.DedupeEnabled ||
		got.DedupeThreshold == nil || *got.DedupeThreshold != dedupeThreshold ||
		got.DedupeWindowSeconds == nil || *got.DedupeWindowSeconds != dedupeWindow ||
		got.KeepOriginal != keepOriginal || !slices.Equal(got.EncodeProfiles, encodeProfiles) {
		t.Fatalf("manual fields after import = %+v, want description/priority/dedupe/retention preserved", got)
	}
	in.Name = "must roll back"
	in.Sites = []string{"duplicate-site", "duplicate-site"}
	in.TextMatches = []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: "partial"}}
	if _, err := f.Import.ImportEPGStationRule(ctx, 4321, in); err == nil {
		t.Fatal("ImportEPGStationRule(invalid child) error = nil, want failure")
	}
	afterFailure, err := f.Store.List(ctx)
	if err != nil || len(afterFailure) != 1 || afterFailure[0].Name != "epg after manual edit" ||
		len(afterFailure[0].TextMatches) != 1 || afterFailure[0].TextMatches[0].Value != "updated" {
		t.Fatalf("rules after failed import = %+v, %v, want previous import unchanged", afterFailure, err)
	}
}

func ruleInput(name string, priority int32, match string) store.RuleInput {
	return store.RuleInput{
		Name: name, Priority: &priority,
		TextMatches: []store.RuleTextMatch{{Target: "name", Mode: "keyword", Value: match}},
	}
}

func boolValue(value bool) *bool { return &value }

func assertRuleChildren(t *testing.T, got store.Rule, want store.RuleInput) {
	t.Helper()
	if !slices.Equal(got.TextMatches, want.TextMatches) {
		t.Errorf("text matches = %+v, want %+v", got.TextMatches, want.TextMatches)
	}
	if !slices.Equal(got.Services, want.Services) {
		t.Errorf("services = %+v, want %+v", got.Services, want.Services)
	}
	if !slices.Equal(got.ChannelTypes, want.ChannelTypes) {
		t.Errorf("channel types = %v, want %v", got.ChannelTypes, want.ChannelTypes)
	}
	if !slices.Equal(got.Genres, want.Genres) {
		t.Errorf("genres = %v, want %v", got.Genres, want.Genres)
	}
	if !slices.Equal(got.Times, want.Times) {
		t.Errorf("time windows = %+v, want %+v", got.Times, want.Times)
	}
	if !slices.Equal(got.Sites, want.Sites) {
		t.Errorf("sites = %v, want %v", got.Sites, want.Sites)
	}
}

func ruleIDs(rules []store.Rule) []int64 {
	ids := make([]int64, len(rules))
	for i, rule := range rules {
		ids[i] = rule.ID
	}
	return ids
}
