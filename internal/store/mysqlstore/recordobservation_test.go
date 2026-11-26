package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

// TestRecordingDerivedColumnsFollowSnapshot は、録画を作る 3 つの経路が series_key と genre_lv1 を
// スナップショットから書くことを、値がある場合と無い場合の両方で検査する。
// MySQL では両方とも生成列でなくアプリが書くので、書き忘れると既定値（NULL・'[]'）のまま残る。
func TestRecordingDerivedColumnsFollowSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		title     string
		genres    json.RawMessage
		wantKey   sql.NullString
		wantGenre string
	}{
		{
			name: "title with a series key and genres", title: "ドラマＡ 第3話",
			genres:  json.RawMessage(`[{"lv1":4,"lv2":0,"un1":0,"un2":0}]`),
			wantKey: sql.NullString{String: "ドラマA", Valid: true}, wantGenre: `[4]`,
		},
		{
			name: "title without a series key and no genres", title: "【】",
			wantKey: sql.NullString{}, wantGenre: `[]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := mysqltest.SetupDB(t)
			recordings := NewRecordings(db)
			base := newContractRecordObservation("derived-"+tc.name, store.RecordingStatusFinished)
			base.Snapshot.Title = tc.title
			base.Snapshot.Genres = tc.genres

			result, err := recordings.ProcessRecord(ctx, base)
			if err != nil || result.RecordingID == nil {
				t.Fatalf("ProcessRecord = (%v, %v), want a recording", result.RecordingID, err)
			}
			requireDerivedColumns(t, db, *result.RecordingID, tc.wantKey, tc.wantGenre)

			failedEvent := base
			failedEvent.ProgramID++
			failedEvent.Snapshot.EventID++
			seedContractProgramSnapshot(t, db, failedEvent.Site, failedEvent.ProgramID, failedEvent.Snapshot)
			execFixture(t, db, `INSERT INTO reservations (site, program_id) VALUES (?, ?)`, failedEvent.Site, failedEvent.ProgramID)
			id, recorded, err := recordings.RecordFailedRecording(ctx, store.FailedRecordingObservation{
				Site: failedEvent.Site, ProgramID: failedEvent.ProgramID, Snapshot: failedEvent.Snapshot,
				Reason: []byte(`{"type":"recording-failed"}`),
			})
			if err != nil || !recorded {
				t.Fatalf("RecordFailedRecording = (%d, %t, %v), want recorded", id, recorded, err)
			}
			requireDerivedColumns(t, db, id, tc.wantKey, tc.wantGenre)

			failedSchedule := base
			failedSchedule.ProgramID += 2
			failedSchedule.Snapshot.EventID += 2
			seedContractProgramSnapshot(t, db, failedSchedule.Site, failedSchedule.ProgramID, failedSchedule.Snapshot)
			execFixture(t, db, `INSERT INTO reservations (site, program_id) VALUES (?, ?)`, failedSchedule.Site, failedSchedule.ProgramID)
			scheduled, err := recordings.ProcessFailedSchedule(ctx, store.FailedScheduleObservation{
				Site: failedSchedule.Site, ProgramID: failedSchedule.ProgramID, Snapshot: failedSchedule.Snapshot,
				Reason: []byte(`{"type":"start-recording-failed"}`),
			})
			if err != nil || scheduled.RecordingID == nil {
				t.Fatalf("ProcessFailedSchedule = (%v, %v), want a recording", scheduled.RecordingID, err)
			}
			requireDerivedColumns(t, db, *scheduled.RecordingID, tc.wantKey, tc.wantGenre)
		})
	}
}

// requireDerivedColumns は録画行の series_key と genre_lv1 を読み、期待値と照合する。
func requireDerivedColumns(t *testing.T, db *sql.DB, recordingID int64, wantKey sql.NullString, wantGenre string) {
	t.Helper()
	var key sql.NullString
	var genre string
	if err := db.QueryRowContext(context.Background(),
		`SELECT series_key, CAST(genre_lv1 AS CHAR) FROM recordings WHERE id = ?`, recordingID).Scan(&key, &genre); err != nil {
		t.Fatalf("reading derived columns of recording %d: %v", recordingID, err)
	}
	if key != wantKey {
		t.Fatalf("series_key of recording %d = %+v, want %+v", recordingID, key, wantKey)
	}
	if genre != wantGenre {
		t.Fatalf("genre_lv1 of recording %d = %s, want %s", recordingID, genre, wantGenre)
	}
}
