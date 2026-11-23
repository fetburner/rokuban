package contracttest

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

var transferOriginalRules = []store.OriginalStoreRule{
	{Site: "tiered", Store: "archive"},
	{Site: "remote", Store: "remote"},
}

func runCountMediaAssetTransferBacklogBySourceStoreMatchesCandidates(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	fsStores := []string{"edge", "backup", "archive"}
	sourceStores := []string{"edge", "backup"}
	media := f.WithPolicy("test-store", transferOriginalRules, fsStores, "derived")
	const attemptLimit int32 = 10

	firstRecording := f.NewRecording(t, "tiered", time.Time{})
	first, _ := f.NewAsset(t, firstRecording, "original", "edge", "backlog-test")
	secondRecording := f.NewRecording(t, "tiered", time.Time{})
	second, _ := f.NewAsset(t, secondRecording, "original", "backup", "backlog-test")
	f.NewAsset(t, f.NewRecording(t, "recent", time.Time{}), "original", "edge", "backlog-test")

	destinationPresent, _ := f.NewAsset(t, f.NewRecording(t, "tiered", time.Time{}), "original", "edge", "backlog-test")
	f.AddCopy(t, destinationPresent, "archive", fmt.Sprintf("backlog-test/%d/archive.ts", destinationPresent), "active", 0)

	_, missingCopyID := f.NewAsset(t, f.NewRecording(t, "tiered", time.Time{}), "original", "edge", "backlog-test")
	f.MarkCopyMissing(t, missingCopyID)

	exhaustedAsset, _ := f.NewAsset(t, f.NewRecording(t, "tiered", time.Time{}), "original", "edge", "backlog-test")
	exhaustedCopyID := f.AddCopy(t, exhaustedAsset, "archive", fmt.Sprintf("backlog-test/%d/archive.ts", exhaustedAsset), "pending", 0)
	f.SeedTransferAttempts(t, exhaustedCopyID, attemptLimit, "exhausted")

	f.NewAsset(t, f.NewRecording(t, "remote", time.Time{}), "original", "edge", "backlog-test")
	// 派生物は規則を見ず、派生物の置き場（ここでは FS 外の S3）へ送る。転送元が FS にあれば候補になる。
	derived, _ := f.NewAsset(t, f.NewRecording(t, "tiered", time.Time{}), "encoded", "edge", "backlog-test")
	derivedAlreadyThere, _ := f.NewAsset(t, f.NewRecording(t, "tiered", time.Time{}), "encoded", "edge", "backlog-test")
	f.AddCopy(t, derivedAlreadyThere, "derived", fmt.Sprintf("backlog-test/%d/derived.ts", derivedAlreadyThere), "active", 0)
	deletedRecording := f.NewRecording(t, "tiered", time.Time{})
	f.NewAsset(t, deletedRecording, "original", "edge", "backlog-test")
	f.TrashRecording(t, deletedRecording)

	candidateCount, byteCount, err := media.CountMediaAssetTransferCandidates(ctx, attemptLimit)
	if err != nil {
		t.Fatalf("counting transfer candidates: %v", err)
	}
	if byteCount != 3 {
		t.Fatalf("candidate bytes = %d, want 3", byteCount)
	}
	byStore, err := media.CountMediaAssetTransferBacklogBySourceStore(ctx, sourceStores, attemptLimit)
	if err != nil {
		t.Fatalf("counting backlog by source store: %v", err)
	}
	var backlogCount int64
	for _, count := range byStore {
		backlogCount += count
	}
	// 候補は、原本 2（tiered の edge と backup）と、FS 外の派生物の置き場 derived へ送る派生物 1（edge）。
	// 宛先が既にある派生物は候補にならない。
	if candidateCount != 3 || backlogCount != candidateCount || byStore["edge"] != 2 || byStore["backup"] != 1 || len(byStore) != 2 {
		t.Fatalf("candidate count = %d, backlog by source store = %v (sum %d); want 3 candidates split edge=2 and backup=1", candidateCount, byStore, backlogCount)
	}
	want := map[int64]store.MediaAssetTransferCandidate{
		first:   {MediaAssetID: first, Store: "archive", SourceStore: "edge"},
		second:  {MediaAssetID: second, Store: "archive", SourceStore: "backup"},
		derived: {MediaAssetID: derived, Store: "derived", SourceStore: "edge"},
	}
	got, err := media.ListMediaAssetTransferCandidates(ctx, attemptLimit, 10)
	gotByID := make(map[int64]store.MediaAssetTransferCandidate, len(got))
	for _, candidate := range got {
		gotByID[candidate.MediaAssetID] = candidate
	}
	if err != nil || len(got) != 3 || !maps.Equal(gotByID, want) {
		t.Fatalf("ListMediaAssetTransferCandidates = (%+v, %v), want %+v", got, err, want)
	}
	got, err = media.ListMediaAssetTransferCandidates(ctx, attemptLimit, 2)
	if err != nil || len(got) != 2 || got[0].MediaAssetID == got[1].MediaAssetID {
		t.Fatalf("ListMediaAssetTransferCandidates with limit 2 = (%+v, %v), want two distinct candidates", got, err)
	}
	for _, candidate := range got {
		if candidate != want[candidate.MediaAssetID] {
			t.Fatalf("unexpected candidate with limit 2: %+v", candidate)
		}
	}
}

// 原本の宛先は FS 置き場に限り、派生物には FS 外の宛先も使える。ごみ箱に入った録画は対象外になる。
func runStorageTransferDestination(t *testing.T, newStore NewStorageTransferStore) {
	fsStores := []string{"edge", "backup", "archive"}
	tests := []struct {
		name      string
		kind      string
		site      string
		derived   string
		deleted   bool
		wantStore string
	}{
		{name: "original matching a FS rule returns the FS store", kind: "original", site: "tiered", derived: "derived", wantStore: "archive"},
		{name: "original matching a non-FS rule returns NULL", kind: "original", site: "remote", derived: "derived"},
		{name: "original matching no rule returns NULL", kind: "original", site: "other", derived: "derived"},
		{name: "deleted recording returns NULL even when the FS rule matches", kind: "original", site: "tiered", derived: "derived", deleted: true},
		{name: "derived asset returns its FS store", kind: "encoded", site: "tiered", derived: "edge", wantStore: "edge"},
		{name: "derived asset returns the derived store even when it is not an FS store", kind: "encoded", site: "tiered", derived: "derived", wantStore: "derived"},
		{name: "deleted derived asset returns NULL", kind: "encoded", site: "tiered", derived: "edge", deleted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			ctx := t.Context()
			media := f.WithPolicy("backup", transferOriginalRules, fsStores, tc.derived)
			end := transferDatabaseClock(t, media)
			recordingID := f.NewRecording(t, tc.site, end.Add(-30*time.Minute))
			assetID, _ := f.NewAsset(t, recordingID, tc.kind, "backup", "destination-test")
			if tc.deleted {
				f.TrashRecording(t, recordingID)
			}
			var want []store.MediaAssetTransferCandidate
			if tc.wantStore != "" {
				want = []store.MediaAssetTransferCandidate{{MediaAssetID: assetID, Store: tc.wantStore, SourceStore: "backup"}}
			}
			got, err := media.ListMediaAssetTransferCandidates(ctx, 10, 10)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("ListMediaAssetTransferCandidates = (%+v, %v), want %+v", got, err, want)
			}
		})
	}
}

// pending の予約は、録画がごみ箱に入るか現在の宛先と食い違うと回収対象になる。
// 規則は tiered を archive へ、remote を FS 外の remote へ送る。派生物の置き場 derived は FS 外でも宛先になる。
func runStorageTransferPendingCopyObsolete(t *testing.T, newStore NewStorageTransferStore) {
	fsStores := []string{"edge", "backup", "archive"}
	tests := []struct {
		name      string
		kind      string
		site      string
		copyStore string
		deleted   bool
		want      bool
	}{
		{name: "deleted recording makes the reservation obsolete", site: "tiered", copyStore: "archive", deleted: true, want: true},
		{name: "desired store outside FS makes the reservation obsolete", site: "remote", copyStore: "archive", want: true},
		{name: "another desired store makes the reservation obsolete", site: "tiered", copyStore: "backup", want: true},
		{name: "matching desired store keeps the reservation", site: "tiered", copyStore: "archive", want: false},
		{name: "derived asset reserved on its non-FS derived store keeps the reservation", kind: "encoded", site: "tiered", copyStore: "derived", want: false},
		{name: "derived asset reserved on another store becomes obsolete", kind: "encoded", site: "tiered", copyStore: "backup", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			ctx := t.Context()
			media := f.WithPolicy(tc.copyStore, transferOriginalRules, fsStores, "derived")
			recordingID := f.NewRecording(t, tc.site, time.Time{})
			assetID, _ := f.NewAsset(t, recordingID, tc.kind, "edge", "obsolete-test")
			relPath := fmt.Sprintf("obsolete-test/%d/%s.ts", assetID, tc.copyStore)
			copyID := f.AddCopy(t, assetID, tc.copyStore, relPath, "pending", 0)
			if tc.deleted {
				f.TrashRecording(t, recordingID)
			}
			candidate := store.MediaAssetCopyDeleteCandidate{CopyID: copyID, MediaAssetID: assetID, Store: tc.copyStore, RelPath: relPath}
			called := false
			// 回収時の条件を検査する。列挙には派生物の FS staging に対する追加の制限がある。
			got, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(ctx, candidate, func([]string) error {
				called = true
				return nil
			})
			if err != nil || got != tc.want || called != tc.want {
				t.Fatalf("Reclaim = (reclaimed=%t, callback=%t, %v), want both %t", got, called, err, tc.want)
			}
		})
	}
}

func runTransferFailureAndObsoleteAttempts(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	fsStores := []string{"edge", "backup", "archive"}
	media := f.WithPolicy("edge", transferOriginalRules, fsStores, "derived")
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, _ := f.NewAsset(t, recordingID, "original", "edge", "attempts")
	transfer, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "archive", fsStores, fsStores, fsStores, 2)
	if err != nil || !ok {
		t.Fatalf("Prepare = (ok=%t, %v), want a reservation", ok, err)
	}
	for i := 0; i < 2; i++ {
		if err := media.RecordMediaAssetTransferFailure(ctx, transfer.CopyID, "copy failed"); err != nil {
			t.Fatalf("RecordMediaAssetTransferFailure: %v", err)
		}
	}
	if got, err := media.CountMediaAssetTransferExhausted(ctx, 2); err != nil || got != 1 {
		t.Fatalf("exhausted = (%d, %v), want 1", got, err)
	}
	if _, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "archive", fsStores, fsStores, fsStores, 2); err != nil || ok {
		t.Fatalf("Prepare exhausted copy = (ok=%t, %v), want no reservation", ok, err)
	}
	if err := media.ClearObsoleteMediaAssetTransferAttempts(ctx); err != nil {
		t.Fatalf("ClearObsoleteMediaAssetTransferAttempts with unchanged policy: %v", err)
	}
	if got, err := media.CountMediaAssetTransferExhausted(ctx, 2); err != nil || got != 1 {
		t.Fatalf("exhausted under unchanged policy = (%d, %v), want 1", got, err)
	}
	switched := f.WithPolicy("edge", []store.OriginalStoreRule{{Site: "tiered", Store: "backup"}}, fsStores, "derived")
	if err := switched.ClearObsoleteMediaAssetTransferAttempts(ctx); err != nil {
		t.Fatalf("ClearObsoleteMediaAssetTransferAttempts after policy switch: %v", err)
	}
	if got, err := media.CountMediaAssetTransferExhausted(ctx, 2); err != nil || got != 0 {
		t.Fatalf("exhausted after switching back = (%d, %v), want 0", got, err)
	}
	if again, ok, err := media.PrepareMediaAssetTransfer(ctx, assetID, "archive", fsStores, fsStores, fsStores, 2); err != nil || !ok || again.CopyID != transfer.CopyID {
		t.Fatalf("Prepare after switching back = (copy=%d, ok=%t, %v), want copy %d", again.CopyID, ok, err, transfer.CopyID)
	}
}

func runReclaimObsoletePendingTransferCopy(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	fsStores := []string{"edge", "backup", "archive"}
	media := f.WithPolicy("backup", transferOriginalRules, fsStores, "derived")
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, _ := f.NewAsset(t, recordingID, "original", "edge", "reclaim")
	copyID := f.AddCopy(t, assetID, "backup", "reclaim/backup.ts", "pending", 0)
	acquireLive(t, media, copyID, uuid.New())
	if got, err := media.MarkMediaAssetTransferObjectOwned(ctx, copyID, "backup", "reclaim/owned.ts"); err != nil || got != store.TransferObjectOwned {
		t.Fatalf("MarkMediaAssetTransferObjectOwned = (%v, %v), want owned", got, err)
	}
	want := store.MediaAssetCopyDeleteCandidate{CopyID: copyID, MediaAssetID: assetID, Store: "backup", RelPath: "reclaim/backup.ts", State: "pending", SizeBytes: 1, Kind: "original", Generation: 0}
	list, err := media.ListObsoletePendingMediaAssetTransferCopies(ctx, 10)
	if err != nil || len(list) != 1 || list[0] != want {
		t.Fatalf("ListObsoletePendingMediaAssetTransferCopies = (%+v, %v), want %+v", list, err, want)
	}
	currentPolicy := f.WithPolicy("backup", []store.OriginalStoreRule{{Site: "tiered", Store: "backup"}}, fsStores, "derived")
	if got, err := currentPolicy.ReclaimObsoletePendingMediaAssetTransferCopy(ctx, list[0], func([]string) error {
		t.Error("reclaim called the deleter after the copy became desired")
		return nil
	}); err != nil || got {
		t.Fatalf("Reclaim after policy switch = (%t, %v), want false", got, err)
	}

	unlinkErr := errors.New("unlink failed")
	failed := func(paths []string) error {
		if !slices.Equal(paths, []string{"reclaim/owned.ts"}) {
			t.Errorf("owned keys for reclaim = %q, want [reclaim/owned.ts]", paths)
		}
		return unlinkErr
	}
	if got, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(ctx, list[0], failed); !errors.Is(err, unlinkErr) || got {
		t.Fatalf("Reclaim with unlink error = (%t, %v), want false with unlink error", got, err)
	}
	if retry, err := media.ListObsoletePendingMediaAssetTransferCopies(ctx, 10); err != nil || !slices.Equal(retry, list) {
		t.Fatalf("retry candidates = (%+v, %v), want %+v", retry, err, list)
	}
	if got, err := media.ReclaimObsoletePendingMediaAssetTransferCopy(ctx, list[0], func(paths []string) error {
		if !slices.Equal(paths, []string{"reclaim/owned.ts"}) {
			t.Errorf("owned keys after unlink failure = %q, want [reclaim/owned.ts]", paths)
		}
		return nil
	}); err != nil || !got {
		t.Fatalf("Reclaim retry = (%t, %v), want true", got, err)
	}
	if got, ok, err := media.GetMediaAssetCopyForDeletion(ctx, list[0]); err != nil || !ok || got.State != "deleted" {
		t.Fatalf("copy after Reclaim = (%+v, %t, %v), want deleted copy", got, ok, err)
	}
	if _, ok, err := media.GetMediaAssetTransferProgressWriterGeneration(ctx, copyID); err != nil || ok {
		t.Fatalf("progress after Reclaim = (%t, %v), want none", ok, err)
	}
	if paths, err := media.ListMediaAssetTransferObjectsOwned(ctx, copyID, "backup"); err != nil || len(paths) != 0 {
		t.Fatalf("owned keys after Reclaim = (%q, %v), want none", paths, err)
	}
}

func runDeleteStaleTransferCopy(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	fsStores := []string{"edge", "backup", "archive"}
	media := f.WithPolicy("edge", transferOriginalRules, fsStores, "derived")
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, copyID := f.NewAsset(t, recordingID, "original", "edge", "prune")
	before := transferDatabaseClock(t, media).Add(-time.Second)
	f.AddCopy(t, assetID, "archive", "prune/archive.ts", "active", 0)
	after := transferDatabaseClock(t, media)
	if got, err := media.ListMediaAssetCopiesToDelete(ctx, before, 10); err != nil || len(got) != 0 {
		t.Fatalf("prunable before overlap = (%+v, %v), want none", got, err)
	}
	list, err := media.ListMediaAssetCopiesToDelete(ctx, after, 10)
	sourcePath := fmt.Sprintf("prune/%d/edge.ts", assetID)
	if err != nil || len(list) != 1 || list[0].CopyID != copyID || list[0].RelPath != sourcePath {
		t.Fatalf("prunable after overlap = (%+v, %v), want copy %d at %q", list, err, copyID, sourcePath)
	}
	job := list[0]
	job.RelPath = "stale-job-path.ts"
	if got, ok, err := media.GetMediaAssetCopyForDeletion(ctx, job); err != nil || !ok || got != list[0] {
		t.Fatalf("GetMediaAssetCopyForDeletion = (%+v, %t, %v), want %+v from current row", got, ok, err, list[0])
	}
	if marked, err := media.MarkMediaAssetCopyDeleted(ctx, job); err != nil || marked {
		t.Fatalf("MarkMediaAssetCopyDeleted on active copy = (%t, %v), want false", marked, err)
	}
	changed := f.WithPolicy("edge", []store.OriginalStoreRule{{Site: "tiered", Store: "backup"}}, fsStores, "derived")
	if path, marked, err := changed.MarkMediaAssetCopyDeleting(ctx, job, after); err != nil || marked || path != "" {
		t.Fatalf("MarkMediaAssetCopyDeleting without a desired copy = (%q, %t, %v), want no transition", path, marked, err)
	}
	if path, marked, err := media.MarkMediaAssetCopyDeleting(ctx, job, after); err != nil || !marked || path != sourcePath {
		t.Fatalf("MarkMediaAssetCopyDeleting = (%q, %t, %v), want deleting at %q", path, marked, err, sourcePath)
	}
	if marked, err := media.MarkMediaAssetCopyDeleted(ctx, job); err != nil || !marked {
		t.Fatalf("MarkMediaAssetCopyDeleted = (%t, %v), want true", marked, err)
	}
	if got, ok, err := media.GetMediaAssetCopyForDeletion(ctx, job); err != nil || !ok || got.State != "deleted" {
		t.Fatalf("pruned copy = (%+v, %t, %v), want deleted copy", got, ok, err)
	}
}

func runMarkCorruptDerivedCopy(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	media := f.Store
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, copyID := f.NewAsset(t, recordingID, "encoded", "edge", "corrupt")
	if got, err := media.MarkMediaAssetCopyCorrupt(ctx, recordingID, assetID, copyID, "edge"); err != nil || got {
		t.Fatalf("MarkMediaAssetCopyCorrupt without original = (%t, %v), want false", got, err)
	}
	originalID, originalCopyID := f.NewAsset(t, recordingID, "original", "edge", "corrupt")
	f.MarkCopyMissing(t, originalCopyID)
	if got, err := media.MarkMediaAssetCopyCorrupt(ctx, recordingID, assetID, copyID, "edge"); err != nil || got {
		t.Fatalf("MarkMediaAssetCopyCorrupt with missing original = (%t, %v), want false", got, err)
	}
	f.AddCopy(t, originalID, "backup", "corrupt/original-backup.ts", "active", 0)
	if got, err := media.MarkMediaAssetCopyCorrupt(ctx, recordingID, assetID, copyID, "edge"); err != nil || !got {
		t.Fatalf("MarkMediaAssetCopyCorrupt with original = (%t, %v), want true", got, err)
	}
	job := store.MediaAssetCopyDeleteCandidate{CopyID: copyID, MediaAssetID: assetID, Store: "edge"}
	if got, ok, err := media.GetMediaAssetCopyForDeletion(ctx, job); err != nil || !ok || got.State != "deleting" {
		t.Fatalf("corrupt copy = (%+v, %t, %v), want deleting copy", got, ok, err)
	}
	if got, err := media.MarkMediaAssetCopyDeleted(ctx, job); err != nil || !got {
		t.Fatalf("MarkMediaAssetCopyDeleted for corrupt copy = (%t, %v), want true", got, err)
	}
}

func runListOldRemoteDerivedCopies(t *testing.T, newStore NewStorageTransferStore) {
	f := newStore(t)
	ctx := t.Context()
	media := f.WithPolicy("edge", transferOriginalRules, []string{"edge", "backup", "archive"}, "derived")
	recordingID := f.NewRecording(t, "tiered", time.Time{})
	assetID, _ := f.NewAsset(t, recordingID, "encoded", "edge", "remote")
	f.AddCopy(t, assetID, "derived", "remote/current.ts", "active", 0)
	f.AddCopy(t, assetID, "old-remote", "remote/old.ts", "active", 0)
	f.AddCopy(t, assetID, "old-remote", "remote/non-current.ts", "active", 1)
	f.AddCopy(t, assetID, "pending-remote", "remote/pending.ts", "pending", 0)
	f.AddCopy(t, assetID, "unconfigured", "remote/unconfigured.ts", "active", 0)
	f.NewAsset(t, recordingID, "original", "old-remote", "remote")

	want := []store.DerivedRemoteCopyCount{{Store: "old-remote", Copies: 1}}
	got, err := media.ListActiveDerivedCopiesInNonDerivedRemote(ctx, []string{"derived", "old-remote", "pending-remote"})
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListActiveDerivedCopiesInNonDerivedRemote = (%+v, %v), want %+v", got, err, want)
	}
}
