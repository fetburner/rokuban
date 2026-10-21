package contracttest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// serializationGrace は、publish 関数の中で相手の書き手が先へ進まないことを見張る時間である。
const serializationGrace = 300 * time.Millisecond

// runPublishCommitFailure は publish が成功した後に commit が失敗するケースを検査する。
// store はエラーを返し、publish は 1 回だけ呼ばれ、公開した行は残らない。
func runPublishCommitFailure(t *testing.T, newFixture NewMediaFixture) {
	ctx := context.Background()
	lose := func(f MediaFixture, calls *int) func() error {
		return func() error {
			*calls++
			f.LoseTransactionDuringPublish(t)
			return nil
		}
	}

	t.Run("original", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		expected := int64(2000)
		f.SetIngestProgress(t, id, 1000, &expected)
		calls := 0
		err := f.Publisher.CommitOriginal(ctx, store.CommitOriginalInput{
			RecordingID: id, RelPath: fmt.Sprintf("sites/%s/%d/commit-failure.m2ts", f.Site, id), SizeBytes: 1,
		}, func(store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
			return store.EncodePolicySnapshot{KeepOriginal: "always", EncodeProfiles: []string{}}, nil
		}, lose(f, &calls))
		if err == nil || calls != 1 {
			t.Fatalf("CommitOriginal = %v with %d publish calls; want an error after one publish", err, calls)
		}
		if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("original after commit failure error = %v, want ErrNotFound", err)
		}
		if _, ok := f.HasEncodePolicy(t, id); ok {
			t.Fatal("encode policy row survived the commit failure")
		}
		if !f.HasIngestProgress(t, id) {
			t.Fatal("ingest progress deletion survived the commit failure")
		}
	})
	t.Run("encoded", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		f.SetEncodeProfiles(t, id, []string{"h264"})
		attempt := f.BeginEncodeAttempt(t, id, "h264")
		calls := 0
		published, err := f.Publisher.PublishEncoded(ctx, store.EncodedPublishInput{
			RecordingID: id, Profile: "h264", Attempt: attempt,
			RelPath: fmt.Sprintf("sites/%s/%d/commit-failure.mp4", f.Site, id), SizeBytes: 1,
		}, lose(f, &calls))
		if err == nil || published || calls != 1 {
			t.Fatalf("PublishEncoded = (%v, %v) with %d publish calls; want an error after one publish", published, err, calls)
		}
		if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, "h264"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("encoded asset after commit failure error = %v, want ErrNotFound", err)
		}
	})
	t.Run("thumbnail", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		originalID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
		calls := 0
		published, err := f.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
			RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d.jpg", id), SizeBytes: 10, InputAssetID: originalID,
		}, lose(f, &calls))
		if err == nil || published || calls != 1 {
			t.Fatalf("PublishThumbnail = (%v, %v) with %d publish calls; want an error after one publish", published, err, calls)
		}
		if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("thumbnail after commit failure = (%v, %v), want (false, nil)", has, err)
		}
	})
	t.Run("seek tiles", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
		calls := 0
		published, err := f.Publisher.PublishSeekTiles(ctx, store.SeekTilesPublishInput{
			RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d_tiles.jpg", id), SizeBytes: 20,
		}, lose(f, &calls))
		if err == nil || published || calls != 1 {
			t.Fatalf("PublishSeekTiles = (%v, %v) with %d publish calls; want an error after one publish", published, err, calls)
		}
		if has, err := f.Reconcile.HasActiveSeekTilesMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("seek tiles after commit failure = (%v, %v), want (false, nil)", has, err)
		}
	})
}

// runOriginalRelPathConflict は、同じ rel_path の active な原本が既にあると ErrConflict を返し、
// publish を呼ばないことを検査する。
func runOriginalRelPathConflict(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	owner := f.NewRecording(t)
	relPath := fmt.Sprintf("sites/%s/%d/conflict.m2ts", f.Site, owner)
	f.NewAsset(t, owner, "original", nil, relPath, "active")
	id := f.NewRecording(t)
	expected := int64(2000)
	f.SetIngestProgress(t, id, 1000, &expected)
	calls := 0
	err := f.Publisher.CommitOriginal(ctx, store.CommitOriginalInput{
		RecordingID: id, RelPath: relPath, SizeBytes: 1,
	}, func(store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
		return store.EncodePolicySnapshot{KeepOriginal: "always", EncodeProfiles: []string{}}, nil
	}, func() error { calls++; return nil })
	if !errors.Is(err, store.ErrConflict) || calls != 0 {
		t.Fatalf("CommitOriginal on an occupied rel_path = %v with %d publish calls; want ErrConflict without publishing", err, calls)
	}
	if _, ok := f.HasEncodePolicy(t, id); ok {
		t.Fatal("encode policy row was frozen for a rejected original")
	}
	if !f.HasIngestProgress(t, id) {
		t.Fatal("ingest progress was deleted for a rejected original")
	}
}

// runDerivedPublicationSkips は thumbnail と seek tiles が、ごみ箱の録画と既に active な行を公開しないことを検査する。
func runDerivedPublicationSkips(t *testing.T, newFixture NewMediaFixture) {
	ctx := context.Background()
	thumbnail := func(f MediaFixture, id, originalID int64, calls *int) (bool, error) {
		return f.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
			RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d.jpg", id), SizeBytes: 10, InputAssetID: originalID,
		}, func() error { *calls++; return nil })
	}
	seekTiles := func(f MediaFixture, id int64, calls *int) (bool, error) {
		return f.Publisher.PublishSeekTiles(ctx, store.SeekTilesPublishInput{
			RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d_tiles.jpg", id), SizeBytes: 20,
		}, func() error { *calls++; return nil })
	}

	t.Run("trashed recording", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		originalID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
		f.SetRecordingTrashed(t, id)
		calls := 0
		if published, err := thumbnail(f, id, originalID, &calls); err != nil || published || calls != 0 {
			t.Fatalf("PublishThumbnail on a trashed recording = (%v, %v) with %d publish calls; want (false, nil) and none", published, err, calls)
		}
		if published, err := seekTiles(f, id, &calls); err != nil || published || calls != 0 {
			t.Fatalf("PublishSeekTiles on a trashed recording = (%v, %v) with %d publish calls; want (false, nil) and none", published, err, calls)
		}
		if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("thumbnail of a trashed recording = (%v, %v), want (false, nil)", has, err)
		}
		if has, err := f.Reconcile.HasActiveSeekTilesMediaAsset(ctx, id); err != nil || has {
			t.Fatalf("seek tiles of a trashed recording = (%v, %v), want (false, nil)", has, err)
		}
	})
	t.Run("existing active assets", func(t *testing.T) {
		f := newFixture(t)
		id := f.NewRecording(t)
		originalID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
		f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
		f.NewAsset(t, id, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", id), "active")
		calls := 0
		if published, err := thumbnail(f, id, originalID, &calls); err != nil || published || calls != 0 {
			t.Fatalf("PublishThumbnail over an active thumbnail = (%v, %v) with %d publish calls; want (false, nil) and none", published, err, calls)
		}
		if published, err := seekTiles(f, id, &calls); err != nil || published || calls != 0 {
			t.Fatalf("PublishSeekTiles over active seek tiles = (%v, %v) with %d publish calls; want (false, nil) and none", published, err, calls)
		}
	})
}

// runEncodedCutSerializesWithChapterEdits は、cut 版の公開が同じ録画のチャプター編集と直列化することを検査する。
// publish 関数の中で編集を始め、公開の tx が終わるまで編集が先へ進まないことを見る。
func runEncodedCutSerializesWithChapterEdits(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	spans := []chapters.Span{{StartMs: 0, EndMs: 10_000, Cut: true}}
	f.SetChapterSpans(t, id, spans)
	keep := chapters.KeepRanges(chapters.Derive(true, spans, nil, 60_000))
	f.SetEncodeProfiles(t, id, []string{"cut"})
	attempt := f.BeginEncodeAttempt(t, id, "cut")
	snapshot, err := f.Chapters.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading chapters: %v", err)
	}

	edit := make(chan error, 1)
	editFinishedInPublish := false
	published, err := f.Publisher.PublishEncoded(ctx, store.EncodedPublishInput{
		RecordingID: id, Profile: "cut", Attempt: attempt, Cut: true, KeepRanges: keep,
		RelPath: fmt.Sprintf("sites/%s/%d/cut.mp4", f.Site, id), SizeBytes: 1,
	}, func() error {
		go func() {
			edit <- f.Chapters.Replace(ctx, id, snapshot.Version, []chapters.Span{{StartMs: 0, EndMs: 20_000, Cut: true}}, false)
		}()
		select {
		case <-edit:
			editFinishedInPublish = true
		case <-time.After(serializationGrace):
		}
		return nil
	})
	if err != nil || !published {
		t.Fatalf("PublishEncoded(cut) = (%v, %v), want published", published, err)
	}
	if editFinishedInPublish {
		t.Fatal("chapter edit finished while the cut publication was in progress, want it to wait")
	}
	select {
	case err := <-edit:
		if err != nil {
			t.Fatalf("chapter edit after the publication = %v, want success", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chapter edit did not finish after the publication committed")
	}
}

// runEncodedWaitsForOpenRemoval は、版の削除が方針の行を持っている間は encoded の公開が待ち、
// 削除が commit された後の desired で再確認して公開しないことを検査する。
func runEncodedWaitsForOpenRemoval(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SetEncodeProfiles(t, id, []string{"h264", "mobile"})
	attempt := f.BeginEncodeAttempt(t, id, "mobile")

	commit := f.BeginEncodedRemoval(t, id, "mobile")
	type result struct {
		published bool
		err       error
	}
	done := make(chan result, 1)
	publishCalls := 0
	go func() {
		published, err := f.Publisher.PublishEncoded(ctx, store.EncodedPublishInput{
			RecordingID: id, Profile: "mobile", Attempt: attempt,
			RelPath: fmt.Sprintf("sites/%s/%d/mobile.mp4", f.Site, id), SizeBytes: 1,
		}, func() error { publishCalls++; return nil })
		done <- result{published, err}
	}()
	select {
	case r := <-done:
		commit()
		t.Fatalf("PublishEncoded returned (%v, %v) while a removal held the policy, want it to wait", r.published, r.err)
	case <-time.After(serializationGrace):
	}
	commit()
	select {
	case r := <-done:
		if r.err != nil || r.published || publishCalls != 0 {
			t.Fatalf("PublishEncoded after the removal = (%v, %v) with %d publish calls; want (false, nil) and none", r.published, r.err, publishCalls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishEncoded did not finish after the removal committed")
	}
}

// runEncodedCutWaitsForChapterEdit は、チャプター編集が録画の行を持っている間は cut 版の公開が待ち、
// 編集が commit された後の区間で再確認して公開しないことを検査する。
func runEncodedCutWaitsForChapterEdit(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	spans := []chapters.Span{{StartMs: 0, EndMs: 10_000, Cut: true}}
	f.SetChapterSpans(t, id, spans)
	keep := chapters.KeepRanges(chapters.Derive(true, spans, nil, 60_000))
	f.SetEncodeProfiles(t, id, []string{"cut"})
	attempt := f.BeginEncodeAttempt(t, id, "cut")

	commit := f.BeginChapterEdit(t, id, []chapters.Span{{StartMs: 0, EndMs: 20_000, Cut: true}})
	type result struct {
		published bool
		err       error
	}
	done := make(chan result, 1)
	publishCalls := 0
	go func() {
		published, err := f.Publisher.PublishEncoded(ctx, store.EncodedPublishInput{
			RecordingID: id, Profile: "cut", Attempt: attempt, Cut: true, KeepRanges: keep,
			RelPath: fmt.Sprintf("sites/%s/%d/cut.mp4", f.Site, id), SizeBytes: 1,
		}, func() error { publishCalls++; return nil })
		done <- result{published, err}
	}()
	select {
	case r := <-done:
		commit()
		t.Fatalf("PublishEncoded(cut) returned (%v, %v) while a chapter edit held the recording, want it to wait", r.published, r.err)
	case <-time.After(serializationGrace):
	}
	commit()
	select {
	case r := <-done:
		if r.err != nil || r.published || publishCalls != 0 {
			t.Fatalf("PublishEncoded(cut) after the edit = (%v, %v) with %d publish calls; want (false, nil) and none", r.published, r.err, publishCalls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishEncoded(cut) did not finish after the chapter edit committed")
	}
}
