package worker

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

// ValidateMediaAssetRelPathNamespace は worker が起動する前に、原本と encoded の
// 生きた media_assets 行が `sites/{site}/` 名前空間に入っていることを検査する。
//
// 述語は前方一致ではなく空でない site セグメント 1 つを要求する正規表現である。
// `LIKE 'sites/%'` は `sites/show.m2ts` を、`LIKE 'sites/_%/%'` は `_` が `/` にも
// 当たるため `sites//a/b` を通してしまう。どちらも
// classifySiteForRescuedFile（internal/catalog/rescue_scan.go）は site を決められず
// 空文字を返すので、この検査を通った行が DB 喪失後の rescue では復元を拒否され、
// orphan として報告される --- 「移行済み」の主張と実際の rescue 可否が食い違う。
//
// 前置導入前の行や、移行前のバックアップから復元した DB を worker がそのまま
// 読むと、ingest・encode・rescue・delete_reconcile の各処理が同じ storage 契約を
// 共有できない。worker はファイルを書き / 移動する唯一のロールなので、最初の
// 仕事を始める前に拒否して、移行漏れをその場で発見できるようにする（streamer は
// `media_assets.rel_path` を読んで配信するが、書きはしない）。thumbnail と seek_tiles は
// `thumbnails/` 配下の別の名前空間なので検査対象にしない。
func ValidateMediaAssetRelPathNamespace(ctx context.Context, media store.MediaReconcile) error {
	violation, err := media.FindInvalidMediaAssetRelPath(ctx)
	if err != nil {
		return fmt.Errorf("checking media_assets rel_path namespace: %w", err)
	}
	if violation == nil {
		return nil
	}

	return fmt.Errorf(
		"media_assets row %d (%s) has rel_path %q without the required sites/{site}/ prefix; migrate media_assets before starting the worker",
		violation.ID, violation.Kind, violation.RelPath,
	)
}
