package ruler_test

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

func newTestRuler(sites []string, pool *pgxpool.Pool, cfg *ruler.Config) *ruler.Ruler {
	return ruler.New(sites, pool, cfg, pgstore.NewEPG(pool))
}
