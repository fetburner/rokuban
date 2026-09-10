package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const invalidMediaAssetRelPathQuery = `
SELECT id, kind, rel_path
FROM media_assets
WHERE state <> 'deleted'
  AND kind IN ('original', 'encoded')
  AND rel_path NOT LIKE 'sites/_%/%'
ORDER BY id
LIMIT 1
`

// ValidateMediaAssetRelPathNamespace は worker が起動する前に、原本と encoded の
// 生きた media_assets 行が `sites/{site}/` 名前空間に入っていることを検査する。
//
// site セグメントが空の `sites//...` や、セグメントの区切りが無い
// `sites/show.m2ts` も拒否する。`NOT LIKE 'sites/%'` だけだと後者を通してしまい、
// classifySiteForRescuedFile（internal/catalog/rescue_scan.go）は site を決められず
// 空文字を返すため、この検査を通った行が DB 喪失後の rescue では復元を拒否され、
// orphan 回収でエイジング後に消える --- 「移行済み」の主張と実際の rescue 可否が
// 食い違う。
//
// 前置導入前の行や、移行前のバックアップから復元した DB を worker がそのまま
// 読むと、ingest・encode・rescue・delete_reconcile の各処理が同じ storage 契約を
// 共有できない。worker はファイルを書き / 移動する唯一のロールなので、最初の
// 仕事を始める前に拒否して、移行漏れをその場で発見できるようにする（streamer は
// `media_assets.rel_path` を読んで配信するが、書きはしない）。thumbnail は
// `thumbnails/{recording_id}.jpg` という別の名前空間なので検査対象にしない。
func ValidateMediaAssetRelPathNamespace(ctx context.Context, pool *pgxpool.Pool) error {
	var id int64
	var kind, relPath string
	if err := pool.QueryRow(ctx, invalidMediaAssetRelPathQuery).Scan(&id, &kind, &relPath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking media_assets rel_path namespace: %w", err)
	}

	return fmt.Errorf(
		"media_assets row %d (%s) has rel_path %q without the required sites/{site}/ prefix; migrate media_assets before starting the worker",
		id, kind, relPath,
	)
}
