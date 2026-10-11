package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/jobs"
)

// CatalogExportWorker はコアメタデータを media_dir/catalog/ に JSON で書き出す
// River ワーカー（docs/storage.md §8、issue #71）。
//
// site 照合ガード（issue #139）は不要と判断: mirakc には触れない（DB 読み取りと
// FS 書き出しのみ）。catalog は常に全サイトをエクスポートするので、他サイトの
// worker がこのジョブを掴んでも「他インスタンスの id を投げる」形の壊れ方が
// 起きない。物理ストレージも DeleteReconcileWorker と同じく site に従属しない
// 単一の MediaDir。
type CatalogExportWorker struct {
	river.WorkerDefaults[jobs.CatalogExportArgs]
	Pool     *pgxpool.Pool
	MediaDir string
}

// Timeout は 1 回の export の上限を返す。
func (w *CatalogExportWorker) Timeout(job *river.Job[jobs.CatalogExportArgs]) time.Duration {
	return jobTimeout(job)
}

// Work は DB から保護対象を集めて catalog JSON を書き、古い世代を刈る。
func (w *CatalogExportWorker) Work(ctx context.Context, job *river.Job[jobs.CatalogExportArgs]) error {
	if w.MediaDir == "" {
		return fmt.Errorf("media dir is empty")
	}

	doc, err := catalog.Export(ctx, w.Pool)
	if err != nil {
		return err
	}

	genDir, err := catalog.Write(w.MediaDir, doc, job.Args.Keep)
	if err != nil {
		return err
	}

	slog.Info("catalog exported",
		"generation_dir", genDir,
		"rules", len(doc.Rules),
		"recordings", len(doc.Recordings),
		"media_assets", len(doc.MediaAssets),
		"drop_stats", len(doc.DropStats),
		"drop_positions", len(doc.DropPositions),
		"media_asset_ts_scans", len(doc.MediaAssetTSScans),
		"program_intents", len(doc.ProgramIntents),
		"program_overrides", len(doc.ProgramOverrides),
	)
	return nil
}
