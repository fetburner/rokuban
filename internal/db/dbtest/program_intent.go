package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramIntentParams は ProgramIntent の上書き対象。program_snapshots の行を先に作っておく必要がある。
type ProgramIntentParams struct {
	Site   string
	Action string
}

// ProgramIntent は番組 programID の録画意図を作る。既定値は site = "default"、action = "record"。opts で既定値を上書きする。
func ProgramIntent(t testing.TB, db sqlcgen.DBTX, programID int64, opts ...func(*ProgramIntentParams)) {
	t.Helper()
	params := ProgramIntentParams{Site: "default", Action: "record"}
	for _, opt := range opts {
		opt(&params)
	}
	if _, err := sqlcgen.New(db).UpsertProgramIntent(t.Context(), sqlcgen.UpsertProgramIntentParams{
		Site: params.Site, ProgramID: programID, Action: params.Action,
	}); err != nil {
		t.Fatalf("upserting program intent for program %d at site %q: %v", programID, params.Site, err)
	}
}
