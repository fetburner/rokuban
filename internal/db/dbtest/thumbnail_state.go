package dbtest

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ThumbnailRow は ThumbnailState が返す、active なサムネイルの論理行と位置。
type ThumbnailRow struct {
	ID                int64
	RelPath           string
	SeekMs            *int64
	DerivedGeneration int64
}

// ThumbnailState は録画 recordingID の、置き場 storeName で active なサムネイルの論理行と、その現在の位置（seek_ms）を返す。行が無ければ found = false を返す。
// サムネイルの差し替えが commit の前に読む行と同じ読み取りで、判断はしない。位置は衛星表から読むので、位置の無い旧サムネイルでは SeekMs が nil になる。
func ThumbnailState(t testing.TB, db sqlcgen.DBTX, recordingID int64, storeName string) (row ThumbnailRow, found bool) {
	t.Helper()
	got, err := sqlcgen.New(db).LockActiveThumbnailMediaAsset(t.Context(), sqlcgen.LockActiveThumbnailMediaAssetParams{
		RecordingID: recordingID, StoreName: storeName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ThumbnailRow{}, false
	}
	if err != nil {
		t.Fatalf("reading active thumbnail of recording %d in store %q: %v", recordingID, storeName, err)
	}
	return ThumbnailRow{ID: got.ID, RelPath: got.RelPath, SeekMs: got.SeekMs, DerivedGeneration: got.DerivedGeneration}, true
}
