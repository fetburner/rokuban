package contracttest

import (
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

func runCMStationOriginalPresence(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	f := newStore(t)
	ctx := t.Context()
	other := store.CMStationRef{NetworkID: 1, ServiceID: 1}
	// 局 A には実体が有る録画と無い録画があり、局 B では全て実体無しにする。
	present := f.NewStationRecording(t, station)
	missingA := f.NewStationRecording(t, station)
	missingB := f.NewStationRecording(t, other)
	for _, input := range []struct {
		station     store.CMStationRef
		recordingID int64
	}{{station, present}, {other, missingB}} {
		if _, err := f.Store.SaveLogoArea(ctx, input.station, testAreaInput(input.recordingID, 10)); err != nil {
			t.Fatalf("saving logo area with present original: %v", err)
		}
	}
	f.MarkOriginalMissing(t, missingA)
	f.MarkOriginalMissing(t, missingB)
	if _, err := f.Store.SaveLogoArea(ctx, other, testAreaInput(missingB, 20)); !errors.Is(err, store.ErrCMLogoRecordingOriginalUnavailable) {
		t.Errorf("SaveLogoArea with missing original = %v, want ErrCMLogoRecordingOriginalUnavailable", err)
	}

	jobs, err := f.Detection.ListMissingLogoCandidates(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].Station != station || jobs[0].RecordingID != present {
		t.Errorf("logo candidate jobs = %+v, %v; want only present recording %d", jobs, err, present)
	}
	result, err := f.Store.DeleteLogo(ctx, station)
	if err != nil || result.CandidateJob == nil || result.CandidateJob.RecordingID != present {
		t.Errorf("DeleteLogo for station with a present original = %+v, %v; want candidate for %d", result, err, present)
	}
	result, err = f.Store.DeleteLogo(ctx, other)
	if err != nil || result.CandidateJob != nil {
		t.Errorf("DeleteLogo for missing-only station = %+v, %v; want no candidate", result, err)
	}
	for _, id := range []int64{present, missingA, missingB} {
		f.SeedDetection(t, id)
	}
	rows, err := f.Store.ListCMLogoStates(ctx)
	if err != nil {
		t.Fatalf("ListCMLogoStates: %v", err)
	}
	// detected は実体の有無によらず検出結果を数え、redetectable は実体の有る原本の検出だけを数える。
	type status struct {
		detected, redetectable, frame int64
	}
	got := make(map[store.CMStationRef]status)
	for _, row := range rows {
		got[store.CMStationRef{NetworkID: row.NetworkID, ServiceID: row.ServiceID}] = status{row.DetectedCount, row.RedetectableCount, row.FrameRecordingID}
	}
	if got[station] != (status{2, 1, present}) {
		t.Errorf("mixed station = %+v, want two detected, one redetectable, and frame from %d", got[station], present)
	}
	if missing, ok := got[other]; !ok || missing != (status{1, 0, 0}) {
		t.Errorf("missing-only station = %+v, found=%v; want one detected, no redetectable, and no frame", missing, ok)
	}
}

func runCMStationAdoptionPresence(t *testing.T, newStore NewCMStationStore, station store.CMStationRef) {
	f := newStore(t)
	ctx := t.Context()
	other := store.CMStationRef{NetworkID: 1, ServiceID: 1}
	present, missing := f.NewStationRecording(t, station), f.NewStationRecording(t, other)
	for _, input := range []struct {
		station     store.CMStationRef
		recordingID int64
	}{{station, present}, {other, missing}} {
		saved, err := f.Store.SaveLogoArea(ctx, input.station, testAreaInput(input.recordingID, 10))
		if err != nil {
			t.Fatalf("saving logo area: %v", err)
		}
		f.SeedCandidate(t, input.station, input.recordingID, saved.AreaUpdatedAt, time.Now(), "ready")
		f.SeedDetection(t, input.recordingID)
	}
	f.MarkOriginalMissing(t, missing)
	// 実体無しの局を先に採用し、検出結果の消去を局ごとに観測する。
	for _, input := range []struct {
		station      store.CMStationRef
		recordingID  int64
		wantDetected bool
	}{{other, missing, true}, {station, present, false}} {
		if err := f.Store.AdoptLogoCandidate(ctx, input.station, true); err != nil {
			t.Fatalf("adopting candidate: %v", err)
		}
		got, err := f.Detection.GetWorkItem(ctx, input.recordingID)
		if err != nil || got.Detected != input.wantDetected {
			t.Errorf("after adoption recording %d detected=%v, err=%v; want detected=%v", input.recordingID, got.Detected, err, input.wantDetected)
		}
	}
}
