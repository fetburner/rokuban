package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ManualReservationParams は ManualReservation の上書き対象。program_snapshots の行を先に作っておく必要がある。
type ManualReservationParams struct {
	Site string
}

// ManualReservation は番組 programID の手動予約を作る。既定値は site = "default"。opts で既定値を上書きする。
func ManualReservation(t testing.TB, db sqlcgen.DBTX, programID int64, opts ...func(*ManualReservationParams)) {
	t.Helper()
	params := ManualReservationParams{Site: "default"}
	for _, opt := range opts {
		opt(&params)
	}
	if _, err := sqlcgen.New(db).CreateManualReservation(t.Context(), sqlcgen.CreateManualReservationParams{
		Site: params.Site, ProgramID: programID,
	}); err != nil {
		t.Fatalf("creating manual reservation for program %d at site %q: %v", programID, params.Site, err)
	}
}
