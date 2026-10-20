package store

import "context"

// TSScan は TS scan worker が対象の読み取りと計測結果の保存に使う契約である。
type TSScan interface {
	// GetActiveOriginalForTSScan は計測が必要な active 原本を返す。対象が無ければ ErrNotFound を返す。
	GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (MediaAsset, error)
	// ListMissingTSScanRecordings は計測記録の無い、またはサイズが古い原本をページ単位で返す。
	ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, limit int32) ([]int64, error)
	// ReplaceScan は計測値と計測サイズを 1 tx で置き換える。
	// 原本の削除・サイズ変更と直列化し、確定後の状態でサイズと計測可否を再確認する。
	// 対象が既に無効かサイズが変わっていれば false, nil を返し、何も書かない。
	ReplaceScan(ctx context.Context, assetID, scannedSizeBytes int64, stats []RecordingDropStat, positions []RecordingDropPosition) (bool, error)
}
