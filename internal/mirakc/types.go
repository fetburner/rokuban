package mirakc

import (
	"encoding/json"
	"time"
)

// Milliseconds は mirakc の UNIX time (ミリ秒) を time.Time に変換する。
type Milliseconds time.Time

func (m *Milliseconds) UnmarshalJSON(b []byte) error {
	var ms int64
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*m = Milliseconds(time.UnixMilli(ms))
	return nil
}

func (m Milliseconds) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(m).UnixMilli())
}

func (m Milliseconds) Time() time.Time {
	return time.Time(m)
}

// Version は GET /api/version のレスポンス。
type Version struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
}

// Schedule は GET /api/recording/schedules の要素。
type Schedule struct {
	State        string        `json:"state"`
	Program      Program       `json:"program"`
	Options      Options       `json:"options"`
	Tags         []string      `json:"tags"`
	FailedReason *FailedReason `json:"failedReason,omitempty"`
}

// ScheduleInput は POST /api/recording/schedules のリクエストボディ。
type ScheduleInput struct {
	ProgramID int64    `json:"programId"`
	Options   Options  `json:"options"`
	Tags      []string `json:"tags,omitempty"`
}

// Options は recording の設定。mirakc の RecordingOptions に対応。
type Options struct {
	ContentPath *string  `json:"contentPath,omitempty"`
	Priority    int      `json:"priority"`
	PreFilters  []string `json:"preFilters,omitempty"`
	PostFilters []string `json:"postFilters,omitempty"`
	LogFilter   *string  `json:"logFilter,omitempty"`
}

// Record は GET /api/recording/records の要素。
type Record struct {
	ID        string      `json:"id"`
	Program   Program     `json:"program"`
	Service   Service     `json:"service"`
	Tags      []string    `json:"tags"`
	Recording RecordInfo  `json:"recording"`
	Content   ContentInfo `json:"content"`
}

// RecordInfo は録画実行の情報。
type RecordInfo struct {
	Options      Options       `json:"options"`
	Status       string        `json:"status"`
	StartTime    Milliseconds  `json:"startTime"`
	EndTime      *Milliseconds `json:"endTime,omitempty"`
	Duration     *int64        `json:"duration,omitempty"`
	FailedReason *FailedReason `json:"failedReason,omitempty"`
}

// ContentInfo は録画コンテンツの情報。
type ContentInfo struct {
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Length *uint64 `json:"length,omitempty"`
}

// FailedReason は録画失敗の理由。discriminated union (type フィールド)。
type FailedReason struct {
	Type     string  `json:"type"`
	Message  *string `json:"message,omitempty"`
	OSError  *int    `json:"osError,omitempty"`
	ExitCode *int    `json:"exitCode,omitempty"`
}

// Program は mirakc の MirakurunProgram 互換。
type Program struct {
	ID          int64             `json:"id"`
	EventID     int               `json:"eventId"`
	ServiceID   int               `json:"serviceId"`
	NetworkID   int               `json:"networkId"`
	StartAt     *Milliseconds     `json:"startAt"`
	Duration    *int64            `json:"duration"`
	IsFree      bool              `json:"isFree"`
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	Extended    map[string]string `json:"extended,omitempty"`
	Genres      []Genre           `json:"genres,omitempty"`
}

// Genre は番組ジャンル。
type Genre struct {
	LV1 int `json:"lv1"`
	LV2 int `json:"lv2"`
	UN1 int `json:"un1"`
	UN2 int `json:"un2"`
}

// Service は mirakc の MirakurunService 互換。
type Service struct {
	ID                 int64          `json:"id"`
	ServiceID          int            `json:"serviceId"`
	NetworkID          int            `json:"networkId"`
	Type               int            `json:"type"`
	LogoID             int            `json:"logoId"`
	RemoteControlKeyID int            `json:"remoteControlKeyId"`
	Name               string         `json:"name"`
	Channel            ServiceChannel `json:"channel"`
	HasLogoData        bool           `json:"hasLogoData"`
}

// ServiceChannel はサービスのチャンネル情報。
type ServiceChannel struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
}

// RecordRemovalResult は DELETE /api/recording/records/{id} のレスポンス。
type RecordRemovalResult struct {
	RecordRemoved  bool `json:"recordRemoved"`
	ContentRemoved bool `json:"contentRemoved"`
}
