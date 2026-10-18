package store

import (
	"context"
	"encoding/json"
	"time"
)

// OverrideField identifies a reservation option whose override should be removed.
type OverrideField string

const (
	// OverridePriority removes the priority override.
	OverridePriority OverrideField = "priority"
	// OverrideContentPath removes the contentPath override.
	OverrideContentPath OverrideField = "contentPath"
	// OverrideFilenameTemplate removes the filenameTemplate override.
	OverrideFilenameTemplate OverrideField = "filenameTemplate"
	// OverrideKeepOriginal removes the keepOriginal override.
	OverrideKeepOriginal OverrideField = "keepOriginal"
	// OverrideEncodeProfiles removes the encodeProfiles override.
	OverrideEncodeProfiles OverrideField = "encodeProfiles"
)

// OverridesPatch applies selected reservation option overrides and resets.
// A nil field in the value section means that field is unchanged.
type OverridesPatch struct {
	Priority         *int
	ContentPath      *string
	FilenameTemplate *string
	KeepOriginal     *string
	EncodeProfiles   *[]string
	Reset            []OverrideField
}

// ReservationRecord is the persisted portion of a reservation row.
type ReservationRecord struct {
	Site                  string
	ProgramID             int64
	RuleID                *int64
	Base                  json.RawMessage
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DedupMatchRecordingID *int64
	DedupSimilarity       *float32
}

// ProgramSnapshot is the persisted program data used by reservation reads.
type ProgramSnapshot struct {
	Title       string
	StartAt     time.Time
	DurationMs  int64
	ChannelType string
	ServiceName string
}

// ReservationDetails joins a reservation with its snapshot and user inputs.
type ReservationDetails struct {
	Reservation   ReservationRecord
	Snapshot      ProgramSnapshot
	Overrides     json.RawMessage
	IntentAction  *string
	NeverRecorded bool
	Series        *string
}

// ReservationOverlap is a reservation candidate returned for overlap checks.
type ReservationOverlap struct {
	ProgramID    int64
	Title        string
	StartAt      time.Time
	DurationMs   int64
	Base         json.RawMessage
	Overrides    json.RawMessage
	IntentAction *string
}

// Reservations persists user recording intent and overrides, and reads reservation views.
// Each write method owns and commits its transaction before returning. PutIntent and
// PatchOverrides serialize writes for the same (site, programID); PatchOverrides reads the
// locked prior value before merging so concurrent field patches preserve one another.
type Reservations interface {
	// PutIntent snapshots the current EPG program and writes its record/skip intent atomically.
	// It returns ErrNotFound when neither the EPG projection nor a frozen snapshot exists.
	PutIntent(ctx context.Context, site string, programID int64, action string) error
	// DeleteIntent removes the intent for a program. A missing intent is success.
	DeleteIntent(ctx context.Context, site string, programID int64) error
	// PatchOverrides merges a field patch with the locked prior overrides and the reservation base.
	// An empty result removes the overrides row; an invalid effective option returns a typed error.
	PatchOverrides(ctx context.Context, site string, programID int64, patch OverridesPatch) error
	// DeleteOverrides removes all overrides for a program. A missing row is success.
	DeleteOverrides(ctx context.Context, site string, programID int64) error
	// GetReservation returns a detailed reservation by its stable (site, programID) key.
	GetReservation(ctx context.Context, site string, programID int64) (ReservationDetails, error)
	// ListReservations returns detailed reservations ordered by site and program start time.
	ListReservations(ctx context.Context) ([]ReservationDetails, error)
	// ListOverlaps returns non-skipped reservations overlapping the EPG program's broadcast window.
	ListOverlaps(ctx context.Context, site string, programID int64) ([]ReservationOverlap, error)
}
