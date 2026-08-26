// Package serverevent は notifier が SSE へ配送する型付きの揮発イベントを定義する。
package serverevent

const (
	// EncodeProgressEventType はエンコード進捗 SSE の event 名。
	EncodeProgressEventType = "encode-progress"
)

// EncodeProgressEvent は永続化しないエンコード進捗の wire payload。
// 完了・失敗の判定には使わず、次のイベントで上書きされる表示だけに使う。
type EncodeProgressEvent struct {
	Type        string  `json:"type"`
	RecordingID int64   `json:"recordingId"`
	Profile     string  `json:"profile"`
	Progress    float64 `json:"progress"`
}
