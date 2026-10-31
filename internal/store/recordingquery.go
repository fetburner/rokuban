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
// ゼロ値のフィールドは「絞り込みなし」を表す。軸どうしは AND で結ぶ。
type RecordingFilter struct {
	// Trash は true のときごみ箱（削除済みで purged でない録画）を返す。
	// false のときは削除されていない録画を返す（置き換わった superseded の行も
	// 履歴として含む）。
	Trash bool

	// Q はキーワードの部分一致。全角半角と大小を正規化して比べ、
	// LIKE の特殊文字は文字どおりに扱う。空なら絞らない。
	Q string
	// QTarget は Q を当てる範囲。"title" はタイトルだけ、"titleDescription" は
	// タイトルと説明。空は "titleDescription" と同じ。
	QTarget string

	// Genres は大分類のいずれかに当たる録画に絞る（軸内 OR）。
	Genres []int16
	// ChannelTypes は放送種別（GR / BS / CS / SKY）のいずれかに絞る（軸内 OR）。
	ChannelTypes []string
	// Sites はサイトのいずれかに絞る（軸内 OR）。空なら全サイト。
	Sites []string
	// Services は (NetworkID, ServiceID) の組のいずれかに絞る（軸内 OR）。
	// site は含まない。site は Sites の別軸で、軸どうしは AND。
	Services []RecordingServiceRef

	// Status は録画の状態に一致する録画に絞る。Trash が false で "failed" のときは、
	// 置き換わった（superseded）行を除く。
	Status string
	// EncodeRecordingIDs はエンコード状態で絞った結果の録画 id 集合。
	// nil は絞らない。空スライスは該当なしを表し 0 件を返す。
	EncodeRecordingIDs *[]int64
	// Source は録画の出どころ（ルール・手動など）に一致する録画に絞る。
	Source string
	// RuleID はそのルールから作られた録画に絞る。
	RuleID *int64
	// SeriesOf は番組ハブの起点になった録画の id。起点と同じ実効シリーズで、
	// superseded でない録画に絞る。起点の実効シリーズが無い（NULL）か、
	// 起点の録画が無ければ 0 件を返す。
	SeriesOf *int64

	// From は番組開始時刻の下限（以上）。
	From *time.Time
	// To は番組開始時刻の上限（未満）。
	To *time.Time

	// SortDesc は番組開始時刻と id の並び順。true は降順（既定の用途）、
	// false は昇順。
	SortDesc bool
	// ContinueWatching は「続きから見る」用の集合を、再生位置の更新が新しい順に返す。
	// 再生位置があり、生きていて、recording または finished で、同じ放送イベントが
	// どの録画でも視聴済みでない録画が対象。
	ContinueWatching bool

	// Before と BeforeID は両方指定するキーセットカーソル。並び順でこの位置より後の
	// 録画を返す。
	Before   *time.Time
	BeforeID *int64

	// Limit は返す最大件数。
	Limit int
}

// RecordingQueryRow は録画読み取りが返す素の DB 事実と射影値を持つ。
// API 固有の状態導出は、呼び出し側がこの行から行う。
type RecordingQueryRow struct {
	ID                int64
	Site              string
	RuleID            *int64
	Source            string
	ServiceName       string
	ChannelType       string
	Channel           string
	NetworkID         int32
	ServiceID         int32
	EventID           int32
	Title             string
	Description       *string
	ProgramStartAt    time.Time
	ProgramDurationMs int64
	Status            string
	StartedAt         *time.Time
	EndedAt           *time.Time
	QualityEvents     json.RawMessage
	DeletedAt         *time.Time
	CreatedAt         time.Time
	HLSGeneration     *int64
	// HLSStore は録画 HLS が読む入力（active な原本のコピー、無ければ ingest temp）がある置き場。
	HLSStore *string
	// OriginalStore は active な原本のコピーがある置き場（複数あれば置き場名順で先頭）。無ければ nil。
	OriginalStore            *string
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
	// trash・seriesOf・EncodeRecordingIDs・ページング・続きから見る条件は使わない。
	Shelves(ctx context.Context, filter RecordingFilter) ([]RecordingShelf, error)
}
