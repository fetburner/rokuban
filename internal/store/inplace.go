package store

import (
	"errors"
	"fmt"
)

// InPlaceCopyObservation は登録するアセットの rel_path で置き場を引いて見つかったコピーの行を表す。
// 方言は同じ tx の中で読んだ値をそのまま詰める。行が無いアセットには nil を渡す。
type InPlaceCopyObservation struct {
	// AssetID は行が属する論理資産の ID。
	AssetID int64
	// RecordingID は論理資産が属する録画の ID。
	RecordingID int64
	// Kind と Profile は論理資産の kind と profile。
	Kind    string
	Profile *string
	// CopyState はコピーの state（active・pending・deleting・deleted）。
	CopyState string
	// RecordingRemoved は録画の deleted_at か purged_at が入っていること（ごみ箱か消去済み）を表す。
	RecordingRemoved bool
}

// InPlaceRegistrationPlan は PlanInPlaceRegistration が決めた in-place 登録の判断である。
type InPlaceRegistrationPlan struct {
	// RecordingID は既存の行が指す録画の ID。0 なら録画を新しく upsert する。
	RecordingID int64
	// ReusedAssetIDs は i 番目のアセットが既存の生きた行として通ったときの論理資産の ID。
	// 0 なら論理資産を upsert する。
	ReusedAssetIDs []int64
}

// PlanInPlaceRegistration は in-place 登録で、rel_path で見つかった既存の行を検査する純関数である。
// observed は assets と同じ長さで、i 番目は assets[i].RelPath で置き場を引いた行（無ければ nil）。
// DB と tx には触れず、path も解釈しない。storeName はエラーの文言に入る置き場の名前である。
//
// 検査は assets の順に行い、最初に違反したアセットのエラーを返す。各アセットでは次の順に当てる。
//  1. 前のアセットで見つかった行と録画が違う → ErrConflict
//  2. 行が deleted（墓石）→ ErrConflict と ErrInPlacePathTombstoned
//  3. 録画がごみ箱か消去済み → ErrConflict
//  4. 行が active でない（deleting・pending）→ ErrConflict
//  5. kind または profile が要求と違う → ErrConflict
//
// どれにも当たらない行は、同じパスの active な再登録として通す。
func PlanInPlaceRegistration(storeName string, assets []InPlaceAsset, observed []*InPlaceCopyObservation) (InPlaceRegistrationPlan, error) {
	plan := InPlaceRegistrationPlan{ReusedAssetIDs: make([]int64, len(assets))}
	for i, asset := range assets {
		row := observed[i]
		if row == nil {
			continue
		}
		if plan.RecordingID != 0 && plan.RecordingID != row.RecordingID {
			return InPlaceRegistrationPlan{}, fmt.Errorf("in-place assets belong to different recordings: %d and %d: %w", plan.RecordingID, row.RecordingID, ErrConflict)
		}
		plan.RecordingID = row.RecordingID
		// 同じ置き場・パスの行は全体で 1 つなので、deleted 行は墓石である。
		if row.CopyState == "deleted" {
			return InPlaceRegistrationPlan{}, fmt.Errorf(
				"in-place asset %q is tombstoned in store %q; move or rename the file before registering it again: %w",
				asset.RelPath, storeName, errors.Join(ErrConflict, ErrInPlacePathTombstoned),
			)
		}
		// 同じパスの active な再登録は論理資産の upsert を通らないので、ごみ箱の録画はここで拒否する。
		if row.RecordingRemoved {
			return InPlaceRegistrationPlan{}, fmt.Errorf("in-place asset %q belongs to trashed recording %d: %w", asset.RelPath, row.RecordingID, ErrConflict)
		}
		if row.CopyState != "active" {
			return InPlaceRegistrationPlan{}, fmt.Errorf("in-place asset %q is %s and will be unlinked: %w", asset.RelPath, row.CopyState, ErrConflict)
		}
		if row.Kind != asset.Kind || !sameStringPointer(row.Profile, asset.Profile) {
			return InPlaceRegistrationPlan{}, fmt.Errorf("published asset %q is %s/%v, requested %s/%v: %w", asset.RelPath, row.Kind, row.Profile, asset.Kind, asset.Profile, ErrConflict)
		}
		plan.ReusedAssetIDs[i] = row.AssetID
	}
	return plan, nil
}

// sameStringPointer は 2 つの *string が同じ値（両方 nil を含む）かを返す。
func sameStringPointer(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
