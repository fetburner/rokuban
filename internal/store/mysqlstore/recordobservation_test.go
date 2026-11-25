package mysqlstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

// TestRecordingSeriesKeyFollowsTitle は、録画を作る 3 つの経路が題名から series_key を書くことを、
// 値がある場合と無い場合（英数字を含まない題名）の両方で検査する。series_key は MySQL では生成列でなく
// アプリが書くので、書き忘れると NULL のまま残る。
func TestRecordingSeriesKeyFollowsTitle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		title string
		want  sql.NullString
	}{
		{name: "title with a series key", title: "ドラマＡ 第3話", want: sql.NullString{String: "ドラマA", Valid: true}},
		{name: "title without a series key", title: "【】", want: sql.NullString{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := mysqltest.SetupDB(t)
			recordings := NewRecordings(db)
			base := newContractRecordObservation("series-"+tc.name, store.RecordingStatusFinished)
			base.Snapshot.Title = tc.title

			success := base
			result, err := recordings.ProcessRecord(ctx, success)
			if err != nil || result.RecordingID == nil {
				t.Fatalf("ProcessRecord = (%v, %v), want a recording", result.RecordingID, err)
			}
			requireSeriesKey(t, db, *result.RecordingID, tc.want)

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
			requireSeriesKey(t, db, id, tc.want)

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
			requireSeriesKey(t, db, *scheduled.RecordingID, tc.want)
		})
	}
}

func requireSeriesKey(t *testing.T, db *sql.DB, recordingID int64, want sql.NullString) {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT series_key FROM recordings WHERE id = ?`, recordingID).Scan(&got); err != nil {
		t.Fatalf("reading series_key of recording %d: %v", recordingID, err)
	}
	if got != want {
		t.Fatalf("series_key of recording %d = %+v, want %+v", recordingID, got, want)
	}
}
