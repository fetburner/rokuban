package pgstore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/store"
)

func TestMapDatabaseErrorEPGStationRuleIDConflict(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         string
		constraint   string
		wantSpecific bool
		wantConflict bool
	}{
		{"migration key", "23505", "rules_epgstation_rule_id_unique", true, true},
		{"duplicate sites", "23505", "rule_sites_pkey", false, true},
		{"other unique constraint", "23505", "rules_pkey", false, true},
		{"exclusion constraint", "23P01", "rules_epgstation_rule_id_unique", false, true},
		{"other error", "23514", "rules_epgstation_rule_id_unique", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint}
			got := mapDatabaseError(fmt.Errorf("writing rule: %w", original))
			if errors.Is(got, store.ErrEPGStationRuleIDConflict) != tc.wantSpecific {
				t.Fatalf("specific conflict = %t, want %t: %v", errors.Is(got, store.ErrEPGStationRuleIDConflict), tc.wantSpecific, got)
			}
			if errors.Is(got, store.ErrConflict) != tc.wantConflict {
				t.Fatalf("general conflict = %t, want %t: %v", errors.Is(got, store.ErrConflict), tc.wantConflict, got)
			}
			if !errors.Is(got, original) {
				t.Fatalf("original database error lost: %v", got)
			}
		})
	}
}
