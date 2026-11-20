package store

import "testing"

func TestNewStoragePolicyRequiresExplicitPlacement(t *testing.T) {
	policy := NewStoragePolicy([]OriginalStoreRule{{Store: "archive"}}, []string{"archive"}, "archive")
	if policy.PlacementEnabled() {
		t.Fatal("NewStoragePolicy enabled placement deletion without an explicit opt-in")
	}
	if !policy.WithPlacementEnabled(true).PlacementEnabled() {
		t.Fatal("WithPlacementEnabled(true) did not enable placement deletion")
	}
}
