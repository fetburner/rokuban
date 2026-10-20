package epgimport

import (
	"time"
)

// msToTimePtr は UnixtimeMS のポインタを *time.Time に変換する。nil はそのまま。
func msToTimePtr(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := time.UnixMilli(*ms)
	return &t
}
