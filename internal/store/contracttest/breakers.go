package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/store"
)

// BreakersFixture は breaker の読み書き契約をまとめる。
type BreakersFixture struct {
	Breakers store.Breakers
	Admin    store.BreakerAdmin
}

// NewBreakersStore は契約テスト用の breaker store を作る。
type NewBreakersStore func(t *testing.T) BreakersFixture

// RunBreakers は breaker の発動、一覧、再開の契約を実装に対して実行する。
func RunBreakers(t *testing.T, newStore NewBreakersStore) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	const scope = "contract-breaker"
	const name = "contract_breaker"

	if tripped, err := f.Breakers.IsTripped(ctx, scope, name); err != nil || tripped {
		t.Fatalf("IsTripped(initial) = (%t, %v), want (false, nil)", tripped, err)
	}
	sample := breaker.Sample{Total: 4, Programs: []breaker.SampleProgram{{ProgramID: 44, Title: "contract"}}}
	firstTrippedAt, err := f.Breakers.Trip(ctx, scope, name, 3, sample)
	if err != nil {
		t.Fatalf("Trip: %v", err)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, scope, name); err != nil || !tripped {
		t.Fatalf("IsTripped(after trip) = (%t, %v), want (true, nil)", tripped, err)
	}
	initialRows, err := f.Admin.List(ctx)
	if err != nil {
		t.Fatalf("List(before repeated trip): %v", err)
	}
	var initialTrippedAt time.Time
	for _, row := range initialRows {
		if row.Scope == scope && row.Name == name {
			initialTrippedAt = row.TrippedAt
			break
		}
	}
	if initialTrippedAt.IsZero() {
		t.Fatalf("List(before repeated trip) = %+v, missing initial breaker row", initialRows)
	}
	if !firstTrippedAt.Equal(initialTrippedAt) {
		t.Fatalf("Trip returned tripped_at %v, want List's TrippedAt %v", firstTrippedAt, initialTrippedAt)
	}
	updatedSample := breaker.Sample{Total: 6, Programs: []breaker.SampleProgram{{ProgramID: 45, Title: "updated"}}}
	repeatedTrippedAt, err := f.Breakers.Trip(ctx, scope, name, 5, updatedSample)
	if err != nil {
		t.Fatalf("Trip(repeated): %v", err)
	}
	if !repeatedTrippedAt.Equal(initialTrippedAt) {
		t.Fatalf("Trip(repeated) returned tripped_at %v, want the retained %v", repeatedTrippedAt, initialTrippedAt)
	}
	const otherScope = "contract-breaker-other"
	if _, err := f.Breakers.Trip(ctx, otherScope, name, 3, breaker.Sample{Total: 2}); err != nil {
		t.Fatalf("Trip(other scope): %v", err)
	}
	rows, err := f.Admin.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List after trips = %+v, want both scopes", rows)
	}
	seenScope, seenOtherScope := false, false
	for _, row := range rows {
		switch row.Scope {
		case scope:
			seenScope = true
			if row.Name != name || row.Pending != updatedSample.Total || row.Threshold != 5 || !row.TrippedAt.Equal(initialTrippedAt) {
				t.Fatalf("List row for scope %q = %+v, want the tripped row", scope, row)
			}
			var got breaker.Sample
			if err := json.Unmarshal(row.Detail, &got); err != nil {
				t.Fatalf("unmarshalling detail for scope %q: %v", scope, err)
			}
			if got.Total != updatedSample.Total || len(got.Programs) != 1 || got.Programs[0] != updatedSample.Programs[0] {
				t.Fatalf("detail for scope %q = %+v, want latest sample %+v", scope, got, updatedSample)
			}
		case otherScope:
			seenOtherScope = true
			if row.Name != name {
				t.Fatalf("List row for other scope = %+v, want breaker name %q", row, name)
			}
		}
	}
	if !seenScope || !seenOtherScope {
		t.Fatalf("List scopes = %+v, want both %q and %q", rows, scope, otherScope)
	}

	if err := f.Admin.Resume(ctx, scope, name); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, scope, name); err != nil || tripped {
		t.Fatalf("IsTripped(after resume) = (%t, %v), want (false, nil)", tripped, err)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, otherScope, name); err != nil || !tripped {
		t.Fatalf("IsTripped(other scope after resume) = (%t, %v), want (true, nil)", tripped, err)
	}
	if err := f.Admin.Resume(ctx, scope, name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Resume(absent) = %v, want ErrNotFound", err)
	}
}
