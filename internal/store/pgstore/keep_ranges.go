package pgstore

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/chapters"
)

// KeepRanges is the PostgreSQL representation of frozen keep intervals.
type KeepRanges = pgtype.Multirange[pgtype.Range[pgtype.Int8]]

// KeepRangesParam maps half-open millisecond ranges to an int8multirange parameter.
func KeepRangesParam(keep []chapters.Range) (KeepRanges, error) {
	out := make(KeepRanges, 0, len(keep))
	for _, r := range keep {
		if r.EndMs <= r.StartMs {
			continue
		}
		out = append(out, pgtype.Range[pgtype.Int8]{
			Lower:     pgtype.Int8{Int64: r.StartMs, Valid: true},
			Upper:     pgtype.Int8{Int64: r.EndMs, Valid: true},
			LowerType: pgtype.Inclusive,
			UpperType: pgtype.Exclusive,
			Valid:     true,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("keep ranges are empty")
	}
	return out, nil
}

// KeepRangesFromMultirange maps a database int8multirange to finite half-open millisecond ranges.
func KeepRangesFromMultirange(mr KeepRanges) ([]chapters.Range, error) {
	out := make([]chapters.Range, 0, len(mr))
	for _, r := range mr {
		if !r.Valid || !r.Lower.Valid || !r.Upper.Valid ||
			r.LowerType != pgtype.Inclusive || r.UpperType != pgtype.Exclusive {
			return nil, fmt.Errorf("keep range %+v is not a finite [lower, upper) range", r)
		}
		out = append(out, chapters.Range{StartMs: r.Lower.Int64, EndMs: r.Upper.Int64})
	}
	return out, nil
}
