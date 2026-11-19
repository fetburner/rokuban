package db_test

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestCMRecordingDuration は、CM 検出の実尺の式を 3 本のクエリで同じ表により検査する。
// 3 本は ListMissingCMDetections、GetCMRecordingDuration、ListMissingCMLogoCandidates で、
// 締切の入力になる値が 3 本とも揃うことも確かめる。
// 各ケースの録画は cm_detection_desired と cm_logo_candidate_desired の両方に入るので、
// 一覧に当該録画が入ることも同時に検査する。一覧から外れる変異は、値の比較を空振りさせずに落ちる。
// CASE の started_at IS NOT NULL を外すと started_null_uses_program_duration が落ちる。
// ELSE を 0 にすると、NULL を含む 3 ケースが落ちる。GREATEST を外すと ended_before_started_clamps_to_zero が落ちる。
// どれも 1 本だけ壊すと、そのクエリの値のアサーションだけが落ちる。
func TestCMRecordingDuration(t *testing.T) {
	t.Parallel()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := start.Add(d)
		return &v
	}
	for _, tc := range []struct {
		name      string
		startedAt *time.Time // nil なら NULL
		endedAt   *time.Time // nil なら NULL
		programMs int64
		want      int64
	}{
		{name: "both_present_returns_difference", startedAt: at(0), endedAt: at(45 * time.Minute), programMs: 1, want: 45 * 60 * 1000},
		{name: "both_null_uses_program_duration", programMs: 2_400_000, want: 2_400_000},
		{name: "started_null_uses_program_duration", endedAt: at(45 * time.Minute), programMs: 2_400_000, want: 2_400_000},
		{name: "ended_null_uses_program_duration", startedAt: at(0), programMs: 2_400_000, want: 2_400_000},
		{name: "ended_before_started_clamps_to_zero", startedAt: at(45 * time.Minute), endedAt: at(0), programMs: 2_400_000, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			f := seedCMDesiredComplete(t, pool)
			testutil.MustExec(t, pool, "setting recording times and duration",
				`UPDATE recordings SET started_at = $2, ended_at = $3, program_duration_ms = $4 WHERE id = $1`,
				f.recordingID, tc.startedAt, tc.endedAt, tc.programMs)
			q := sqlcgen.New(pool)
			testutil.RequireNoError(t, q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
				NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
			}), "UpsertCMLogoArea")

			detections, err := q.ListMissingCMDetections(ctx, 100)
			testutil.RequireNoError(t, err, "ListMissingCMDetections")
			if len(detections) != 1 || detections[0].RecordingID != f.recordingID {
				t.Fatalf("ListMissingCMDetections = %#v, want only recording %d", detections, f.recordingID)
			}
			candidates, err := q.ListMissingCMLogoCandidates(ctx, 100)
			testutil.RequireNoError(t, err, "ListMissingCMLogoCandidates")
			if len(candidates) != 1 || candidates[0].RecordingID != f.recordingID {
				t.Fatalf("ListMissingCMLogoCandidates = %#v, want only recording %d", candidates, f.recordingID)
			}
			duration, err := q.GetCMRecordingDuration(ctx, f.recordingID)
			testutil.RequireNoError(t, err, "GetCMRecordingDuration")

			if duration != tc.want {
				t.Errorf("GetCMRecordingDuration = %d, want %d", duration, tc.want)
			}
			if got := detections[0].RecordingDurationMs; got != tc.want {
				t.Errorf("ListMissingCMDetections duration = %d, want %d", got, tc.want)
			}
			if got := candidates[0].RecordingDurationMs; got != tc.want {
				t.Errorf("ListMissingCMLogoCandidates duration = %d, want %d", got, tc.want)
			}
		})
	}
}
