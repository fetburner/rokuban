package store

import (
	"context"

	"github.com/fetburner/rokuban/internal/catalogdoc"
)

// RestoreOptions は catalog の復元前に呼び出し側が確認したファイル状態を伝える。
type RestoreOptions struct {
	// MissingAssetIDs は復元時点でファイルが無かった active asset の ID。
	// 該当 asset は履歴を保って deleted として復元する。
	MissingAssetIDs map[int64]struct{}
}

// RestoreResult は catalog の復元件数と、復元できなかった行の件数を表す。
type RestoreResult struct {
	Rules                      int
	LabelRules                 int
	Recordings                 int
	RecordingEncodePolicies    int
	RecordingPurgeRequests     int
	RecordingWatched           int
	MediaAssets                int
	DropStats                  int
	DropPositions              int
	MediaAssetTSScans          int
	ProgramSnapshots           int
	ProgramIntents             int
	ProgramOverrides           int
	RecordingChapterOwnerships int
	RecordingChapterSpans      int
	SkippedProgramSnapshots    int
	SkippedProgramIntents      int
	SkippedProgramOverrides    int
	MissingMediaFiles          int
}

// Catalog は保護対象データの一貫した export と原子的な restore を行う。
type Catalog interface {
	// Export は全保護対象を単一のスナップショットから読む。
	Export(ctx context.Context) (*catalogdoc.Document, error)
	// Restore は文書内の全テーブルを 1 tx で復元する。分類ルールの再評価も同じ tx に含む。
	// 呼び出し側は先に media asset のファイルロックを取得し、Restore の完了まで保持する。
	// Restore は分類ルールの再評価と直列化し、最初の書き込みより前にその判断を確定する。
	Restore(ctx context.Context, document *catalogdoc.Document, options RestoreOptions) (*RestoreResult, error)
}
