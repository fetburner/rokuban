package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.Catalog = (*Catalog)(nil)

// Catalog は catalog の export / restore を PostgreSQL で実装する。
type Catalog struct {
	pool *pgxpool.Pool
}

// NewCatalog は PostgreSQL を使う Catalog を作る。
func NewCatalog(pool *pgxpool.Pool) *Catalog {
	return &Catalog{pool: pool}
}
