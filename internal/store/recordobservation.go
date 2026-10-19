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

// IngestPriorityAfterFinish は、録画の完了後に初めて取り込む ingest に付ける優先度である。
// 録画中に始める ingest は番組長のあいだ worker 枠を保持するので、完了後に取り込むものは
// 優先度を下げて、録画中の取り込みを待たせない。録画中の ingest は ingest ジョブの既定の優先度を使う。
const IngestPriorityAfterFinish = 2

// ProcessResult は録画観測の commit 後に呼び出し側が行う処理に必要な結果を返す。
type ProcessResult struct {
	// RecordingID は観測を帰属させた録画である。録画に帰属させなかったときは nil。
	RecordingID *int64
	// PreviousStatus は観測前の録画の状態である。呼び出し側は webhook を
	// finished への遷移だけに絞るために使う。sweep が同じ finished の record を
	// 何度処理しても、再通知しないためである。録画を新しく作ったときは空。
	PreviousStatus string
	// Title は通知に載せる録画の題名である。
	Title string
	// EnqueueIngest は、commit 後に ingest ジョブを投入すべきかを表す。録画に帰属した
	// recording / finished の record だけが true になる。投入はヒントであり、失っても
	// 定期の record sweep が投入し直す。
	EnqueueIngest bool
	// IngestPriority は ingest ジョブの優先度である。0 はジョブの既定を使う。
	IngestPriority int
	// FailedQualityEventAdded は、失敗理由の品質イベントをこの観測で初めて追記したかを表す。
	// 呼び出し側はこれが true のときだけ失敗のメトリクスと通知を出す。
	FailedQualityEventAdded bool
	// FailedRecordingSkipped は、失敗の観測を録画に帰属させなかったことを表す。同じ放送イベントに
	// 生きた失敗でない録画（本物の成功 record が先に枠を得たもの）があるとき、失敗をその録画へ
	// 帰属させず、録画も作らない。ProcessRecord は record 同期行を録画なしの観測として残し、
	// ingest は起きない。
	FailedRecordingSkipped bool
}

// RecordObservations は watcher が録画観測と失敗イベントを永続化する契約である。
// 書き込みメソッドは自身のトランザクションを commit してから返す。
type RecordObservations interface {
	// HasReservation は、放送イベントの予約が現在存在するかを返す。
	// watcher が mirakc への問い合わせの前に、観測すべき失敗かを絞るために使う。
	HasReservation(ctx context.Context, site string, programID int64) (bool, error)
	// ProcessRecord は record 同期行、録画行、品質イベントを原子的に反映する。
	// 同じ (site, record_id) の観測は直列化される。後発は先発の commit 後の状態を見るので、
	// 同じ record に録画が 2 つできることも、後発が一意違反で失敗することもない。
	// 録画の作成が同じ放送イベントの生きた録画と衝突したときは ErrConflict を返し、
	// record 同期行を含めて何も残さない。
	ProcessRecord(ctx context.Context, observation RecordObservation) (ProcessResult, error)
	// ProcessFailedSchedule は予約のある失敗 schedule を再構成し、同じ event/reason は 1 度だけ記録する。
	// 同じ放送イベントを RecordFailedRecording が並行して観測しても、生きた失敗の録画は 1 つに収束する。
	// 同じ放送イベントに生きた失敗でない録画があるときは、それへ失敗を追記せず FailedRecordingSkipped を返す。
	// 予約が無ければ何もしない。
	ProcessFailedSchedule(ctx context.Context, observation FailedScheduleObservation) (ProcessResult, error)
	// RecordFailedRecording は recording.failed の観測を記録し、対象の録画 ID を返す。
	// 予約が消えていたときは何も記録せず recorded=false を返す。
	// 同じ放送イベントを ProcessFailedSchedule が並行して観測しても、生きた失敗の録画は 1 つに収束する。
	RecordFailedRecording(ctx context.Context, observation FailedRecordingObservation) (recordingID int64, recorded bool, err error)
	// AppendRecordBrokenEvent は record に紐づく録画があれば破損イベントを追記する。
	AppendRecordBrokenEvent(ctx context.Context, site, recordID, reason string) (bool, error)
	// DeleteStaleRecordSyncs は全量観測に無い record 同期行を削除する。
	// 観測時刻ではなく、呼び出し側が渡した生きている record の ID の集合で判定する。
	// 渡した ID の行は観測時刻によらず残し、渡していない行だけを消す。
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
