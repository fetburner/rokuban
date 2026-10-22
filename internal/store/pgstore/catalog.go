package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.Catalog = (*Catalog)(nil)

// Catalog は catalog の export / restore を PostgreSQL で実装する。
type Catalog struct {
	pool      *pgxpool.Pool
	siteNames []string
}

// NewCatalog は PostgreSQL を使う Catalog を作る。siteNames は構成の全 site で、
// Restore が全 site の ruler 入力の版を進める。空の一覧は呼び出し側の配線の誤りなので
// panic する（黙って一部 site を仮定すると、古い評価がコミットされる）。
func NewCatalog(pool *pgxpool.Pool, siteNames []string) *Catalog {
	if len(siteNames) == 0 {
		panic("pgstore.NewCatalog: siteNames must not be empty")
	}
	return &Catalog{pool: pool, siteNames: append([]string(nil), siteNames...)}
}
