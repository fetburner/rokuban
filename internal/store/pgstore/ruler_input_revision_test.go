package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRulesDeleteBumpsSiteAfterConcurrentScopeUpdate(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rules := NewRules(pool, "site-a", "site-b")
	rule, err := rules.Create(ctx, revisionScopeRuleInput("site-a"))
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning rule row blocker: %v", err)
	}
	defer rollback(blocker)
	var lockedID int64
	if err := blocker.QueryRow(ctx, `SELECT id FROM rules WHERE id = $1 FOR UPDATE`, rule.ID).Scan(&lockedID); err != nil {
		t.Fatalf("locking rule row: %v", err)
	}

	updated := make(chan error, 1)
	go func() {
		_, err := rules.Update(ctx, rule.ID, revisionScopeRuleInput("site-b"))
		updated <- err
	}()
	waitForRuleWriters(t, pool, 1)

	deleted := make(chan error, 1)
	go func() {
		_, err := rules.Delete(ctx, rule.ID)
		deleted <- err
	}()
	waitForRuleWriters(t, pool, 2)

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("releasing rule row blocker: %v", err)
	}
	if err := <-updated; err != nil {
		t.Fatalf("updating rule scope concurrently with deletion: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("deleting rule after scope update: %v", err)
	}

	reservations := NewReservations(pool)
	for site, want := range map[string]int64{"site-a": 2, "site-b": 2} {
		got, err := reservations.GetRulerInputRevision(ctx, site)
		if err != nil {
			t.Fatalf("reading %s ruler input revision: %v", site, err)
		}
		if got != want {
			t.Errorf("%s ruler input revision = %d, want %d", site, got, want)
		}
	}
}

func revisionScopeRuleInput(site string) store.RuleInput {
	return store.RuleInput{
		Name:  "scope revision test",
		Sites: []string{site},
		TextMatches: []store.RuleTextMatch{{
			Target: "name", Mode: "keyword", Value: "scope", CaseSensitive: true,
		}},
	}
}
