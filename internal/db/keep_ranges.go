package db

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/chapters"
)

// KeepRanges は keep 区間を int8multirange として読み書きするときの Go の型。
type KeepRanges = pgtype.Multirange[pgtype.Range[pgtype.Int8]]

// KeepRangesParam は ms の半開区間列を int8multirange の param へ写す。
// 空の区間は捨てる。並び替えと隣接区間の結合は PostgreSQL の multirange が値として
// 正規化する（値どうしの一致比較ができる）。カット版の凍結区間（media_asset_cuts）と
// 作り直し要求（encoded_asset_rebuild_requests。書き込むのは api だけ）の書き込みが共有する。
// 残る区間が無ければエラーを返す（どちらの表も CHECK で空を拒否するので先に落とす）。
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
			// Valid を立てないと pgx が「NULL の要素」として符号化を拒否する
			// （multirange cannot contain NULL element）。
			Valid: true,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("keep ranges are empty")
	}
	return out, nil
}

// KeepRangesFromMultirange は KeepRangesParam の逆向きで、DB から読んだ
// int8multirange を ms の半開区間列へ戻す。PostgreSQL は int8range を [) に
// 正規化して昇順に並べて返すので、そのまま chapters.Range の列になる。
// 有限の [) 以外の区間（上下限の欠けた区間など）はエラーにする。
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
