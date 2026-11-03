package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// CatalogExportWorker はコアメタデータを、この worker が担当する FS 置き場の root
// 配下の catalog/ に JSON で書き出す queue worker（docs/storage.md §8、issue #71）。
//
// キューは置き場ごとの original_{store}（internal/jobs/spec.go）なので、MediaDir は
// その置き場の root になる。Export は全 site を対象にする。
//
// site 照合ガード（issue #139）は不要と判断: mirakc には触れない（DB 読み取りと
// FS 書き出しのみ）。catalog は常に全サイトをエクスポートするので、他サイトの
// worker がこのジョブを掴んでも「他インスタンスの id を投げる」形の壊れ方が
// 起きない。
type CatalogExportWorker struct {
	Store          store.Catalog
	MediaDir       string
	MediaStoreName string
}

// Work は DB から保護対象を集めて catalog JSON を書き、古い世代を刈る。
func (w *CatalogExportWorker) Work(ctx context.Context, args jobs.CatalogExportArgs) error {
	if w.MediaDir == "" {
		return fmt.Errorf("media dir is empty")
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "starting catalog export"); err != nil {
		return err
	}

	doc, err := w.Store.Export(ctx)
	if err != nil {
		return err
	}

	genDir, err := catalog.Write(w.MediaDir, doc, args.Keep,
		func() error { return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "writing catalog") })
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
