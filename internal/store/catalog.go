package store

import (
	"context"

	"github.com/fetburner/rokuban/internal/catalogdoc"
)

// RestoreOptions は catalog の復元前に呼び出し側が確認したファイル状態を伝える。
type RestoreOptions struct {
	// MissingAssetCopyKeys は復元時点でファイルが無かった active コピーのキー。
	// 該当コピーは deleted として復元し、論理資産の状態は復元後の全コピーから再計算する。
	MissingAssetCopyKeys map[MediaAssetCopyKey]struct{}
}

// MediaAssetCopyKey は catalog 内の物理コピーを同定する。
type MediaAssetCopyKey struct {
	MediaAssetID int64
	Store        string
	RelPath      string
}

// RestoreResult は catalog の復元件数と、復元できなかった行の件数を表す。
type RestoreResult struct {
	Rules                   int
	LabelRules              int
	Recordings              int
	RecordingEncodePolicies int
	RecordingPurgeRequests  int
	RecordingWatched        int
	MediaAssets             int
	DropStats               int
	DropPositions           int
	MediaAssetTSScans       int
	ProgramSnapshots        int
	ProgramIntents          int
	ProgramOverrides        int
	// チャプターの所有 2 表（ユーザーの手作業）。ここが 0 でも rescue は成功する
	// （所有していない録画が大半なので、件数だけでは復元漏れを判定できない）。
	RecordingChapterOwnerships int
	RecordingChapterSpans      int

	// SkippedProgramSnapshots は識別子が壊れていて復元できなかった
	// program_snapshots の件数。0 でなければ依存する program_intents /
	// program_overrides も落ちている。黙って切り捨てず呼び出し側が報告する。
	SkippedProgramSnapshots int
	SkippedProgramIntents   int
	SkippedProgramOverrides int

	// MissingMediaFiles は復元時点でファイルが無く、deleted として復元した
	// active コピーの件数（RestoreOptions.MissingAssetCopyKeys の該当数）。
	MissingMediaFiles int
}

// Catalog は保護対象データの一貫した export と原子的な restore を行う。
type Catalog interface {
	// Export は全保護対象を単一のスナップショットから読む。
	Export(ctx context.Context) (*catalogdoc.Document, error)
	// Restore は文書内の全テーブルを 1 tx で復元する。同じ tx で分類ルールを再評価し、
	// 構成の全 site の ruler 入力の版を 1 ずつ進める。
	// 呼び出し側は先に media asset のファイルロックを取得し、Restore の完了まで保持する。
	// Restore は分類ルールの再評価と直列化する。直列化は最初の書き込みより前から始まる。
	Restore(ctx context.Context, document *catalogdoc.Document, options RestoreOptions) (*RestoreResult, error)
}
