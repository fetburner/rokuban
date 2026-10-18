package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fetburner/rokuban/internal/rulequery"
)

// EPGService は番組表 API が読むサービス射影である。
type EPGService struct {
	Site               string
	NetworkID          int32
	ServiceID          int32
	Name               string
	ChannelType        string
	Channel            string
	RemoteControlKeyID int32
	HasLogoData        bool
	HasPrograms        bool
}

// EPGServiceUpsert は EPG 同期が書くサービス射影の値である。
type EPGServiceUpsert struct {
	Site               string
	NetworkID          int32
	ServiceID          int32
	Type               int32
	LogoID             int32
	RemoteControlKeyID int32
	Name               string
	ChannelType        string
	Channel            string
	HasLogoData        bool
}

// EPGProgramUpsert は EPG 同期が書く番組射影の値である。
type EPGProgramUpsert struct {
	Site        string
	ProgramID   int64
	NetworkID   int32
	ServiceID   int32
	EventID     int32
	StartAt     time.Time
	DurationMs  int64
	EndAt       time.Time
	IsFree      bool
	Name        string
	Description string
	Extended    json.RawMessage
	Genres      json.RawMessage
	Video       json.RawMessage
	Audios      json.RawMessage
}

// EPGServiceRef はサービスの NID/SID の組である。
type EPGServiceRef struct {
	NetworkID int32
	ServiceID int32
}

// EPGProgramListQuery は番組表一覧の絞り込み条件である。
type EPGProgramListQuery struct {
	Site        string
	WindowStart time.Time
	WindowEnd   time.Time
	Services    []EPGServiceRef
}

// EPGProgram は番組表の詳細 API が読む番組射影である。
type EPGProgram struct {
	Site        string
	ProgramID   int64
	NetworkID   int32
	ServiceID   int32
	EventID     int32
	StartAt     time.Time
	DurationMs  int64
	EndAt       time.Time
	IsFree      bool
	Name        string
	Description string
	Extended    json.RawMessage
	Genres      json.RawMessage
	Video       json.RawMessage
	Audios      json.RawMessage
	GenreLv1    []int16
}

// EPGProgramDetail は番組詳細と、その番組に対するユーザー意図を返す。
type EPGProgramDetail struct {
	Program      EPGProgram
	IntentAction *string
}

// EPGProgramListItem は番組表一覧と予約・意図の射影を返す。
type EPGProgramListItem struct {
	Site         string
	ProgramID    int64
	NetworkID    int32
	ServiceID    int32
	EventID      int32
	StartAt      time.Time
	DurationMs   int64
	EndAt        time.Time
	IsFree       bool
	Name         string
	Description  string
	GenreLv1     []int16
	IntentAction *string
	RecordingID  *int64
}

// ProgramMatch は検索またはシリーズから返る番組の表示用射影である。
type ProgramMatch struct {
	Site       string
	ProgramID  int64
	NetworkID  int32
	ServiceID  int32
	StartAt    time.Time
	DurationMs int64
	Name       string
	IsFree     bool
}

// EPGSync は mirakc から取得した EPG 射影を同期する。
//
// 各メソッドは 1 つの同期ステップだけを実行し、複数ステップをまとめた tx を作らない。
type EPGSync interface {
	// SweepMark は同期開始時点の DB 時刻を返す。
	SweepMark(ctx context.Context) (time.Time, error)
	// UpsertServices は 1 バッチ分のサービス射影を書き込む。
	UpsertServices(ctx context.Context, services []EPGServiceUpsert) error
	// DeleteStaleServices は mark より古い site のサービス射影を削除する。
	DeleteStaleServices(ctx context.Context, site string, mark time.Time) (int64, error)
	// UpsertPrograms は 1 バッチ分の番組射影を書き込む。
	UpsertPrograms(ctx context.Context, programs []EPGProgramUpsert) error
	// DeleteStalePrograms は指定 network のサービス群で mark より古い番組射影を削除する。
	DeleteStalePrograms(ctx context.Context, site string, mark time.Time, networkID int32, serviceIDs []int32) (int64, error)
	// PrunePrograms は endAt より前に終了した番組射影を削除する。
	PrunePrograms(ctx context.Context, site string, endAt time.Time) (int64, error)
	// NotifyProgramsUpdated は番組表更新を購読者へ通知する。
	NotifyProgramsUpdated(ctx context.Context) error
}

// EPGReader は番組表 API が読む射影を返す。
type EPGReader interface {
	// ListServices は site のサービス一覧を返す。
	ListServices(ctx context.Context, site string) ([]EPGService, error)
	// ListPrograms は時間窓と任意のサービス集合に一致する番組一覧を返す。
	ListPrograms(ctx context.Context, query EPGProgramListQuery) ([]EPGProgramListItem, error)
	// GetProgram は site の programID を詳細形で返す。
	GetProgram(ctx context.Context, site string, programID int64) (EPGProgramDetail, error)
	// ListUpcomingProgramsBySeries は録画と同じ実効シリーズに属する今後の番組を返す。
	ListUpcomingProgramsBySeries(ctx context.Context, recordingID int64) ([]ProgramMatch, error)
}

// ProgramSearch は番組検索と ruler のルール評価を提供する。
type ProgramSearch interface {
	// SearchPrograms は conditions に一致した番組を返す。
	SearchPrograms(ctx context.Context, conditions rulequery.Conditions) ([]ProgramMatch, error)
	// MatchProgramIDsForRule は 1 つの site で ruleID に一致する programID を返す。
	MatchProgramIDsForRule(ctx context.Context, site string, ruleID int64) ([]int64, error)
	// ValidateRegex はこの実装が受け付ける正規表現として pattern を検証する。
	ValidateRegex(ctx context.Context, pattern string) error
}
