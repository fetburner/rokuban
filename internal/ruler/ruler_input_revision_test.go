package ruler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRunPass_RuleEditDuringPassReevaluates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	insertRevisionTestPrograms(t, pool, ctx)
	rules := pgstore.NewRules(pool, []string{db.DefaultSite})
	rule, err := rules.Create(ctx, revisionTestRuleInput("P"))
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	r := newRevisionTestRuler(pool)

	changed := false
	beforeRulerWriteTx = func() {
		if changed {
			return
		}
		changed = true
		if _, err := rules.Update(ctx, rule.ID, revisionTestRuleInput("Q")); err != nil {
			t.Fatalf("editing rule during pass: %v", err)
		}
	}
	t.Cleanup(func() { beforeRulerWriteTx = nil })

	if err := r.runPassForSite(ctx, db.DefaultSite); err != nil {
		t.Fatalf("runPassForSite: %v", err)
	}
	assertRevisionTestReservations(t, pool, ctx, 2)
}

func TestRunPass_RuleDeleteDuringPassReevaluates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	insertRevisionTestPrograms(t, pool, ctx)
	rules := pgstore.NewRules(pool, []string{db.DefaultSite})
	rule, err := rules.Create(ctx, revisionTestRuleInput("P"))
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	r := newRevisionTestRuler(pool)

	deleted := false
	beforeRulerWriteTx = func() {
		if deleted {
			return
		}
		deleted = true
		if _, err := rules.Delete(ctx, rule.ID); err != nil {
			t.Fatalf("deleting rule during pass: %v", err)
		}
	}
	t.Cleanup(func() { beforeRulerWriteTx = nil })

	if err := r.runPassForSite(ctx, db.DefaultSite); err != nil {
		t.Fatalf("runPassForSite: %v", err)
	}
	ids := readRevisionTestReservations(t, pool, ctx)
	if len(ids) != 0 {
		t.Fatalf("reservation program IDs = %v, want none after rule deletion", ids)
	}
}

func TestRunPass_StalePassCannotOverwriteNewerPass(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	insertRevisionTestPrograms(t, pool, ctx)
	rules := pgstore.NewRules(pool, []string{db.DefaultSite})
	rule, err := rules.Create(ctx, revisionTestRuleInput("P"))
	if err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	r := newRevisionTestRuler(pool)
	newerPassRan := false
	beforeRulerWriteTx = func() {
		if newerPassRan {
			return
		}
		newerPassRan = true
		if _, err := rules.Update(ctx, rule.ID, revisionTestRuleInput("Q")); err != nil {
			t.Fatalf("editing rule during old pass: %v", err)
		}
		hook := beforeRulerWriteTx
		beforeRulerWriteTx = nil
		err := r.runPassForSite(ctx, db.DefaultSite)
		beforeRulerWriteTx = hook
		if err != nil {
			t.Fatalf("running newer pass: %v", err)
		}
	}
	t.Cleanup(func() { beforeRulerWriteTx = nil })

	if err := r.runPassForSite(ctx, db.DefaultSite); err != nil {
		t.Fatalf("runPassForSite: %v", err)
	}
	assertRevisionTestReservations(t, pool, ctx, 2)
}

func TestRunPass_InputRevisionMismatchLimit(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	insertRevisionTestPrograms(t, pool, ctx)
	if _, err := pgstore.NewRules(pool, []string{db.DefaultSite}).Create(ctx, revisionTestRuleInput("P")); err != nil {
		t.Fatalf("creating rule: %v", err)
	}
	r := newRevisionTestRuler(pool)
	mismatches := 0
	beforeRulerWriteTx = func() {
		mismatches++
		if _, err := sqlcgen.New(pool).BumpRulerInputRevision(ctx, db.DefaultSite); err != nil {
			t.Fatalf("bumping input revision: %v", err)
		}
	}
	t.Cleanup(func() { beforeRulerWriteTx = nil })

	err := r.runPassForSite(ctx, db.DefaultSite)
	if err == nil {
		t.Fatal("runPassForSite() error = nil, want error after repeated revision changes")
	}
	if mismatches != 3 {
		t.Fatalf("revision mismatches = %d, want 3", mismatches)
	}
	ids := readRevisionTestReservations(t, pool, ctx)
	if len(ids) != 0 {
		t.Fatalf("reservations after failed pass = %v, want none", ids)
	}
}

func revisionTestRuleInput(keyword string) store.RuleInput {
	return store.RuleInput{
		Name:  "revision test",
		Sites: []string{db.DefaultSite},
		TextMatches: []store.RuleTextMatch{{
			Target: "name", Mode: "keyword", Value: keyword, CaseSensitive: true,
		}},
	}
}

func insertRevisionTestPrograms(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, 32736, 1024, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, db.DefaultSite); err != nil {
		t.Fatalf("inserting service fixture: %v", err)
	}
	startAt := time.Now().Add(24 * time.Hour)
	for _, program := range []struct {
		id    int64
		name  string
		event int32
	}{{1, "P program", 1}, {2, "Q program", 2}} {
		if _, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description
) VALUES ($1, $2, 32736, 1024, $3, $4::timestamptz, 1800000, $4::timestamptz + interval '30 minutes', true, $5, '')`,
			db.DefaultSite, program.id, program.event, startAt, program.name); err != nil {
			t.Fatalf("inserting program %d fixture: %v", program.id, err)
		}
	}
}

func newRevisionTestRuler(pool *pgxpool.Pool) *Ruler {
	passes := pgstore.NewReservations(pool)
	return New([]string{db.DefaultSite}, passes, pgstore.NewBreakers(pool), nil, nil, nil, pgstore.NewEPG(pool))
}

func assertRevisionTestReservations(t *testing.T, pool *pgxpool.Pool, ctx context.Context, want int64) {
	t.Helper()
	ids := readRevisionTestReservations(t, pool, ctx)
	if len(ids) != 1 || ids[0] != want {
		t.Fatalf("reservation program IDs = %v, want [%d]", ids, want)
	}
}

func readRevisionTestReservations(t *testing.T, pool *pgxpool.Pool, ctx context.Context) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT program_id FROM reservations WHERE site = $1 ORDER BY program_id`, db.DefaultSite)
	if err != nil {
		t.Fatalf("querying reservations: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning reservation: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading reservations: %v", err)
	}
	return ids
}
