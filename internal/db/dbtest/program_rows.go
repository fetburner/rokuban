package dbtest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramIntentRow は program_intents の 1 行のうち、テストが検査する列である。
type ProgramIntentRow struct {
	Action    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReadProgramIntent は (site, programID) の録画意図を読む。行が無ければ ok = false を返す。
// 契約の読み取りは作成・更新時刻を返さないので、時刻を検査するテストがこれを使う。
func ReadProgramIntent(t testing.TB, db sqlcgen.DBTX, site string, programID int64) (row ProgramIntentRow, ok bool) {
	t.Helper()
	got, err := sqlcgen.New(db).GetProgramIntent(t.Context(), sqlcgen.GetProgramIntentParams{Site: site, ProgramID: programID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProgramIntentRow{}, false
	}
	if err != nil {
		t.Fatalf("reading program intent for program %d at site %q: %v", programID, site, err)
	}
	return ProgramIntentRow{Action: got.Action, CreatedAt: got.CreatedAt, UpdatedAt: got.UpdatedAt}, true
}

// ProgramOverridesRow は program_overrides の 1 行のうち、テストが検査する列である。
type ProgramOverridesRow struct {
	Overrides json.RawMessage
	UpdatedAt time.Time
}

// ReadProgramOverrides は (site, programID) の上書きを読む。行が無ければ ok = false を返す。
func ReadProgramOverrides(t testing.TB, db sqlcgen.DBTX, site string, programID int64) (row ProgramOverridesRow, ok bool) {
	t.Helper()
	got, err := sqlcgen.New(db).GetProgramOverrides(t.Context(), sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProgramOverridesRow{}, false
	}
	if err != nil {
		t.Fatalf("reading program overrides for program %d at site %q: %v", programID, site, err)
	}
	return ProgramOverridesRow{Overrides: got.Overrides, UpdatedAt: got.UpdatedAt}, true
}

// ProgramSnapshotRow は program_snapshots の 1 行のうち、テストが検査する列である。
type ProgramSnapshotRow struct {
	StartAt time.Time
}

// ReadProgramSnapshot は (site, programID) の番組スナップショットを読む。行が無ければ ok = false を返す。
// 予約が無くなった番組のスナップショットは契約の読み取りに現れないので、これで読む。
func ReadProgramSnapshot(t testing.TB, db sqlcgen.DBTX, site string, programID int64) (row ProgramSnapshotRow, ok bool) {
	t.Helper()
	got, err := sqlcgen.New(db).GetProgramSnapshot(t.Context(), sqlcgen.GetProgramSnapshotParams{Site: site, ProgramID: programID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProgramSnapshotRow{}, false
	}
	if err != nil {
		t.Fatalf("reading program snapshot for program %d at site %q: %v", programID, site, err)
	}
	return ProgramSnapshotRow{StartAt: got.StartAt}, true
}
