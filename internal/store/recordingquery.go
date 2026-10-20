package store

import (
	"context"
	"encoding/json"
	"time"
)

// RecordingServiceRef は録画検索の service 軸で使う network ID と service ID。
type RecordingServiceRef struct {
	NetworkID int32
	ServiceID int32
}

// RecordingFilter は録画一覧と棚の読み取り条件を表す。
// 空の文字列・スライス・nil ポインタは該当する検索軸を絞らない。
type RecordingFilter struct {
	Trash bool

	Q       string
	QTarget string // "title" または "titleDescription"。空は titleDescription。

	Genres       []int16
	ChannelTypes []string
	Sites        []string
	Services     []RecordingServiceRef

	Status             string
	EncodeRecordingIDs *[]int64
	Source             string
	RuleID             *int64
	SeriesOf           *int64

	From *time.Time
	To   *time.Time

	SortDesc         bool
	ContinueWatching bool

	Before   *time.Time
	BeforeID *int64

	Limit int
}

// RecordingQueryRow は録画読み取りが返す素の DB 事実と射影値を持つ。
// API 固有の状態導出は、呼び出し側がこの行から行う。
type RecordingQueryRow struct {
	ID                       int64
	Site                     string
	RuleID                   *int64
	Source                   string
	ServiceName              string
	ChannelType              string
	Channel                  string
	NetworkID                int32
	ServiceID                int32
	EventID                  int32
	Title                    string
	Description              *string
	ProgramStartAt           time.Time
	ProgramDurationMs        int64
	Status                   string
	StartedAt                *time.Time
	EndedAt                  *time.Time
	QualityEvents            json.RawMessage
	DeletedAt                *time.Time
	CreatedAt                time.Time
	OriginalSizeBytes        *int64
	DropPackets              int64
	DropDrops                int64
	DropErrors               int64
	DropScrambled            int64
	HasMeasuredDropSummary   bool
	KeepOriginal             string
	AvailableEncodedAssets   json.RawMessage
	ResumePositionMs         *int64
	ResumePositionUpdatedAt  *time.Time
	WatchedAt                *time.Time
	EncodeProfiles           []string
	EncodeAttempts           json.RawMessage
	ChaptersOwned            bool
	ChapterSpans             json.RawMessage
	Series                   *string
	SeriesKey                *string
	HasOriginalAsset         bool
	HasIngestableRecord      bool
	HasAbnormallyEndedRecord bool
	IngestWrittenBytes       *int64
	IngestExpectedBytes      *int64
	IngestObservedAt         *time.Time
	CMDetect                 bool
	CMDetected               bool
	CMRanges                 json.RawMessage
	CMAttemptState           *string
	CMAttemptStage           *string
	CMAttemptError           *string
}

// RecordingShelf は棚クエリが返す 1 つの実効シリーズの集計結果。
type RecordingShelf struct {
	Value            *string
	Title            string
	Count            int64
	PlayableCount    int64
	UnwatchedCount   int64
	LatestStartAt    time.Time
	RepresentativeID int64
}

// RecordingQuery は録画一覧・単体取得・シリーズ棚の読み取り契約。
// 各メソッドは一つの読み取りクエリから結果を返す。
type RecordingQuery interface {
	// List はフィルタとキーセットページングを適用した録画行を返す。
	List(ctx context.Context, filter RecordingFilter) ([]RecordingQueryRow, error)
	// Get は purged されていない録画行を返し、対象が無ければ ErrNotFound を返す。
	Get(ctx context.Context, recordingID int64) (RecordingQueryRow, error)
	// Shelves は生きている録画を実効シリーズごとに集計する。
	// フィルタは一覧と共有する検索軸（status を含む）に適用する。
	// trash・seriesOf・encodeState・ページング・続きから見る条件は使わない。
	Shelves(ctx context.Context, filter RecordingFilter) ([]RecordingShelf, error)
}
