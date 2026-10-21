package contracttest

import (
	"context"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

func requireMediaReaderFixture(t *testing.T, f MediaFixture) {
	t.Helper()
	if f.Reader == nil || f.Progress == nil || f.NewRecording == nil || f.SetEncodeProfiles == nil || f.BeginEncodeAttempt == nil || f.FailEncodeAttempt == nil || f.NewAsset == nil || f.SetRecordingDropStats == nil || f.SetRecordingDropPositions == nil || f.StartEncodeProgressListener == nil {
		t.Fatalf("incomplete media reader fixture: %+v", f)
	}
}

func runEncodeQueue(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	requireMediaReaderFixture(t, f)
	ctx := context.Background()
	newDesiredRecording := func(profiles []string, runningProfile string, failed bool) int64 {
		t.Helper()
		id := f.NewRecording(t)
		f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d-original.m2ts", f.Site, id), "active")
		f.SetEncodeProfiles(t, id, profiles)
		if runningProfile != "" || failed {
			profile := runningProfile
			if failed {
				profile = profiles[0]
			}
			f.BeginEncodeAttempt(t, id, profile)
		}
		if failed {
			f.FailEncodeAttempt(t, id, profiles[0], 2)
		}
		return id
	}

	queuedID := newDesiredRecording([]string{"h264", "hevc"}, "", false)
	runningID := newDesiredRecording([]string{"h264"}, "h264", false)
	exhaustedID := newDesiredRecording([]string{"h264"}, "", true)
	unknownID := newDesiredRecording([]string{"not-configured"}, "", false)

	got, err := f.Reader.ListEncodeQueue(ctx, store.EncodeQueueParams{
		KnownProfiles: []string{"h264", "hevc"}, AttemptLimit: 2,
	})
	if err != nil {
		t.Fatalf("ListEncodeQueue: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("encode queue = %+v; want two queued profiles and one running profile", got)
	}
	counts := make(map[int64]int, len(got))
	running := make(map[int64]int, len(got))
	for _, entry := range got {
		counts[entry.RecordingID]++
		switch entry.RecordingID {
		case queuedID:
			if entry.Running {
				t.Fatalf("queued profile entry = %+v; want Running=false", entry)
			}
		case runningID:
			if !entry.Running {
				t.Fatalf("running entry = %+v; want Running=true", entry)
			}
			running[entry.RecordingID]++
		default:
			t.Fatalf("unexpected encode queue entry %+v; exhausted=%d unknown=%d", entry, exhaustedID, unknownID)
		}
	}
	if counts[queuedID] != 2 || counts[runningID] != 1 || running[runningID] != 1 {
		t.Fatalf("encode queue = %+v; want two queued profiles for %d and one running profile for %d", got, queuedID, runningID)
	}
}

func seedRecordingDropReads(t *testing.T, newFixture NewMediaFixture) (MediaFixture, int64, string, int64) {
	t.Helper()
	f := newFixture(t)
	requireMediaReaderFixture(t, f)
	firstRecording := f.NewRecording(t)
	otherRecording := f.NewRecording(t)
	firstAsset := f.NewAsset(t, firstRecording, "original", nil, "sites/contract/first-original.m2ts", "active")
	otherAsset := f.NewAsset(t, otherRecording, "original", nil, "sites/contract/other-original.m2ts", "active")
	pidType := "video"
	f.SetRecordingDropStats(t, firstAsset, []store.RecordingDropStat{{
		Pid: 256, Packets: 10_000, Drops: 4, Errors: 2, Scrambled: 1, PidType: &pidType,
	}})
	f.SetRecordingDropStats(t, otherAsset, []store.RecordingDropStat{{
		Pid: 512, Packets: 20_000, Drops: 8, Errors: 3, Scrambled: 2,
	}})
	elapsed := int64(900)
	f.SetRecordingDropPositions(t, firstAsset, []store.RecordingDropPosition{
		{Pid: 256, ByteOffset: 1024, ElapsedMs: &elapsed},
		{Pid: 256, ByteOffset: 4096},
	})
	f.SetRecordingDropPositions(t, otherAsset, []store.RecordingDropPosition{
		{Pid: 512, ByteOffset: 2048},
	})
	return f, firstRecording, pidType, elapsed
}

func runRecordingDropStats(t *testing.T, newFixture NewMediaFixture) {
	f, firstRecording, pidType, _ := seedRecordingDropReads(t, newFixture)
	ctx := context.Background()
	stats, err := f.Reader.ListRecordingDropStats(ctx, firstRecording)
	if err != nil {
		t.Fatalf("ListRecordingDropStats(first): %v", err)
	}
	if len(stats) != 1 || stats[0].Pid != 256 || stats[0].Packets != 10_000 || stats[0].Drops != 4 || stats[0].Errors != 2 || stats[0].Scrambled != 1 || stats[0].PidType == nil || *stats[0].PidType != pidType {
		t.Fatalf("drop stats = %+v; want only the first recording's written PID stats", stats)
	}
}

func runRecordingDropPositions(t *testing.T, newFixture NewMediaFixture) {
	f, firstRecording, _, elapsed := seedRecordingDropReads(t, newFixture)
	ctx := context.Background()
	positions, err := f.Reader.ListRecordingDropPositions(ctx, firstRecording)
	if err != nil {
		t.Fatalf("ListRecordingDropPositions(first): %v", err)
	}
	if len(positions) != 2 || positions[0].Pid != 256 || positions[0].ByteOffset != 1024 || positions[0].ElapsedMs == nil || *positions[0].ElapsedMs != elapsed || positions[1].Pid != 256 || positions[1].ByteOffset != 4096 || positions[1].ElapsedMs != nil {
		t.Fatalf("drop positions = %+v; want the first recording's two written positions", positions)
	}
}

func runEncodeProgressReport(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	requireMediaReaderFixture(t, f)
	ctx := context.Background()
	want := store.EncodeProgress{RecordingID: f.NewRecording(t), Profile: "h264", Progress: 0.375}
	waitForProgress := f.StartEncodeProgressListener(t)
	if err := f.Progress.ReportEncodeProgress(ctx, want); err != nil {
		t.Fatalf("ReportEncodeProgress: %v", err)
	}
	if got := waitForProgress(); got != want {
		t.Fatalf("received encode progress = %+v; want %+v", got, want)
	}
}
