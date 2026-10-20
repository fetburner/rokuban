package store

import (
	"context"
	"encoding/json"
	"time"
)

// BreakerSample はブレーカー発動時の確認用データである。
type BreakerSample struct {
	// Total は止めた削除の総数。
	Total int `json:"total"`
	// Programs は先頭いくつかの抜粋（全部は入れない。件数が多いときこそ発動する）。
	Programs []BreakerSampleProgram `json:"programs,omitempty"`
}

// BreakerSampleProgram は確認用抜粋の 1 件。
type BreakerSampleProgram struct {
	ProgramID int64  `json:"programId"`
	Title     string `json:"title,omitempty"`
}

// CircuitBreaker は発動中のサーキットブレーカーを表す。
type CircuitBreaker struct {
	Site      string
	Name      string
	TrippedAt time.Time
	Pending   int
	Threshold int
	Detail    json.RawMessage
}

// Breakers は worker がサーキットブレーカーを発動・観測する契約である。
type Breakers interface {
	// Trip は発動を記録し、すでに発動中なら pending と detail を更新する。発動時刻（tripped_at）を返す。
	Trip(ctx context.Context, site, name string, threshold int, sample BreakerSample) (time.Time, error)
	// IsTripped は指定したブレーカーが発動中かを返す。
	IsTripped(ctx context.Context, site, name string) (bool, error)
}

// BreakerAdmin は API が発動中一覧と手動再開を行う契約である。
type BreakerAdmin interface {
	List(ctx context.Context) ([]CircuitBreaker, error)
	// Resume は発動行を削除し、行が無ければ ErrNotFound を返す。
	Resume(ctx context.Context, site, name string) error
}
