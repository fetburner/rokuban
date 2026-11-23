package contracttest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

func preparePendingCopy(t *testing.T, scoped MediaStoreScope, input store.PendingMediaAssetCopy) {
	t.Helper()
	if err := scoped.Publisher.PreparePendingMediaAssetCopy(t.Context(), input); err != nil {
		t.Fatalf("PreparePendingMediaAssetCopy(%+v): %v", input, err)
	}
}

func requireLiveCopy(t *testing.T, scoped MediaStoreScope, path string, want bool) {
	t.Helper()
	if got, err := scoped.Publisher.HasLiveMediaAssetCopy(t.Context(), path); err != nil || got != want {
		t.Fatalf("HasLiveMediaAssetCopy(%q) = %v, %v; want %v, nil", path, got, err, want)
	}
}

func listPendingCopies(t *testing.T, scoped MediaStoreScope, before time.Time, limit int32) []store.MediaDeleteAsset {
	t.Helper()
	rows, err := scoped.Reclaim.ListAgedPendingMediaAssetCopies(t.Context(), before, limit)
	if err != nil {
		t.Fatalf("ListAgedPendingMediaAssetCopies: %v", err)
	}
	return rows
}

func runPendingCopyPreparation(t *testing.T, f MediaFixture) {
	primary := f.OpenStore("tokyo")
	otherStore := f.OpenStore("osaka")
	ctx := t.Context()
	input := store.PendingMediaAssetCopy{
		RecordingID: f.NewRecording(t), Kind: "thumbnail", RelPath: "thumbnails/test.g5.jpg",
		SizeBytes: 31, DerivedGeneration: 5,
	}
	requireLiveCopy(t, primary, input.RelPath, false)
	preparePendingCopy(t, primary, input)
	requireLiveCopy(t, primary, input.RelPath, true)
	requireLiveCopy(t, otherStore, input.RelPath, false)
	if _, err := f.Serving.GetThumbnailMediaAssetForServing(ctx, input.RecordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serving before publication = %v, want ErrNotFound", err)
	}

	before := time.Now().Add(time.Hour)
	rows := listPendingCopies(t, primary, before, 10)
	if len(rows) != 1 {
		t.Fatalf("pending after preparation = %+v, want one copy", rows)
	}
	// 再試行は同じキーとコピー ID を保つ。同じ置き場・世代の別キーは生きたコピーを増やせない。
	preparePendingCopy(t, primary, input)
	second := input
	second.RelPath = "thumbnails/second.g5.jpg"
	if err := primary.Publisher.PreparePendingMediaAssetCopy(ctx, second); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second live copy in the same store and generation = %v, want ErrConflict", err)
	}
	requireLiveCopy(t, primary, second.RelPath, false)
	if got := listPendingCopies(t, primary, before, 10); !slices.Equal(got, rows) {
		t.Fatalf("pending after replay and conflict = %+v, want unchanged %+v", got, rows)
	}
	preparePendingCopy(t, otherStore, input)
	requireLiveCopy(t, otherStore, input.RelPath, true)

	fresh := input
	fresh.ReserveNewGeneration = true
	for _, gen := range []int64{4, 5} {
		fresh.DerivedGeneration = gen
		if err := primary.Publisher.PreparePendingMediaAssetCopy(ctx, fresh); !errors.Is(err, store.ErrGenerationAdvanced) {
			t.Errorf("reserving generation %d = %v, want ErrGenerationAdvanced", gen, err)
		}
	}
	fresh.DerivedGeneration = 6
	fresh.RelPath = "thumbnails/test.g6.jpg"
	preparePendingCopy(t, primary, fresh)
	requireLiveCopy(t, primary, fresh.RelPath, true)

	// 回収で退役した行は一意性の対象外。同世代の新しいキーで置き換えられる。
	freshCopies := listPendingCopies(t, primary, before, 10)
	freshIndex := slices.IndexFunc(freshCopies, func(copy store.MediaDeleteAsset) bool {
		return copy.RelPath == "thumbnails/test.g6.jpg"
	})
	if len(freshCopies) != 2 || freshIndex < 0 {
		t.Fatalf("pending generations = %+v, want original and generation 6", freshCopies)
	}
	reclaimed, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, freshCopies[freshIndex].ID, before, func([]string) error { return nil })
	if err != nil || !reclaimed {
		t.Fatalf("reclaim generation 6 = %v, %v; want true, nil", reclaimed, err)
	}
	fresh.RelPath = "thumbnails/replacement.g6.jpg"
	if err := primary.Publisher.PreparePendingMediaAssetCopy(ctx, fresh); !errors.Is(err, store.ErrGenerationAdvanced) {
		t.Fatalf("reserving retired generation = %v, want ErrGenerationAdvanced", err)
	}
	fresh.ReserveNewGeneration = false
	preparePendingCopy(t, primary, fresh)
	requireLiveCopy(t, primary, fresh.RelPath, true)
}

func runPendingCopyTombstone(t *testing.T, f MediaFixture) {
	primary := f.OpenStore("tokyo")
	ctx := t.Context()
	f.NewAsset(t, f.NewRecording(t), "original", nil, "originals/reserved-after-delete.ts", "deleted")
	profile := "h264"
	input := store.PendingMediaAssetCopy{
		RecordingID: f.NewRecording(t), Kind: "encoded", Profile: &profile,
		RelPath: "originals/reserved-after-delete.ts", SizeBytes: 1, DerivedGeneration: 1,
	}
	// 削除済みキーは所有確認で見えなくても、別の資産に再割り当てできない。
	requireLiveCopy(t, primary, input.RelPath, false)
	if err := primary.Publisher.PreparePendingMediaAssetCopy(ctx, input); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("PreparePendingMediaAssetCopy at tombstoned path = %v, want ErrConflict", err)
	}
	requireLiveCopy(t, primary, input.RelPath, false)
	if got := listPendingCopies(t, primary, time.Now().Add(time.Hour), 10); len(got) != 0 {
		t.Fatalf("pending copies after tombstone conflict = %+v, want none", got)
	}
	input.RelPath = "encoded/replacement.g1.mp4"
	preparePendingCopy(t, primary, input)
}

func runPendingCopyReclaim(t *testing.T, f MediaFixture) {
	primary := f.OpenStore("tokyo")
	otherStore := f.OpenStore("osaka")
	ctx := t.Context()
	id := f.NewRecording(t)
	original := f.NewAsset(t, id, "original", nil, "originals/active.ts", "active")
	f.NewAsset(t, f.NewRecording(t), "original", nil, "originals/pending.ts", "pending")
	f.NewAsset(t, f.NewRecording(t), "original", nil, "originals/deleting.ts", "deleting")
	requireLiveCopy(t, primary, "originals/active.ts", true)
	requireLiveCopy(t, primary, "originals/deleting.ts", true)
	first := store.PendingMediaAssetCopy{RecordingID: id, Kind: "thumbnail", RelPath: "thumbnails/aged.g3.jpg", SizeBytes: 31, DerivedGeneration: 3}
	preparePendingCopy(t, primary, first)
	second := store.PendingMediaAssetCopy{RecordingID: f.NewRecording(t), Kind: "seek_tiles", RelPath: "thumbnails/other-aged.g2.jpg", SizeBytes: 63, DerivedGeneration: 2}
	preparePendingCopy(t, primary, second)
	otherInput := first
	otherInput.RecordingID = f.NewRecording(t)
	preparePendingCopy(t, otherStore, otherInput)

	now := time.Now().UTC().Truncate(time.Second)
	all := listPendingCopies(t, primary, now.Add(time.Hour), 10)
	if len(all) != 2 {
		t.Fatalf("pending derived copies = %+v, want both primary copies", all)
	}
	if all[0].RelPath != "thumbnails/aged.g3.jpg" {
		all[0], all[1] = all[1], all[0]
	}
	old, atCutoff := all[0], all[1]
	if old.RelPath != "thumbnails/aged.g3.jpg" || atCutoff.RelPath != "thumbnails/other-aged.g2.jpg" {
		t.Fatalf("pending derived copies = %+v, want aged thumbnail and boundary seek tiles", all)
	}
	if old.ID <= 0 || old.MediaAssetID <= 0 || old.MediaAssetID == original || old.RecordingID != id ||
		old.Kind != "thumbnail" || old.SizeBytes != 31 || old.Generation != 3 {
		t.Fatalf("aged pending payload = %+v, want a physical copy of recording %d's 31-byte thumbnail at generation 3", old, id)
	}
	f.SetCopyCreatedAt(t, old.ID, now.Add(-time.Hour))
	f.SetCopyCreatedAt(t, atCutoff.ID, now)
	other := listPendingCopies(t, otherStore, now.Add(time.Hour), 10)
	if len(other) != 1 || other[0].RelPath != "thumbnails/aged.g3.jpg" || other[0].ID == old.ID {
		t.Fatalf("other store pending = %+v, want its own physical copy", other)
	}
	f.SetCopyCreatedAt(t, other[0].ID, now.Add(-time.Hour))
	if rows := listPendingCopies(t, primary, now, 10); len(rows) != 1 || rows[0] != old {
		t.Fatalf("pending strictly before cutoff = %+v, want only %+v", rows, old)
	}
	if rows := listPendingCopies(t, primary, now.Add(time.Hour), 1); len(rows) != 1 || !slices.Contains(all, rows[0]) {
		t.Fatalf("limited pending page = %+v, want one of %+v", rows, all)
	}

	skip := func(paths []string) error {
		t.Errorf("reclaim called deleteObject for an ineligible copy with paths %v", paths)
		return nil
	}
	for _, copyID := range []int64{atCutoff.ID, other[0].ID} {
		reclaimed, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, copyID, now, skip)
		if reclaimed || err != nil {
			t.Errorf("reclaim ineligible copy %d = %v, %v; want false, nil", copyID, reclaimed, err)
		}
	}
	wantErr := errors.New("object removal failed")
	calls := 0
	reclaimed, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, old.ID, now, func(paths []string) error {
		calls++
		if paths != nil {
			t.Errorf("normal pending copy passed paths = %#v, want nil", paths)
		}
		return wantErr
	})
	if reclaimed || !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("failed reclaim = %v, %v; calls = %d, want false, callback error and one call", reclaimed, err, calls)
	}
	if rows := listPendingCopies(t, primary, now, 10); len(rows) != 1 || rows[0] != old {
		t.Fatalf("pending after failed delete = %+v, want %+v", rows, old)
	}
	requireLiveCopy(t, primary, first.RelPath, true)
	reclaimed, err = primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, old.ID, now, func(paths []string) error {
		calls++
		if paths != nil {
			t.Errorf("normal pending retry passed paths = %#v, want nil", paths)
		}
		return nil
	})
	if !reclaimed || err != nil || calls != 2 {
		t.Fatalf("successful retry = %v, %v; calls = %d, want true, nil and one additional call", reclaimed, err, calls)
	}
	requireLiveCopy(t, primary, first.RelPath, false)
	requireLiveCopy(t, otherStore, first.RelPath, true)
	if rows := listPendingCopies(t, primary, now, 10); len(rows) != 0 {
		t.Fatalf("pending after successful deletion = %+v, want none", rows)
	}
	if reclaimed, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, old.ID, now, skip); reclaimed || err != nil {
		t.Errorf("reclaim retired copy = %v, %v; want false, nil", reclaimed, err)
	}

	input := store.ThumbnailPublishInput{RecordingID: id, InputAssetID: original, RelPath: "thumbnails/published.g4.jpg", SizeBytes: 1, DerivedGeneration: 4, PendingCopy: true}
	preparePendingCopy(t, primary, store.PendingMediaAssetCopy{RecordingID: id, Kind: "thumbnail", RelPath: input.RelPath, SizeBytes: 1, DerivedGeneration: 4})
	rows := listPendingCopies(t, primary, now.Add(time.Hour), 10)
	publishIndex := slices.IndexFunc(rows, func(copy store.MediaDeleteAsset) bool {
		return copy.RelPath == "thumbnails/published.g4.jpg"
	})
	if len(rows) != 2 || publishIndex < 0 {
		t.Fatalf("pending before publication = %+v, want boundary copy and thumbnail", rows)
	}
	publishCalls := 0
	if published, err := primary.Publisher.PublishThumbnail(ctx, input, func() error { publishCalls++; return nil }); !published || err != nil || publishCalls != 1 {
		t.Fatalf("publish before reclaim = %v, %v; calls %d, want true, nil and one call", published, err, publishCalls)
	}
	if reclaimed, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, rows[publishIndex].ID, now.Add(time.Hour), skip); reclaimed || err != nil {
		t.Errorf("reclaim already published copy = %v, %v; want false, nil", reclaimed, err)
	}
	if pending := listPendingCopies(t, primary, now.Add(time.Hour), 10); len(pending) != 1 || pending[0] != atCutoff {
		t.Errorf("pending after publication = %+v, want only %+v", pending, atCutoff)
	}
	requireLiveCopy(t, primary, "thumbnails/published.g4.jpg", true)
	got, err := f.Serving.GetThumbnailMediaAssetForServing(ctx, id)
	if err != nil || got.RelPath != "thumbnails/published.g4.jpg" {
		t.Errorf("serving after skipped reclaim = %+v, %v; want published thumbnail", got, err)
	}

	// 新しい世代の準備だけでは、読み手が見る現行世代とパスは変わらない。
	preparePendingCopy(t, primary, store.PendingMediaAssetCopy{
		RecordingID: id, Kind: "thumbnail", RelPath: "thumbnails/replacement.g5.jpg", SizeBytes: 2, DerivedGeneration: 5,
	})
	got, err = f.Serving.GetThumbnailMediaAssetForServing(ctx, id)
	if err != nil || got.RelPath != "thumbnails/published.g4.jpg" || got.SizeBytes != 1 {
		t.Errorf("serving after preparing replacement = %+v, %v; want existing one-byte thumbnail", got, err)
	}
}

func runPendingCopyTransferOwnership(t *testing.T, f MediaFixture) {
	primary := f.OpenStore("tokyo")
	before := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		path  string
		owned []string
	}{
		{"thumbnails/no-ownership.jpg", []string{}},
		{"thumbnails/assigned.jpg", []string{"thumbnails/owned-object.jpg"}},
	} {
		preparePendingCopy(t, primary, store.PendingMediaAssetCopy{
			RecordingID: f.NewRecording(t), Kind: "thumbnail", RelPath: tc.path, SizeBytes: 1,
		})
		rows := listPendingCopies(t, primary, before, 10)
		if len(rows) != 1 || rows[0].RelPath != tc.path {
			t.Fatalf("pending transfer copies = %+v, want one at %q", rows, tc.path)
		}
		row := rows[0]
		// 同じ世代の別のコピーが転送の証拠になる。予約のキー自体には所有記録が無い。
		f.AddCopy(t, row.MediaAssetID, "osaka", tc.path)
		for _, path := range tc.owned {
			f.SeedOwnedTransferPath(t, row.ID, path)
		}
		calls := 0
		applied, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(t.Context(), row.ID, before, func(paths []string) error {
			calls++
			if paths == nil || !slices.Equal(paths, tc.owned) {
				t.Errorf("owned paths for %q = %#v, want %#v", tc.path, paths, tc.owned)
			}
			return nil
		})
		if err != nil || !applied || calls != 1 {
			t.Fatalf("transfer reclaim = %v, %v; calls = %d, want true, nil and one call", applied, err, calls)
		}
	}
}

func runPendingCopyPublicationWaits(t *testing.T, f MediaFixture) {
	primary := f.OpenStore("tokyo")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	id := f.NewRecording(t)
	original := f.NewAsset(t, id, "original", nil, "originals/source.ts", "active")
	preparePendingCopy(t, primary, store.PendingMediaAssetCopy{RecordingID: id, Kind: "thumbnail", RelPath: "thumbnails/race.jpg", SizeBytes: 1})
	before := time.Now().Add(time.Hour)
	rows := listPendingCopies(t, primary, before, 10)
	if len(rows) != 1 {
		t.Fatalf("pending copies before reclaim = %+v, want one", rows)
	}
	type result struct {
		applied bool
		err     error
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	finishDelete := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	defer wg.Wait()
	defer finishDelete()
	reclaim := make(chan result, 1)
	wg.Go(func() {
		applied, err := primary.Reclaim.ReclaimPendingMediaAssetCopy(ctx, rows[0].ID, before, func([]string) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		reclaim <- result{applied, err}
	})
	select {
	case <-started:
	case got := <-reclaim:
		t.Fatalf("reclaim returned before delete callback = %+v", got)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	published := make(chan result, 1)
	publishCalls := 0
	wg.Go(func() {
		applied, err := primary.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
			RecordingID: id, InputAssetID: original, RelPath: "thumbnails/race.jpg", SizeBytes: 1, PendingCopy: true,
		}, func() error { publishCalls++; return nil })
		published <- result{applied, err}
	})
	f.WaitForPublishBlocked(t)
	finishDelete()
	if got := <-reclaim; !got.applied || got.err != nil {
		t.Fatalf("reclaim = %+v, want true, nil", got)
	}
	if got := <-published; got.applied || !errors.Is(got.err, store.ErrPendingMediaAssetCopyMissing) || publishCalls != 0 {
		t.Fatalf("publish after reclaim = %+v; calls = %d, want missing pending copy and no callback", got, publishCalls)
	}
	if _, err := f.Serving.GetThumbnailMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("serving reclaimed thumbnail = %v, want ErrNotFound", err)
	}
}
