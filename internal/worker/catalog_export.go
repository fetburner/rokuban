package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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
// Site が空なら全サイト。Keep が 0 以下なら catalog.DefaultKeep（7）。
// サイト横断の災害復旧が主用途なので、site 省略が既定の運用形。
type CatalogExportArgs struct {
	Site string `json:"site,omitempty"`
	Keep int    `json:"keep,omitempty"`
}

// Kind は River ジョブの種別名を返す。
func (CatalogExportArgs) Kind() string { return "catalog_export" }

// InsertOpts は River ジョブの挿入オプションを返す。
//
// 同一引数の同時実行を UniqueOpts で防ぐ。ByState は pendingJobStates に絞る
// （completed を含めると定期ジョブが実質ワンショットになる）。
func (CatalogExportArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: river.QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// CatalogExportWorker はコアメタデータを media_dir/catalog/ に JSON で書き出す
// River ワーカー（docs/storage.md §8、issue #71）。
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

	doc, err := catalog.Export(ctx, sqlcgen.New(w.Pool), job.Args.Site)
	if err != nil {
		return err
	}

	path, err := catalog.Write(w.MediaDir, doc, job.Args.Keep)
	if err != nil {
		return err
	}

	slog.Info("catalog exported",
		"path", path,
		"site", job.Args.Site,
		"rules", len(doc.Rules),
		"recordings", len(doc.Recordings),
		"media_assets", len(doc.MediaAssets),
		"drop_stats", len(doc.DropStats),
		"program_intents", len(doc.ProgramIntents),
		"program_overrides", len(doc.ProgramOverrides),
	)
	return nil
}
