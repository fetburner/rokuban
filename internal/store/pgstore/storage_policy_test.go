package pgstore

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/store"
)

func TestOriginalRulesJSONMatchesLegacyBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original []config.OriginalStoreRule
		want     string
	}{
		{
			name:     "unconditional",
			original: []config.OriginalStoreRule{{Store: "main"}},
			want:     `[{"store":"main"}]`,
		},
		{
			name:     "site",
			original: []config.OriginalStoreRule{{Match: &config.OriginalStoreMatch{Site: "tokyo"}, Store: "archive"}},
			want:     `[{"match":{"site":"tokyo"},"store":"archive"}]`,
		},
		{
			name:     "older than",
			original: []config.OriginalStoreRule{{Match: &config.OriginalStoreMatch{OlderThan: "30d"}, Store: "archive"}},
			want:     `[{"match":{"older_than_us":2592000000000},"store":"archive"}]`,
		},
		{
			name:     "site and older than",
			original: []config.OriginalStoreRule{{Match: &config.OriginalStoreMatch{Site: "tokyo", OlderThan: "30d"}, Store: "archive"}},
			want:     `[{"match":{"site":"tokyo","older_than_us":2592000000000},"store":"archive"}]`,
		},
		{name: "empty", want: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := (config.StorageConfig{Originals: tc.original}).BuildStorePolicy()
			if err != nil {
				t.Fatal(err)
			}
			got, err := originalRulesJSON(policy)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("rules bytes = %s, want literal %s", got, tc.want)
			}
		})
	}
}

func TestOriginalRulesJSONRejectsSubMicrosecondThreshold(t *testing.T) {
	policy := store.NewStoragePolicy([]store.OriginalStoreRule{{
		OlderThan: time.Nanosecond,
		Store:     "archive",
	}}, nil, "")
	if _, err := originalRulesJSON(policy); err == nil {
		t.Fatal("originalRulesJSON accepted a sub-microsecond threshold")
	}
}

func TestOriginalRulesJSONZeroPolicyIsSafe(t *testing.T) {
	policy := store.StoragePolicy{}
	got, err := originalRulesJSON(policy)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[]` {
		t.Fatalf("zero policy rules = %s, want literal []", got)
	}
	if stores := policy.FSStores(); len(stores) != 0 {
		t.Fatalf("zero policy FS stores = %v, want none", stores)
	}
	if derived := policy.DerivedStore(); derived != "" {
		t.Fatalf("zero policy derived store = %q, want empty", derived)
	}
	if policy.PlacementEnabled() {
		t.Fatal("zero policy enables placement deletion")
	}
}
