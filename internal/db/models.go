package db

import (
	"encoding/json"
	"time"
)

// Reservation は予約（desired state）。
type Reservation struct {
	ID                int64           `db:"id"`
	Site              string          `db:"site"`
	ProgramID         int64           `db:"program_id"`
	Source            string          `db:"source"`
	RuleID            *int64          `db:"rule_id"`
	State             string          `db:"state"`
	Base              json.RawMessage `db:"base"`
	Overrides         json.RawMessage `db:"overrides"`
	Title             string          `db:"title"`
	ProgramStartAt    time.Time       `db:"program_start_at"`
	ProgramDurationMs int64           `db:"program_duration_ms"`
	CreatedAt         time.Time       `db:"created_at"`
	UpdatedAt         time.Time       `db:"updated_at"`
}

// ReservationOptions は reservations.base / overrides の jsonb 構造。
// jsonb 内は camelCase（Go/JSON 規約）。
type ReservationOptions struct {
	Skip           *bool    `json:"skip,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
	ContentPath    *string  `json:"contentPath,omitempty"`
	EncodeProfiles []string `json:"encodeProfiles,omitempty"`
	KeepOriginal   *string  `json:"keepOriginal,omitempty"`
}

// Effective は base に overrides をマージした結果を返す。
func (o *ReservationOptions) Effective(base *ReservationOptions) ReservationOptions {
	if base == nil {
		if o == nil {
			return ReservationOptions{}
		}
		return *o
	}
	eff := *base
	if o == nil {
		return eff
	}
	if o.Skip != nil {
		eff.Skip = o.Skip
	}
	if o.Priority != nil {
		eff.Priority = o.Priority
	}
	if o.ContentPath != nil {
		eff.ContentPath = o.ContentPath
	}
	if o.EncodeProfiles != nil {
		eff.EncodeProfiles = o.EncodeProfiles
	}
	if o.KeepOriginal != nil {
		eff.KeepOriginal = o.KeepOriginal
	}
	return eff
}

// ScheduleSync は mirakc schedule の観測。
type ScheduleSync struct {
	Site          string          `db:"site"`
	ProgramID     int64           `db:"program_id"`
	ReservationID *int64          `db:"reservation_id"`
	State         string          `db:"state"`
	Options       json.RawMessage `db:"options"`
	Tags          []string        `db:"tags"`
	FailedReason  json.RawMessage `db:"failed_reason"`
	ObservedAt    time.Time       `db:"observed_at"`
}

// Recording は録画履歴（永続資産）。
type Recording struct {
	ID                int64           `db:"id"`
	ReservationID     *int64          `db:"reservation_id"`
	RuleID            *int64          `db:"rule_id"`
	Source            string          `db:"source"`
	Site              string          `db:"site"`
	NetworkID         int             `db:"network_id"`
	ServiceID         int             `db:"service_id"`
	EventID           int             `db:"event_id"`
	ServiceName       string          `db:"service_name"`
	ChannelType       string          `db:"channel_type"`
	Channel           string          `db:"channel"`
	Title             string          `db:"title"`
	Description       *string         `db:"description"`
	Extended          json.RawMessage `db:"extended"`
	Genres            json.RawMessage `db:"genres"`
	IsFree            bool            `db:"is_free"`
	ProgramStartAt    time.Time       `db:"program_start_at"`
	ProgramDurationMs int64           `db:"program_duration_ms"`
	Status            string          `db:"status"`
	StartedAt         *time.Time      `db:"started_at"`
	EndedAt           *time.Time      `db:"ended_at"`
	KeepOriginal      string          `db:"keep_original"`
	EncodeProfiles    []string        `db:"encode_profiles"`
	QualityEvents     json.RawMessage `db:"quality_events"`
	DeletedAt         *time.Time      `db:"deleted_at"`
	CreatedAt         time.Time       `db:"created_at"`
	UpdatedAt         time.Time       `db:"updated_at"`
}

// RecordSync は mirakc record の観測。
type RecordSync struct {
	Site          string          `db:"site"`
	RecordID      string          `db:"record_id"`
	RecordingID   *int64          `db:"recording_id"`
	ProgramID     int64           `db:"program_id"`
	Status        string          `db:"status"`
	ContentPath   *string         `db:"content_path"`
	ContentLength *int64          `db:"content_length"`
	Tags          []string        `db:"tags"`
	FailedReason  json.RawMessage `db:"failed_reason"`
	ObservedAt    time.Time       `db:"observed_at"`
}

// MediaAsset はメディアアセット（永続資産）。
type MediaAsset struct {
	ID          int64      `db:"id"`
	RecordingID int64      `db:"recording_id"`
	Kind        string     `db:"kind"`
	Profile     *string    `db:"profile"`
	RelPath     string     `db:"rel_path"`
	SizeBytes   int64      `db:"size_bytes"`
	State       string     `db:"state"`
	DeletedAt   *time.Time `db:"deleted_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// DropStat は PID 別ドロップ統計。
type DropStat struct {
	MediaAssetID int64 `db:"media_asset_id"`
	PID          int   `db:"pid"`
	Packets      int64 `db:"packets"`
	Drops        int64 `db:"drops"`
	Errors       int64 `db:"errors"`
	Scrambled    int64 `db:"scrambled"`
}

// QualityEvent は recordings.quality_events の配列要素。
type QualityEvent struct {
	At     time.Time       `json:"at"`
	Event  string          `json:"event"`
	Reason json.RawMessage `json:"reason"`
}

// 定数

const (
	SourceRule   = "rule"
	SourceManual = "manual"

	ReservationStateActive   = "active"
	ReservationStateDetached = "detached"
	ReservationStateOrphaned = "orphaned"

	RecordingStatusRecording = "recording"
	RecordingStatusFinished  = "finished"
	RecordingStatusFailed    = "failed"

	AssetKindOriginal  = "original"
	AssetKindEncoded   = "encoded"
	AssetKindThumbnail = "thumbnail"

	AssetStateActive   = "active"
	AssetStateDeleting = "deleting"
	AssetStateDeleted  = "deleted"

	KeepOriginalAlways       = "always"
	KeepOriginalUntilEncoded = "until_encoded"
)
