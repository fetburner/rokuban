package db

import "github.com/jackc/pgx/v5"

// Tx is the transaction contract used by job queue adapters. It wraps pgx.Tx so callers
// depend on Rokuban's database package rather than the queue library's transaction type.
type Tx interface {
	pgx.Tx
}
