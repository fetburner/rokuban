package dbtest

import (
	"encoding/json"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramOverridesParams は ProgramOverrides の上書き対象。program_snapshots の行を先に作っておく必要がある。
type ProgramOverridesParams struct {
	Site      string
	Overrides json.RawMessage
}

// ProgramOverrides は番組 programID のパラメータ上書きを作る。既定値は site = "default"、overrides = {"priority":10}。opts で既定値を上書きする。
func ProgramOverrides(t testing.TB, db sqlcgen.DBTX, programID int64, opts ...func(*ProgramOverridesParams)) {
	t.Helper()
	params := ProgramOverridesParams{
		Site:      "default",
		Overrides: json.RawMessage(`{"priority":10}`),
	}
	for _, opt := range opts {
		opt(&params)
	}
	if _, err := sqlcgen.New(db).UpsertProgramOverrides(t.Context(), sqlcgen.UpsertProgramOverridesParams{
		Site: params.Site, ProgramID: programID, Overrides: params.Overrides,
	}); err != nil {
		t.Fatalf("upserting program overrides for program %d at site %q: %v", programID, params.Site, err)
	}
}
