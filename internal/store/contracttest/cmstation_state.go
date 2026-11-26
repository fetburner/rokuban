package contracttest

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// runCMStationStateList は ListCMLogoStates の API が読むフィールドを検査する。
// 局の表示名・件数・失敗・再検出・フレームの元・枠・学習済みロゴ・候補の要約を、実際の状態から確かめる。
func runCMStationStateList(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	t.Helper()
	t.Run("logo state list takes names from the newest recording and counts each state", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		oldest := f.NewStationRecording(t, station)
		middle := f.NewStationRecording(t, station)
		newest := f.NewStationRecording(t, station)
		f.SetRecordingNames(t, oldest, "旧名", "old-site")
		f.SetRecordingNames(t, middle, "中名", "mid-site")
		f.SetRecordingNames(t, newest, "新名", "new-site")
		for _, id := range []int64{oldest, middle, newest} {
			f.EnableCMDetection(t, id)
		}
		// 失敗した試行の録画は再試行の対象にならないので pending に数えない。
		f.SeedFailedAttempt(t, oldest, "match", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		f.SeedDetection(t, newest)

		got := cmStationRow(t, ctx, f, station)
		if got.ServiceName != "新名" || got.Site != "new-site" {
			t.Errorf("names = %q/%q, want the newest recording's 新名/new-site", got.ServiceName, got.Site)
		}
		if got.RecordingCount != 3 || got.FailedCount != 1 || got.PendingCount != 1 || got.LastFailureStage != "match" {
			t.Errorf("counts recording/failed/pending/stage = %d/%d/%d/%q, want 3/1/1/\"match\"",
				got.RecordingCount, got.FailedCount, got.PendingCount, got.LastFailureStage)
		}
		if got.DetectedCount != 1 || got.RedetectableCount != 1 {
			t.Errorf("detected/redetectable = %d/%d, want 1/1", got.DetectedCount, got.RedetectableCount)
		}
		if got.FrameRecordingID != newest {
			t.Errorf("frame recording = %d, want newest present original %d", got.FrameRecordingID, newest)
		}
		if got.Area != nil || got.LearnedAt != nil || got.Candidate != nil {
			t.Errorf("station without area, logo, or candidate = area %+v, learned %v, candidate %+v", got.Area, got.LearnedAt, got.Candidate)
		}
	})

	t.Run("logo state list reports the area and learned logo", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		recordingID := f.NewStationRecording(t, station)
		area, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
		if err != nil {
			t.Fatalf("saving area: %v", err)
		}
		f.SeedCandidate(t, station, recordingID, area.AreaUpdatedAt, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "ready")
		if err := f.Store.AdoptLogoCandidate(ctx, station, false); err != nil {
			t.Fatalf("adopting candidate: %v", err)
		}

		got := cmStationRow(t, ctx, f, station)
		if got.Area == nil || got.Area.X != 10 || got.Area.Y != 10 || got.Area.W != 100 || got.Area.H != 50 ||
			got.Area.CodedWidth != 1920 || got.Area.CodedHeight != 1080 || !got.Area.UpdatedAt.Equal(area.AreaUpdatedAt) {
			t.Errorf("area = %+v, want the saved area at version %s", got.Area, area.AreaUpdatedAt)
		}
		if got.LearnedAt == nil || got.LearnedCodedWidth == nil || *got.LearnedCodedWidth != 1920 ||
			got.LearnedCodedHeight == nil || *got.LearnedCodedHeight != 1080 {
			t.Errorf("learned logo = at %v, %v x %v; want learned at 1920x1080", got.LearnedAt, got.LearnedCodedWidth, got.LearnedCodedHeight)
		}
		if got.Candidate != nil {
			t.Errorf("candidate after adoption = %+v, want none", got.Candidate)
		}
	})

	t.Run("logo state list reports the candidate summary", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		other := store.CMStationRef{NetworkID: 1, ServiceID: 1}
		recordingID := f.NewStationRecording(t, other)
		area, err := f.Store.SaveLogoArea(ctx, other, testAreaInput(recordingID, 10))
		if err != nil {
			t.Fatalf("saving area: %v", err)
		}
		attemptedAt, started, err := f.Store.BeginLogoCandidate(ctx, other, recordingID, area.AreaUpdatedAt)
		if err != nil || !started {
			t.Fatalf("BeginLogoCandidate() = %s, %v, %v; want started", attemptedAt, started, err)
		}
		stage, message := "save", "saving failed"
		if ok, err := f.Store.MarkLogoCandidateFailed(ctx, other, area.AreaUpdatedAt, attemptedAt, &stage, message); err != nil || !ok {
			t.Fatalf("MarkLogoCandidateFailed() = %v, %v; want marked", ok, err)
		}

		got := cmStationRow(t, ctx, f, other)
		if got.Candidate == nil {
			t.Fatal("candidate summary = nil, want the failed candidate")
		}
		c := got.Candidate
		if c.State != "failed" || c.Stage == nil || *c.Stage != stage || c.Error == nil || *c.Error != message ||
			c.RecordingID == nil || *c.RecordingID != recordingID || !c.AttemptedAt.Equal(attemptedAt) ||
			c.X != 10 || c.W != 100 || c.CodedWidth != 1920 {
			t.Errorf("candidate summary = %+v, want failed/save/%q at %s for recording %d", c, message, attemptedAt, recordingID)
		}
		if got.LearnedAt != nil {
			t.Errorf("learned logo without adoption = %v, want none", got.LearnedAt)
		}
	})
}

// cmStationRow は ListCMLogoStates の結果から局の行を 1 つ返す。無ければテストを失敗させる。
func cmStationRow(t *testing.T, ctx context.Context, f CMStationFixture, station store.CMStationRef) store.CMLogoState {
	t.Helper()
	rows, err := f.Store.ListCMLogoStates(ctx)
	if err != nil {
		t.Fatalf("ListCMLogoStates: %v", err)
	}
	for _, row := range rows {
		if row.NetworkID == station.NetworkID && row.ServiceID == station.ServiceID {
			return row
		}
	}
	t.Fatalf("ListCMLogoStates has no row for network %d service %d", station.NetworkID, station.ServiceID)
	return store.CMLogoState{}
}
