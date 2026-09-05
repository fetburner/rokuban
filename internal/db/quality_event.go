package db

import (
	"encoding/json"
	"time"
)

// QualityEvent は recordings.quality_events の配列要素。
type QualityEvent struct {
	At     time.Time       `json:"at"`
	Event  string          `json:"event"`
	Reason json.RawMessage `json:"reason"`
}
