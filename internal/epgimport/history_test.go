package epgimport

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestImportHistory_IdempotentRerun is the acceptance criterion: re-running
// the same history import must not add rows.
func TestImportHistory_IdempotentRerun(t *testing.T) {
	pool := testutil.SetupDB(t)
	q := sqlcgen.New(pool)
	ctx := context.Background()

	items := []HistoryItem{{ChannelID: 3273601024, ChannelType: "GR", Name: "旧番組", EndAt: 1785001800000}}

	first, err := ImportHistory(ctx, q, "default", items)
	if err != nil {
		t.Fatalf("first ImportHistory: %v", err)
	}
	if first.Registered != 1 {
		t.Fatalf("first.Registered = %d, want 1", first.Registered)
	}

	second, err := ImportHistory(ctx, q, "default", items)
	if err != nil {
		t.Fatalf("second ImportHistory: %v", err)
	}
	if second.Registered != 1 {
		t.Fatalf("second.Registered = %d, want 1", second.Registered)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recordings count = %d, want 1 (rerun must not duplicate)", count)
	}

	var deletedAtSet bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM recordings`).Scan(&deletedAtSet); err != nil {
		t.Fatal(err)
	}
	if !deletedAtSet {
		t.Error("history-imported recording must be tombstoned (deleted_at set) — it never had rokuban-managed files")
	}
}

// TestSyntheticEventID_DeterministicAndDistinct: re-importing the same
// (name, endAt) must produce the same synthetic event id (the idempotency
// key for rows with no real broadcast event id), and different inputs must
// not collide trivially.
func TestSyntheticEventID_DeterministicAndDistinct(t *testing.T) {
	a1 := syntheticEventID("番組A", 1785001800000)
	a2 := syntheticEventID("番組A", 1785001800000)
	if a1 != a2 {
		t.Fatalf("syntheticEventID not deterministic: %d != %d", a1, a2)
	}
	if a1 >= 0 {
		t.Errorf("syntheticEventID = %d, want negative (must not collide with real mirakc event ids)", a1)
	}
	b := syntheticEventID("番組B", 1785001800000)
	if a1 == b {
		t.Errorf("different names produced the same synthetic event id: %d", a1)
	}
}
