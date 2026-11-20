package store

import "context"

// TSScanSource は読み出し時の原本と、保存時に再検証する内容のリビジョンを持つ。
type TSScanSource struct {
	MediaAsset
	ContentRevision int64
}

// TSScan は TS scan worker が対象の読み取りと計測結果の保存に使う契約である。
type TSScan interface {
	// GetActiveOriginalForTSScan は計測が必要な active 原本を返す。対象が無ければ ErrNotFound を返す。
	GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (TSScanSource, error)
	// ListMissingTSScanRecordings は計測記録の無い、またはサイズが古い原本をページ単位で返す。
	ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, limit int32) ([]int64, error)
	// ReplaceScan は計測値と計測サイズを 1 tx で置き換える。
	// 原本の削除・内容変更と直列化してから、原本が active のままか、内容のリビジョンとサイズが読み出し時と同じかを再確認する。
	// 対象が無効か読み出し時から変わっていれば false, nil を返し、何も書かない。
	// 途中の書き込みが失敗したら、以前の計測値と計測サイズが残る。
	// 検査: contracttest.RunTSScan の「replaces statistics and marker together」（サイズ変化で置き換えない）、
	// 「rechecks active state after waiting for the asset lock」、「rolls back statistics and marker after a failed replacement」。
	// 内容変更との直列化とリビジョンの再確認は、契約テストでは未検証。
	ReplaceScan(ctx context.Context, source TSScanSource, scannedSizeBytes int64, stats []RecordingDropStat, positions []RecordingDropPosition) (bool, error)
}
