// delete_select.go は保持ポリシーによる削除候補の選定を担当する。
// 読む表は media_assets と、trash_deletable_recordings、
// until_encoded_deletable_originals、removed_encoded_assets が参照する表。
// 表への書き込みとファイルシステム操作は行わない。
//
// 選定は mediaDir を受け取らない自由関数なので、ファイルシステムに触れないことが
// コンパイル時に保証される。選定が返すのは候補だけで、until_encoded の述語は
// 実行の直前に delete_execute.go が再評価する。公開前孤児の候補は、ファイル配置の
// 規則（名前の分類と mtime の再確認）に依存するため、選定ではなく storage_audit.go が
// 出して deleteCandidateSet に詰める。

package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// deleteCandidateSet は 1 パスの新規物理削除対象。canonical 孤児は報告のみで含めない。
// ブレーカーはソースを問わず削除対象だけを合計する。
type deleteCandidateSet struct {
	trash                   []store.MediaDeleteAsset
	untilEncoded            []store.MediaDeleteAsset
	removedEncoded          []store.MediaDeleteAsset
	oldGeneration           []store.MediaDeleteAsset
	placement               []store.MediaAssetCopyDeleteCandidate
	obsoletePendingTransfer []store.MediaAssetCopyDeleteCandidate
	agedOrphans             []string
}

func (c deleteCandidateSet) total() int {
	seen := make(map[int64]struct{})
	for _, rows := range [][]store.MediaDeleteAsset{c.trash, c.untilEncoded, c.removedEncoded, c.oldGeneration} {
		for _, row := range rows {
			if row.ID > 0 {
				seen[row.ID] = struct{}{}
			}
		}
	}
	for _, row := range c.placement {
		if row.CopyID > 0 {
			seen[row.CopyID] = struct{}{}
		}
	}
	for _, row := range c.obsoletePendingTransfer {
		if row.CopyID > 0 {
			seen[row.CopyID] = struct{}{}
		}
	}
	return len(seen) + len(c.agedOrphans)
}

// collectDeleteCandidates は新規削除対象を 3 つの DB 述語から取得する。
//
// MediaDir を受け取らないため、削除元の選定はファイルシステムに触れない。
//
// 原本を入力とする active な encode/thumbnail ジョブの有無はここでは見ない
// （旧条件 3。docs/storage/retention.md「retention reconcile ループ」の
// 削除条件 1・2 の一覧直後の判断根拠参照）。until_encoded_deletable_originals
// （条件 2）が desired な派生物の完備を要求するため、出力未コミットの
// ジョブは既に条件 2 が止め、出力コミット済みのジョブは各ワーカーの冒頭の
// 冪等チェックが原本を開かずに skip する（順序そのものは未検証。
// docs/storage/retention.md 同箇所参照）。
func collectDeleteCandidates(ctx context.Context, media store.MediaDelete, trashCutoff, overlapCutoff time.Time) (deleteCandidateSet, error) {
	var c deleteCandidateSet
	var err error
	c.trash, err = media.ListTrashAssetsToDelete(ctx, trashCutoff, deleteReconcileRowLimit)
	if err != nil {
		return c, fmt.Errorf("listing trash assets past retention: %w", err)
	}
	c.untilEncoded, err = media.ListUntilEncodedOriginalsToDelete(ctx, deleteReconcileRowLimit)
	if err != nil {
		return c, fmt.Errorf("listing until_encoded originals: %w", err)
	}
	c.removedEncoded, err = media.ListRemovedEncodedAssetsToDelete(ctx, deleteReconcileRowLimit)
	if err != nil {
		return c, fmt.Errorf("listing removed encoded assets: %w", err)
	}
	c.oldGeneration, err = media.ListOldGenerationMediaAssetCopiesToDelete(ctx, overlapCutoff, deleteReconcileRowLimit)
	if err != nil {
		return c, fmt.Errorf("listing old-generation media asset copies: %w", err)
	}
	return c, nil
}
