package store_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// TestPlanEncodedPublish は旧 worker の公開計画の真理値表を、期待値のリテラルで固定する。
func TestPlanEncodedPublish(t *testing.T) {
	keepA := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	keepB := []chapters.Range{{StartMs: 0, EndMs: 2000}}
	str := func(s string) *string { return &s }
	asset := func(state, rel string, keep []chapters.Range) *store.EncodedAssetObservation {
		return &store.EncodedAssetObservation{RelPath: rel, State: state, KeepRanges: keep}
	}
	tests := []struct {
		name     string
		asset    *store.EncodedAssetObservation
		cut      bool
		keep     []chapters.Range
		previous *string
		want     store.EncodedPublishPlan
	}{
		{"no row, first observation", nil, false, nil, nil, store.EncodedPublishPlan{}},
		{"no row, previous empty", nil, false, nil, str(""), store.EncodedPublishPlan{}},
		{"no row, previous had a path", nil, false, nil, str("a"), store.EncodedPublishPlan{Skip: true}},
		{"active non-cut", asset("active", "a", nil), false, nil, nil, store.EncodedPublishPlan{ObservedRelPath: "a", Skip: true}},
		{"active non-cut, moved", asset("active", "b", nil), false, nil, str("a"), store.EncodedPublishPlan{ObservedRelPath: "b", Skip: true}},
		{"active cut, same keep", asset("active", "a", keepA), true, keepA, nil, store.EncodedPublishPlan{ObservedRelPath: "a", Replaces: true, Skip: true}},
		{"active cut, different keep", asset("active", "a", keepA), true, keepB, nil, store.EncodedPublishPlan{ObservedRelPath: "a", Replaces: true}},
		{"active cut, different keep, unmoved", asset("active", "a", keepA), true, keepB, str("a"), store.EncodedPublishPlan{ObservedRelPath: "a", Replaces: true}},
		{"active cut, different keep, moved", asset("active", "b", keepA), true, keepB, str("a"), store.EncodedPublishPlan{ObservedRelPath: "b", Replaces: true, Stale: true}},
		{"active cut, same keep, moved", asset("active", "b", keepA), true, keepA, str("a"), store.EncodedPublishPlan{ObservedRelPath: "b", Replaces: true, Skip: true}},
		{"tombstone, unmoved", asset("tombstone", "a", nil), false, nil, str("a"), store.EncodedPublishPlan{ObservedRelPath: "a"}},
		{"tombstone, moved", asset("tombstone", "b", nil), false, nil, str("a"), store.EncodedPublishPlan{ObservedRelPath: "b", Skip: true}},
		{"deleting cut, moved", asset("deleting", "b", keepA), true, keepA, str("a"), store.EncodedPublishPlan{ObservedRelPath: "b", Skip: true}},
		{"deleting cut, first observation", asset("deleting", "b", keepA), true, keepA, nil, store.EncodedPublishPlan{ObservedRelPath: "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.PlanEncodedPublish(tt.asset, tt.cut, tt.keep, tt.previous)
			if got != tt.want {
				t.Fatalf("PlanEncodedPublish = %+v, want %+v", got, tt.want)
			}
		})
	}
}
