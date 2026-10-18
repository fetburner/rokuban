package store

import (
	"context"
	"encoding/json"
	"time"
)

// RecordingSnapshot は録画作成時に一度だけ保存する番組とサービスの観測値を表す。
type RecordingSnapshot struct {
	NetworkID         int32
	ServiceID         int32
	EventID           int32
	ServiceName       string
	ChannelType       string
	Channel           string
	Title             string
	Description       *string
	Extended          json.RawMessage
	Genres            json.RawMessage
	IsFree            bool
	ProgramStartAt    time.Time
	ProgramDurationMs int64
}

// RecordObservation は mirakc の 1 record に対する観測結果を store へ渡す。
type RecordObservation struct {
	Site          string
	RecordID      string
	ProgramID     int64
	Snapshot      RecordingSnapshot
	Managed       bool
	Tags          []string
	Status        string
	StartedAt     *time.Time
	EndedAt       *time.Time
	ContentPath   *string
	ContentLength *int64
	FailedReason  json.RawMessage
}

// FailedScheduleObservation は schedules API から再構成した失敗を表す。
type FailedScheduleObservation struct {
	Site      string
	ProgramID int64
	Snapshot  RecordingSnapshot
	Reason    json.RawMessage
}

// FailedRecordingObservation は recording.failed イベントから再構成した失敗を表す。
type FailedRecordingObservation struct {
	Site      string
	ProgramID int64
	Snapshot  RecordingSnapshot
	Reason    json.RawMessage
}

// ProcessResult は録画観測の commit 後に呼び出し側が行う処理に必要な結果を返す。
type ProcessResult struct {
	RecordingID             *int64
	PreviousStatus          string
	Title                   string
	EnqueueIngest           bool
	IngestPriority          int
	FailedQualityEventAdded bool
	FailedRecordingSkipped  bool
}

// RecordObservations は watcher が録画観測と失敗イベントを永続化する契約である。
// 書き込みメソッドは自身のトランザクションを commit してから返す。
type RecordObservations interface {
	// HasReservation は、放送イベントの予約が現在存在するかを返す。
	HasReservation(ctx context.Context, site string, programID int64) (bool, error)
	// ProcessRecord は record 同期行、録画行、品質イベントを原子的に反映する。
	// 同じ (site, record_id) の観測は直列化される。
	ProcessRecord(ctx context.Context, observation RecordObservation) (ProcessResult, error)
	// ProcessFailedSchedule は予約のある失敗 schedule を再構成し、同じ event/reason は 1 度だけ記録する。
	ProcessFailedSchedule(ctx context.Context, observation FailedScheduleObservation) (ProcessResult, error)
	// RecordFailedRecording は recording.failed の観測を記録し、対象の録画 ID を返す。
	// 予約が消えていた場合は 0 と nil を返す。
	RecordFailedRecording(ctx context.Context, observation FailedRecordingObservation) (int64, error)
	// AppendRecordBrokenEvent は record に紐づく録画があれば破損イベントを追記する。
	AppendRecordBrokenEvent(ctx context.Context, site, recordID, reason string) (bool, error)
	// DeleteStaleRecordSyncs は全量観測に無い record 同期行を削除する。
	DeleteStaleRecordSyncs(ctx context.Context, site string, liveRecordIDs []string) (int64, error)
	// MarkSweepComplete は record 全量突き合わせの成功時刻を更新する。
	MarkSweepComplete(ctx context.Context, site string) error
}

// RecordIngestTarget は取り込み先と進捗の分母に使う観測値を表す。
type RecordIngestTarget struct {
	RecordingID   *int64
	ContentLength *int64
}

// IngestTarget は ingest worker が record_sync から転送先を読む契約である。
type IngestTarget interface {
	// GetRecordSyncIngestTarget は (site, record_id) の転送先と観測済み長さを返す。
	GetRecordSyncIngestTarget(ctx context.Context, site, recordID string) (RecordIngestTarget, error)
}

// RecordBacklogSnapshot は未取り込み record の件数と合計サイズを表す。
type RecordBacklogSnapshot struct {
	Records int64
	Bytes   int64
}

// RecordBacklog は metrics が未取り込み record の滞留量を読む契約である。
type RecordBacklog interface {
	// GetUningestedRecordBacklog は対象 site の未取り込み record の件数と合計サイズを返す。
	GetUningestedRecordBacklog(ctx context.Context, site string) (RecordBacklogSnapshot, error)
}
