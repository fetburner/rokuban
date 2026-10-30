package store_test

import (
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// TestPlanEncodedPublish は旧 worker の公開計画の真理値表を、期待値のリテラルで固定する。
func TestPlanEncodedPublish(t *testing.T) {
	keepA := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	keepB := []chapters.Range{{StartMs: 0, EndMs: 2000}}
	num := func(n int64) *int64 { return &n }
	type observed struct {
		compare bool
		gen     *int64
	}
	noCompare := observed{}
	sawNoRow := observed{compare: true}
	saw := func(n int64) observed { return observed{compare: true, gen: num(n)} }
	asset := func(state string, generation int64, keep []chapters.Range) *store.EncodedAssetObservation {
		return &store.EncodedAssetObservation{Active: state == "active", DerivedGeneration: generation, KeepRanges: keep}
	}
	input := func(cut bool, keep []chapters.Range) store.EncodedPublishInput {
		return store.EncodedPublishInput{Cut: cut, KeepRanges: keep}
	}
	state := func(asset *store.EncodedAssetObservation) store.EncodedPublishState {
		return store.EncodedPublishState{Desired: true, EncodedAsset: asset}
	}
	tests := []struct {
		name     string
		input    store.EncodedPublishInput
		state    store.EncodedPublishState
		previous observed
		want     store.EncodedPublishPlan
	}{
		{"no row, first observation", input(false, nil), state(nil), noCompare, store.EncodedPublishPlan{}},
		{"no row, previous empty", input(false, nil), state(nil), sawNoRow, store.EncodedPublishPlan{}},
		{"no row, previous had a path", input(false, nil), state(nil), saw(1), store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipAssetAdvanced}},
		{"no row, previous had a generation 0 row", input(false, nil), state(nil), saw(0), store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipAssetAdvanced}},
		{"generation 0 row, previous had no row", input(false, nil), state(asset("tombstone", 0, nil)), sawNoRow, store.EncodedPublishPlan{ObservedGeneration: num(0), Skip: true, SkipReason: store.EncodedPublishSkipAssetAdvanced}},
		{"active non-cut", input(false, nil), state(asset("active", 1, nil)), noCompare, store.EncodedPublishPlan{ObservedGeneration: num(1), Skip: true, SkipReason: store.EncodedPublishSkipAssetSatisfied}},
		{"active non-cut, moved", input(false, nil), state(asset("active", 2, nil)), saw(1), store.EncodedPublishPlan{ObservedGeneration: num(2), Skip: true, SkipReason: store.EncodedPublishSkipAssetSatisfied}},
		{"active cut, same keep", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepA, EncodedAsset: asset("active", 1, keepA)}, noCompare, store.EncodedPublishPlan{ObservedGeneration: num(1), Replaces: true, Skip: true, SkipReason: store.EncodedPublishSkipAssetSatisfied}},
		{"active cut rebuild, different keep", func() store.EncodedPublishInput {
			in := input(true, keepB)
			in.HasRebuildRequest = true
			in.RequestedKeep = keepB
			return in
		}(), store.EncodedPublishState{Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keepB, EncodedAsset: asset("active", 1, keepA)}, noCompare, store.EncodedPublishPlan{ObservedGeneration: num(1), Replaces: true}},
		{"active cut ordinary cannot replace", input(true, keepB), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepB, EncodedAsset: asset("active", 1, keepA)}, noCompare, store.EncodedPublishPlan{ObservedGeneration: num(1), Replaces: true, Skip: true, SkipReason: store.EncodedPublishSkipActiveCutProtected}},
		{"active cut, different keep, moved", input(true, keepB), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepB, EncodedAsset: asset("active", 2, keepA)}, saw(1), store.EncodedPublishPlan{ObservedGeneration: num(2), Replaces: true, Stale: true}},
		{"active cut, same keep, moved", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepA, EncodedAsset: asset("active", 2, keepA)}, saw(1), store.EncodedPublishPlan{ObservedGeneration: num(2), Replaces: true, Skip: true, SkipReason: store.EncodedPublishSkipAssetSatisfied}},
		{"tombstone, unmoved", input(false, nil), state(asset("tombstone", 1, nil)), saw(1), store.EncodedPublishPlan{ObservedGeneration: num(1)}},
		{"tombstone, moved", input(false, nil), state(asset("tombstone", 2, nil)), saw(1), store.EncodedPublishPlan{ObservedGeneration: num(2), Skip: true, SkipReason: store.EncodedPublishSkipAssetAdvanced}},
		{"deleting cut, moved", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepA, EncodedAsset: asset("deleting", 2, keepA)}, saw(1), store.EncodedPublishPlan{ObservedGeneration: num(2), Skip: true, SkipReason: store.EncodedPublishSkipAssetAdvanced}},
		{"deleting cut, first observation", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepA, EncodedAsset: asset("deleting", 2, keepA)}, noCompare, store.EncodedPublishPlan{ObservedGeneration: num(2)}},
		{"desired profile was removed", input(false, nil), store.EncodedPublishState{EncodedAsset: asset("active", 1, nil)}, noCompare, store.EncodedPublishPlan{ObservedGeneration: num(1), Skip: true, SkipReason: store.EncodedPublishSkipDesiredProfileExcluded}},
		{"rebuild request missing", func() store.EncodedPublishInput {
			in := input(true, keepA)
			in.HasRebuildRequest = true
			in.RequestedKeep = keepA
			return in
		}(), store.EncodedPublishState{Desired: true}, noCompare, store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipRebuildRequestMissing}},
		{"rebuild request overwritten", func() store.EncodedPublishInput {
			in := input(true, keepA)
			in.HasRebuildRequest = true
			in.RequestedKeep = keepA
			return in
		}(), store.EncodedPublishState{Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keepB}, noCompare, store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipRebuildRequestOverwritten}},
		{"rebuild request matches, no asset", func() store.EncodedPublishInput {
			in := input(true, keepA)
			in.HasRebuildRequest = true
			in.RequestedKeep = keepA
			return in
		}(), store.EncodedPublishState{Desired: true, HasRebuildRequest: true, RebuildRequestKeep: keepA}, noCompare, store.EncodedPublishPlan{}},
		{"ordinary cut owned and unchanged, no asset", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepA}, noCompare, store.EncodedPublishPlan{}},
		{"ordinary cut lost ownership", input(true, keepA), store.EncodedPublishState{Desired: true, CurrentCutKeep: keepA}, noCompare, store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipCutOwnershipMissing}},
		{"ordinary cut keep changed", input(true, keepA), store.EncodedPublishState{Desired: true, ChapterOwned: true, CurrentCutKeep: keepB}, noCompare, store.EncodedPublishPlan{Skip: true, SkipReason: store.EncodedPublishSkipCutKeepChanged}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.input
			in.ObservedGeneration = tt.previous.gen
			got := store.PlanEncodedPublish(in, tt.state, tt.previous.compare)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PlanEncodedPublish = %+v, want %+v", got, tt.want)
			}
		})
	}
}
