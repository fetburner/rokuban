package dbtest

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// SetMediaAssetCuts は派生資産 assetID の凍結した keep 区間（media_asset_cuts）を keep に置き換える。既存の行は消してから入れる。keep は ms の半開区間で、空の区間は捨てる。すべて空ならテストを失敗させる。
func SetMediaAssetCuts(t testing.TB, db sqlcgen.DBTX, assetID int64, keep []chapters.Range) {
	t.Helper()
	ranges, err := keepRangesParam(keep)
	if err != nil {
		t.Fatalf("encoding keep ranges for media asset %d: %v", assetID, err)
	}
	q := sqlcgen.New(db)
	if err := q.DeleteMediaAssetCuts(t.Context(), assetID); err != nil {
		t.Fatalf("deleting cuts of media asset %d: %v", assetID, err)
	}
	if err := q.InsertMediaAssetCuts(t.Context(), sqlcgen.InsertMediaAssetCutsParams{
		MediaAssetID: assetID, KeepRanges: ranges,
	}); err != nil {
		t.Fatalf("inserting cuts of media asset %d: %v", assetID, err)
	}
}

// keepRangesParam は keep を int8multirange の引数に変える。store の KeepRangesParam と同じ変換。
// dbtest は pgstore を import できない（pgstore の同一パッケージのテストが dbtest を import するため）ので、ここに写す。
func keepRangesParam(keep []chapters.Range) (pgtype.Multirange[pgtype.Range[pgtype.Int8]], error) {
	out := make(pgtype.Multirange[pgtype.Range[pgtype.Int8]], 0, len(keep))
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
