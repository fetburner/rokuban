package epgimport

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// isNoRows は pgx.ErrNoRows のラップを剥がして判定する。
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// msToTimePtr は UnixtimeMS のポインタを *time.Time に変換する。nil はそのまま。
func msToTimePtr(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := time.UnixMilli(*ms)
	return &t
}
