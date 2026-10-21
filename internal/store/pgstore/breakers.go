package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Breakers はサーキットブレーカーを PostgreSQL で操作する。
type Breakers struct {
	pool *pgxpool.Pool
}

// NewBreakers は PostgreSQL を使うサーキットブレーカー store を作る。
func NewBreakers(pool *pgxpool.Pool) *Breakers {
	return &Breakers{pool: pool}
}

// Trip は発動を記録し、既に発動中なら pending と detail を更新する。発動時刻を返す。
func (s *Breakers) Trip(ctx context.Context, site, name string, threshold int, sample store.BreakerSample) (time.Time, error) {
	trippedAt, err := tripCircuitBreaker(ctx, sqlcgen.New(s.pool), site, name, threshold, sample)
	if err != nil {
		return time.Time{}, err
	}
	return trippedAt, nil
}

type circuitBreakerTripQuerier interface {
	TripCircuitBreaker(context.Context, sqlcgen.TripCircuitBreakerParams) (sqlcgen.CircuitBreaker, error)
}

func tripCircuitBreaker(ctx context.Context, q circuitBreakerTripQuerier, site, name string, threshold int, sample store.BreakerSample) (time.Time, error) {
	detail, err := json.Marshal(store.TruncateBreakerSample(sample))
	if err != nil {
		return time.Time{}, fmt.Errorf("marshalling circuit breaker sample: %w", err)
	}
	row, err := q.TripCircuitBreaker(ctx, sqlcgen.TripCircuitBreakerParams{
		Site: site, Name: name, Pending: int32(sample.Total), Threshold: int32(threshold), Detail: detail,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("tripping circuit breaker %q: %w", name, err)
	}
	return row.TrippedAt, nil
}

// IsTripped は指定したブレーカーが発動中かを返す。
func (s *Breakers) IsTripped(ctx context.Context, site, name string) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{Site: site, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting circuit breaker %q: %w", name, err)
	}
	return true, nil
}

// List は発動中のサーキットブレーカーを site、name 順で返す。
func (s *Breakers) List(ctx context.Context) ([]store.CircuitBreaker, error) {
	rows, err := sqlcgen.New(s.pool).ListCircuitBreakers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing circuit breakers: %w", err)
	}
	result := make([]store.CircuitBreaker, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.CircuitBreaker{
			Site: row.Site, Name: row.Name, TrippedAt: row.TrippedAt,
			Pending: int(row.Pending), Threshold: int(row.Threshold), Detail: row.Detail,
		})
	}
	return result, nil
}

// Resume は発動行を削除し、行が無ければ store.ErrNotFound を返す。
func (s *Breakers) Resume(ctx context.Context, site, name string) error {
	rows, err := sqlcgen.New(s.pool).ResumeCircuitBreaker(ctx, sqlcgen.ResumeCircuitBreakerParams{Site: site, Name: name})
	if err != nil {
		return fmt.Errorf("resuming circuit breaker %q: %w", name, err)
	}
	if rows == 0 {
		return fmt.Errorf("circuit breaker %q at site %q: %w", name, site, store.ErrNotFound)
	}
	return nil
}

var _ store.Breakers = (*Breakers)(nil)
var _ store.BreakerAdmin = (*Breakers)(nil)
