package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// CMStationFixture は局単位の CM ロゴ契約と、テスト用状態作成フックをまとめる。
type CMStationFixture struct {
	Store        store.CMStation
	NewRecording func(t *testing.T) int64
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
			<-result // 待ち解除後の成否は問わない。待ったことだけを主張する。
		})
	}

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

func testAreaInput(recordingID int64, x int32) store.CMLogoAreaInput {
	return store.CMLogoAreaInput{
		RecordingID: recordingID, X: x, Y: 10, W: 100, H: 50,
		CodedWidth: 1920, CodedHeight: 1080,
	}
}
