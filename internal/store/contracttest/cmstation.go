package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// CMStationDetection は局単位の契約が検出側から読むメソッドだけを表す。
// 残りの CMDetection は局単位の契約の検査に要らないので、ここには含めない。
type CMStationDetection interface {
	// ListMissingLogoCandidates は desired な候補解析を返す。
	ListMissingLogoCandidates(ctx context.Context, limit int32) ([]store.CMLogoCandidateJob, error)
	// GetWorkItem は録画と原本の検出入力を返す。
	GetWorkItem(ctx context.Context, recordingID int64) (store.CMDetectionWorkItem, error)
}

// CMStationFixture は局単位の CM ロゴ契約と、テスト用状態作成フックをまとめる。
type CMStationFixture struct {
	Store               store.CMStation
	Detection           CMStationDetection
	NewStationRecording func(t *testing.T, station store.CMStationRef) int64
	MarkOriginalMissing func(t *testing.T, recordingID int64)
	SeedDetection       func(t *testing.T, recordingID int64)
	// SetRecordingNames は録画の表示名と site を書き換える（最新の録画から名前を取ることを確かめる）。
	SetRecordingNames func(t *testing.T, recordingID int64, serviceName, site string)
	// EnableCMDetection は録画の CM 検出を有効にする（方針の行を作る）。
	EnableCMDetection func(t *testing.T, recordingID int64)
	// SeedFailedAttempt は現在の方針世代の failed 試行を作る。
	SeedFailedAttempt func(t *testing.T, recordingID int64, stage string, attemptedAt time.Time)
	NewRecording      func(t *testing.T) int64
	// BeginStationWrite は同じ局の書き込みロックを保持した tx を作り、commit 関数を返す。
	BeginStationWrite func(t *testing.T, station store.CMStationRef) (commit func())
	// WaitForStationWriteWait は name のメソッドがその局ロックで待っていることを確認する。
	WaitForStationWriteWait func(t *testing.T, station store.CMStationRef, name string)
	// SeedCandidate は指定した枠版・試行時刻の候補行を作る。
	SeedCandidate func(t *testing.T, station store.CMStationRef, recordingID int64, areaUpdatedAt, attemptedAt time.Time, state string)
	// FailCandidateDelete は候補の削除を失敗させ、返す関数で故障注入を解除する。
	FailCandidateDelete func(t *testing.T, station store.CMStationRef) (cleanup func())
}

// NewCMStationStore は実在の CMStation 契約実装とフィクスチャを作る。
type NewCMStationStore func(t *testing.T) CMStationFixture

// RunCMStation は実在する store 実装に局単位 CM ロゴの契約を実行する。
func RunCMStation(t *testing.T, newStore NewCMStationStore) {
	t.Helper()
	station := store.CMStationRef{NetworkID: 32678, ServiceID: 5168}
	runCMStationSerialization(t, newStore, station)
	t.Run("logo hints and station counts require present originals", func(t *testing.T) {
		runCMStationOriginalPresence(t, newStore, station)
	})
	t.Run("candidate adoption preserves detections without a present original", func(t *testing.T) {
		runCMStationAdoptionPresence(t, newStore, station)
	})

	t.Run("candidate results reject an old area version", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		recordingID := f.NewRecording(t)
		first, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
		if err != nil {
			t.Fatalf("saving initial area: %v", err)
		}
		attemptedAt, started, err := f.Store.BeginLogoCandidate(ctx, station, recordingID, first.AreaUpdatedAt)
		if err != nil || !started {
			t.Fatalf("BeginLogoCandidate() = %s, %v, %v; want started", attemptedAt, started, err)
		}
		time.Sleep(time.Millisecond)
		if _, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 20)); err != nil {
			t.Fatalf("saving changed area: %v", err)
		}
		f.SeedCandidate(t, station, recordingID, first.AreaUpdatedAt, attemptedAt, "running")
		ready, err := f.Store.MarkLogoCandidateReady(ctx, station, first.AreaUpdatedAt, attemptedAt, []byte("logo"), nil)
		if err != nil || ready {
			t.Fatalf("MarkLogoCandidateReady(old area version) = %v, %v; want no write", ready, err)
		}
		candidate, err := f.Store.GetLogoCandidate(ctx, station)
		if err != nil {
			t.Fatalf("reading stale candidate: %v", err)
		}
		if candidate.State != "running" {
			t.Fatalf("stale candidate state = %q, want running", candidate.State)
		}
	})

	t.Run("candidate failure marks only the matching running attempt", func(t *testing.T) {
		runCMStationCandidateFailure(t, newStore, station)
	})

	runCMStationStateList(t, newStore, station)

	t.Run("discarding an old attempt leaves a recreated candidate", func(t *testing.T) {
		runCMStationDiscardOldAttempt(t, newStore, station)
	})

	t.Run("area and candidate deletion roll back together", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		recordingID := f.NewRecording(t)
		initial, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
		if err != nil {
			t.Fatalf("saving initial area: %v", err)
		}
		f.SeedCandidate(t, station, recordingID, initial.AreaUpdatedAt, time.Now().UTC(), "ready")
		cleanup := f.FailCandidateDelete(t, station)
		defer cleanup()
		if _, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 20)); err == nil {
			t.Fatal("SaveLogoArea with candidate deletion failure returned nil")
		}
		area, err := f.Store.GetLogoArea(ctx, station)
		if err != nil {
			t.Fatalf("reading area after failed save: %v", err)
		}
		if area.UpdatedAt != initial.AreaUpdatedAt || area.X != 10 {
			t.Fatalf("area after failed save = %+v, want original version and x=10", area)
		}
		candidate, err := f.Store.GetLogoCandidate(ctx, station)
		if err != nil {
			t.Fatalf("reading candidate after failed save: %v", err)
		}
		if candidate.State != "ready" {
			t.Fatalf("candidate after failed save = %q, want ready", candidate.State)
		}
	})

	t.Run("logo deletion returns its next hint destination", func(t *testing.T) {
		f := newStore(t)
		ctx := context.Background()
		recordingID := f.NewRecording(t)
		saved, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
		if err != nil {
			t.Fatalf("saving area: %v", err)
		}
		result, err := f.Store.DeleteLogo(ctx, station)
		if err != nil {
			t.Fatalf("deleting logo with an area and original: %v", err)
		}
		if result.CandidateJob == nil {
			t.Fatal("DeleteLogo with an area and original returned no candidate destination")
		}
		if result.CandidateJob.Station != station || result.CandidateJob.RecordingID != recordingID ||
			result.CandidateJob.AreaUpdatedAt != saved.AreaUpdatedAt {
			t.Fatalf("candidate destination = %+v, want station, recording, and area version from saved state", result.CandidateJob)
		}
		if err := f.Store.DeleteLogoArea(ctx, station); err != nil {
			t.Fatalf("deleting area: %v", err)
		}
		result, err = f.Store.DeleteLogo(ctx, station)
		if err != nil {
			t.Fatalf("deleting logo without an area: %v", err)
		}
		if result.CandidateJob != nil {
			t.Fatalf("DeleteLogo without an area returned candidate destination %+v, want reconcile destination", result.CandidateJob)
		}
	})

	t.Run("missing station reads use the store error", func(t *testing.T) {
		f := newStore(t)
		if _, err := f.Store.GetLogoArea(context.Background(), station); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetLogoArea(missing) error = %v, want ErrNotFound", err)
		}
		if _, err := f.Store.GetLogo(context.Background(), station); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetLogo(missing) error = %v, want ErrNotFound", err)
		}
		if _, err := f.Store.GetLogoCandidate(context.Background(), station); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetLogoCandidate(missing) error = %v, want ErrNotFound", err)
		}
		if _, err := f.Store.SaveLogoArea(context.Background(), station, testAreaInput(-1, 10)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("SaveLogoArea(missing recording) error = %v, want ErrNotFound", err)
		}
	})
}

// runCMStationCandidateFailure は、試行時刻が一致する running 候補だけが failed になることを確かめる。
// 一致しない試行時刻では書かず、stage save を受け付けることも確かめる。
func runCMStationCandidateFailure(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	recordingID := f.NewRecording(t)
	area, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
	if err != nil {
		t.Fatalf("saving area: %v", err)
	}
	attemptedAt, started, err := f.Store.BeginLogoCandidate(ctx, station, recordingID, area.AreaUpdatedAt)
	if err != nil || !started {
		t.Fatalf("BeginLogoCandidate() = %s, %v, %v; want started", attemptedAt, started, err)
	}
	stage, message := "save", "saving failed"
	ok, err := f.Store.MarkLogoCandidateFailed(ctx, station, area.AreaUpdatedAt, attemptedAt.Add(time.Second), &stage, message)
	if err != nil || ok {
		t.Fatalf("MarkLogoCandidateFailed(other attempt time) = %v, %v; want no write", ok, err)
	}
	if c, err := f.Store.GetLogoCandidate(ctx, station); err != nil || c.State != "running" {
		t.Fatalf("candidate after other attempt time = %+v, %v; want running", c, err)
	}
	ok, err = f.Store.MarkLogoCandidateFailed(ctx, station, area.AreaUpdatedAt, attemptedAt, &stage, message)
	if err != nil || !ok {
		t.Fatalf("MarkLogoCandidateFailed(running attempt) = %v, %v; want the running row marked", ok, err)
	}
	candidate, err := f.Store.GetLogoCandidate(ctx, station)
	if err != nil {
		t.Fatalf("reading failed candidate: %v", err)
	}
	if candidate.State != "failed" || candidate.Stage == nil || *candidate.Stage != stage ||
		candidate.Error == nil || *candidate.Error != message {
		t.Fatalf("failed candidate = %+v, want failed with stage %q and error %q", candidate, stage, message)
	}
}

// runCMStationSerialization は、局の書き込みロックを保持中は局の書き込みが待つことを確かめる。
func runCMStationSerialization(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	t.Helper()
	serialized := []struct {
		name string
		call func(ctx context.Context, s store.CMStation, recordingID int64, areaUpdatedAt time.Time) error
	}{
		{"SaveLogoArea", func(ctx context.Context, s store.CMStation, id int64, _ time.Time) error {
			_, err := s.SaveLogoArea(ctx, station, testAreaInput(id, 20))
			return err
		}},
		{"DeleteLogoArea", func(ctx context.Context, s store.CMStation, _ int64, _ time.Time) error {
			return s.DeleteLogoArea(ctx, station)
		}},
		{"DeleteLogo", func(ctx context.Context, s store.CMStation, _ int64, _ time.Time) error {
			_, err := s.DeleteLogo(ctx, station)
			return err
		}},
		{"DeleteLogoCandidate", func(ctx context.Context, s store.CMStation, _ int64, _ time.Time) error {
			return s.DeleteLogoCandidate(ctx, station)
		}},
		{"AdoptLogoCandidate", func(ctx context.Context, s store.CMStation, _ int64, _ time.Time) error {
			return s.AdoptLogoCandidate(ctx, station, false)
		}},
		{"BeginLogoCandidate", func(ctx context.Context, s store.CMStation, id int64, at time.Time) error {
			_, _, err := s.BeginLogoCandidate(ctx, station, id, at)
			return err
		}},
	}
	for _, tc := range serialized {
		t.Run("station writes serialize with an open station write/"+tc.name, func(t *testing.T) {
			f := newStore(t)
			ctx := context.Background()
			recordingID := f.NewRecording(t)
			saved, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
			if err != nil {
				t.Fatalf("saving initial area: %v", err)
			}
			commit := f.BeginStationWrite(t, station)
			result := make(chan error, 1)
			go func() { result <- tc.call(ctx, f.Store, recordingID, saved.AreaUpdatedAt) }()
			f.WaitForStationWriteWait(t, station, tc.name)
			select {
			case err := <-result:
				commit()
				t.Fatalf("%s returned %v while the station write was open, want it to wait", tc.name, err)
			default:
			}
			commit()
			if err := <-result; err != nil {
				t.Logf("%s completed after station-write release: %v", tc.name, err)
			}
		})
	}
}

// runCMStationDiscardOldAttempt は、削除して作り直された候補に古い試行の discard が効かないことを検査する。
// 古い試行の discard は no-op で、作り直された running 行は新しい attempted_at のまま残る。
// discard の一致条件を外すと、古い試行の discard が行を消して失敗する。
// 先に一致する試行の discard が行を消すことを確かめ、discard が何も消さない誤りで空虚に通らないようにする。
func runCMStationDiscardOldAttempt(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	t.Helper()
	f := newStore(t)
	ctx := context.Background()
	recordingID := f.NewRecording(t)
	area, err := f.Store.SaveLogoArea(ctx, station, testAreaInput(recordingID, 10))
	if err != nil {
		t.Fatalf("saving area: %v", err)
	}
	// 削除前の試行と作り直した後の試行。固定値で 1 秒ずらす。
	oldAttempt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newAttempt := oldAttempt.Add(time.Second)
	f.SeedCandidate(t, station, recordingID, area.AreaUpdatedAt, oldAttempt, "running")
	if err := f.Store.DiscardLogoCandidate(ctx, station, area.AreaUpdatedAt, oldAttempt); err != nil {
		t.Fatalf("discarding the current attempt: %v", err)
	}
	if _, err := f.Store.GetLogoCandidate(ctx, station); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("candidate after discarding the current attempt: err = %v, want not found", err)
	}
	f.SeedCandidate(t, station, recordingID, area.AreaUpdatedAt, newAttempt, "running")

	if err := f.Store.DiscardLogoCandidate(ctx, station, area.AreaUpdatedAt, oldAttempt); err != nil {
		t.Fatalf("discarding the old attempt = %v; want no-op", err)
	}
	candidate, err := f.Store.GetLogoCandidate(ctx, station)
	if err != nil {
		t.Fatalf("reading recreated candidate: %v", err)
	}
	if candidate.State != "running" || !candidate.AttemptedAt.Equal(newAttempt) {
		t.Fatalf("recreated candidate = %q at %s, want running at %s", candidate.State, candidate.AttemptedAt, newAttempt)
	}
}

func testAreaInput(recordingID int64, x int32) store.CMLogoAreaInput {
	return store.CMLogoAreaInput{
		RecordingID: recordingID, X: x, Y: 10, W: 100, H: 50,
		CodedWidth: 1920, CodedHeight: 1080,
	}
}
