package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/catalog"
)

const (
	// defaultCatalogExportInterval は catalog エクスポートの既定間隔（日次相当）。
	// docs/storage.md §8「日次 + 世代保持」。
	defaultCatalogExportInterval = 24 * time.Hour

	// catalogExportTimeout は 1 回の export（DB 読み + JSON 書き + 刈り取り）の上限。
	// 保護対象は数 MB 規模なので短くて足りるが、River 既定（1 分）より余裕を持たせる。
	catalogExportTimeout = 5 * time.Minute
)

// CatalogExportArgs は catalog エクスポートジョブの引数。
//
// Keep が 0 以下なら catalog.DefaultKeep（7）。catalog は常に全サイトを
// エクスポートする（site 絞り込みは書き手・呼び手が最後まで現れなかったため
// issue #441 で落とした）。
type CatalogExportArgs struct {
	Keep int `json:"keep,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (CatalogExportArgs) Kind() string { return "catalog_export" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// 同一引数の同時実行を UniqueOpts で防ぐ。ByState は pendingJobStates に絞る
// （completed を含めると定期ジョブが実質ワンショットになる）。
//
// Queue は cleanupQueue（issue #185 M4-13）。delete_reconcile と同じ理由
// （cleanupQueue のコメント参照）。
//
// ByQueue: uniqueByQueue の理由は pendingJobStates 直後の doc コメント参照
// （river.QueueDefault → cleanup への移設自体がキュー名変更なので、同じ問題を踏む）。
func (CatalogExportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: cleanupQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
		},
	}
}

// CatalogExportWorker はコアメタデータを media_dir/catalog/ に JSON で書き出す
// River ワーカー（docs/storage.md §8、issue #71）。
//
// site 照合ガード（issue #139）は不要と判断: mirakc には触れない（DB 読み取りと
// FS 書き出しのみ）。catalog は常に全サイトをエクスポートするので、他サイトの
// worker がこのジョブを掴んでも「他インスタンスの id を投げる」形の壊れ方が
// 起きない。物理ストレージも DeleteReconcileWorker と同じく site に従属しない
// 単一の MediaDir。
type CatalogExportWorker struct {
	river.WorkerDefaults[CatalogExportArgs]
	Pool     *pgxpool.Pool
	MediaDir string
}

// Timeout は 1 回の export の上限を返す。
func (w *CatalogExportWorker) Timeout(*river.Job[CatalogExportArgs]) time.Duration {
	return catalogExportTimeout
}

// Work は DB から保護対象を集めて catalog JSON を書き、古い世代を刈る。
func (w *CatalogExportWorker) Work(ctx context.Context, job *river.Job[CatalogExportArgs]) error {
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
		"program_intents", len(doc.ProgramIntents),
		"program_overrides", len(doc.ProgramOverrides),
	)
	return nil
}
