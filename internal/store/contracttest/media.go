// Package contracttest は store の契約を実装に対して検査する統合テストを持つ。
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

// MediaFixture はメディア契約の実装と状態を作るフックをまとめる。
type MediaFixture struct {
	Serving   store.MediaServing
	Reconcile store.MediaReconcile
	Precheck  store.IngestPrecheck
	Storage   store.StorageObservation
	Reader    store.MediaReader
	Query     store.RecordingQuery
	Catalog   store.Catalog
	Publisher store.MediaPublish
	Progress  store.EncodeProgressReporter
	// Chapters は、公開と直列化する書き手として使う。
	Chapters store.ChapterEdits

	// Site は NewRecording が作る録画の site。
	Site string

	NewRecording              func(t *testing.T) int64
	SetRecordingEncodeTimes   func(t *testing.T, recordingID int64, startedAt, endedAt *time.Time)
	SetRecordingPurged        func(t *testing.T, recordingID int64)
	SetEncodeProfiles         func(t *testing.T, recordingID int64, profiles []string)
	BeginEncodeAttempt        func(t *testing.T, recordingID int64, profile string) int32
	FailEncodeAttempt         func(t *testing.T, recordingID int64, profile string, attemptCount int32)
	SetRecordingDropStats     func(t *testing.T, assetID int64, stats []store.RecordingDropStat)
	SetRecordingDropPositions func(t *testing.T, assetID int64, positions []store.RecordingDropPosition)
	// StartEncodeProgressListener は LISTEN を確立し、通知を store.EncodeProgress として返す関数を返す。
	StartEncodeProgressListener   func(t *testing.T) func() store.EncodeProgress
	SetIngestProgress             func(t *testing.T, recordingID int64, written int64, expected *int64)
	NewInPlaceInput               func(t *testing.T, relPath string) store.InPlaceInput
	NewAsset                      func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64
	SetAssetState                 func(t *testing.T, assetID int64, state string)
	SetMediaAssetKeepRanges       func(t *testing.T, assetID int64, keep []chapters.Range)
	SetEncodedAssetRebuildRequest func(t *testing.T, recordingID int64, profile string, keep []chapters.Range)
	// BeginChapterEdit はチャプター編集 worker と同じく録画の行をロックして区間を書く tx を開き、
	// commit する関数を返す。commit までロックを保持する。
	BeginChapterEdit func(t *testing.T, recordingID int64, spans []chapters.Span) (commit func())
	// BeginEncodedRemoval は版の削除と同じく録画の行と方針の行をロックして profile を desired から外す tx を開き、
	// commit する関数を返す。commit までロックを保持する。
	BeginEncodedRemoval func(t *testing.T, recordingID int64, profile string) (commit func())
	// LoseTransactionDuringPublish は publish 関数の中から呼ばれ、store が開いている tx の接続を落とす。
	// 呼んだ後に publish が成功を返すと、publish の成功の後に commit だけが失敗する。
	LoseTransactionDuringPublish func(t *testing.T)
	// SetRecordingRemovalTimes は録画の deleted_at・purged_at・superseded_at を指定した値に書く。
	// nil は NULL にする。
	SetRecordingRemovalTimes func(t *testing.T, recordingID int64, deletedAt, purgedAt, supersededAt *time.Time)
	// SetRecordingTrashed は録画をごみ箱に入れる（deleted_at を埋める）。
	SetRecordingTrashed func(t *testing.T, recordingID int64)
	// NewRecordSync は録画に対応する mirakc record の同期行を作る。
	NewRecordSync func(t *testing.T, recordingID int64, site, recordID, status string)
	// SetCMRanges は録画に CM 検出の結果（CM の区間）を書く。
	SetCMRanges func(t *testing.T, recordingID int64, ranges []chapters.Range)
	// SetChapterSpans は録画のチャプターをユーザー所有にし、区間を書く。
	SetChapterSpans func(t *testing.T, recordingID int64, spans []chapters.Span)
	// SetOrphanFirstSeenAgo は孤児候補の first_seen を DB の now() から ago 前へずらす。
	SetOrphanFirstSeenAgo func(t *testing.T, relPath string, ago time.Duration)
	// SetMissingFirstSeenAgo は実体無し候補の first_seen を DB の now() から ago 前へずらす。
	SetMissingFirstSeenAgo func(t *testing.T, assetID int64, ago time.Duration)
}

// NewMediaFixture は実在の契約実装とフィクスチャを作る。
type NewMediaFixture func(t *testing.T) MediaFixture

// RunMedia は実在のメディア store 実装に対して読み取り・配信・名前空間・監査の契約を実行する。
func RunMedia(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("current cut keep is derived from owned chapters", func(t *testing.T) { runCurrentCutKeep(t, newFixture) })
	t.Run("recording encode times preserve nullable timestamps", func(t *testing.T) { runRecordingEncodeTimes(t, newFixture) })
	t.Run("encode notify subject includes purged recordings", func(t *testing.T) { runRecordingNotifySubject(t, newFixture) })
	t.Run("ingest precheck recognizes committed originals in every state", func(t *testing.T) { runHasOriginalMediaAsset(t, newFixture) })
	t.Run("live rel_path precheck matches the partial unique index", func(t *testing.T) { runLiveMediaAssetRecordingID(t, newFixture) })
	t.Run("media jobs read active inputs through their contract", func(t *testing.T) { runMediaJobInputs(t, newFixture) })
	t.Run("encode planning reads asset and request state through its contract", func(t *testing.T) { runMediaEncodePlanning(t, newFixture) })
	t.Run("encode queue filters desired profiles and attempts", func(t *testing.T) { runEncodeQueue(t, newFixture) })
	t.Run("drop stats are scoped to the target recording", func(t *testing.T) { runRecordingDropStats(t, newFixture) })
	t.Run("drop positions are scoped to the target recording", func(t *testing.T) { runRecordingDropPositions(t, newFixture) })
	t.Run("encode progress reaches the listener", func(t *testing.T) { runEncodeProgressReport(t, newFixture) })
	t.Run("serving returns only active assets", func(t *testing.T) { runMediaServingActiveOnly(t, newFixture) })
	t.Run("serving covers thumbnails and seek tiles", func(t *testing.T) { runMediaServingDerived(t, newFixture) })
	t.Run("original VOD target is scoped to the recording's site", func(t *testing.T) { runMediaOriginalVODTarget(t, newFixture) })
	t.Run("recording HLS original target shares the original VOD visibility predicate except the finished status", func(t *testing.T) { runMediaRecordingHLSTarget(t, newFixture) })
	t.Run("recording removal state reports deleted, purged and superseded times", func(t *testing.T) { runMediaRecordingRemovalState(t, newFixture) })
	t.Run("chase target requires a record and a recording outside the trash", func(t *testing.T) { runMediaChaseTarget(t, newFixture) })
	t.Run("namespace guard accepts site paths and rejects malformed ones", func(t *testing.T) { runMediaNamespaceMalformed(t, newFixture) })
	t.Run("namespace guard rejects unprefixed originals and encoded files", func(t *testing.T) { runMediaNamespaceUnprefixed(t, newFixture) })
	t.Run("namespace guard leaves thumbnails and seek tiles to their own namespace", func(t *testing.T) { runMediaNamespaceDerived(t, newFixture) })
	t.Run("audit lists every media asset path including reserved namespaces", func(t *testing.T) { runMediaAuditPaths(t, newFixture) })
	t.Run("thumbnail reselection candidates carry typed timeline and inputs", func(t *testing.T) { runMediaThumbnailCandidates(t, newFixture) })
	t.Run("orphan ageing compares against the database clock", func(t *testing.T) { runMediaOrphanAgeing(t, newFixture) })
	t.Run("missing asset ageing compares against the database clock", func(t *testing.T) { runMediaMissingAgeing(t, newFixture) })
}

// RunMediaPublish verifies the atomic publication and in-place registration contract.
func RunMediaPublish(t *testing.T, newFixture NewMediaFixture) {
	t.Helper()
	t.Run("clearing ingest progress is idempotent", func(t *testing.T) { runClearIngestProgress(t, newFixture) })
	t.Run("original publication freezes policy and clears progress atomically", func(t *testing.T) { runOriginalPublication(t, newFixture) })
	t.Run("original callback failure rolls back rows", func(t *testing.T) { runOriginalPublicationRollback(t, newFixture) })
	t.Run("encoded publication rechecks attempt and desired profiles", func(t *testing.T) { runEncodedPublicationRechecks(t, newFixture) })
	t.Run("encoded callback failure rolls back asset", func(t *testing.T) { runEncodedPublicationRollback(t, newFixture) })
	t.Run("thumbnail and seek tiles publish through one callback", func(t *testing.T) { runDerivedPublication(t, newFixture) })
	t.Run("thumbnail and seek tile callback failures roll back assets", func(t *testing.T) { runDerivedPublicationRollback(t, newFixture) })
	t.Run("in-place registration is idempotent and prefers published rows", func(t *testing.T) { runInPlacePublication(t, newFixture) })
	t.Run("commit failure after publish returns an error and leaves no rows", func(t *testing.T) { runPublishCommitFailure(t, newFixture) })
	t.Run("original publication rejects an occupied rel_path without publishing", func(t *testing.T) { runOriginalRelPathConflict(t, newFixture) })
	t.Run("thumbnail and seek tiles skip trashed recordings and existing assets", func(t *testing.T) { runDerivedPublicationSkips(t, newFixture) })
	t.Run("cut publication serializes with chapter edits", func(t *testing.T) { runEncodedCutSerializesWithChapterEdits(t, newFixture) })
	t.Run("cut publication waits for an open chapter edit and rechecks", func(t *testing.T) { runEncodedCutWaitsForChapterEdit(t, newFixture) })
	t.Run("encoded publication waits for an open removal and rechecks", func(t *testing.T) { runEncodedWaitsForOpenRemoval(t, newFixture) })
}

func runOriginalPublication(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	expected := int64(2000)
	f.SetIngestProgress(t, id, 1000, &expected)
	resolverCalls, publishCalls := 0, 0
	if _, ok := mediaEncodePolicy(t, f, id); ok {
		t.Fatal("encode policy exists before original commit")
	}
	err := f.Publisher.CommitOriginal(ctx, store.CommitOriginalInput{
		RecordingID: id, RelPath: fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), SizeBytes: 1000,
	}, func(in store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
		resolverCalls++
		if in.RecordingID != id || in.Site != f.Site || in.Reservation != nil {
			t.Fatalf("policy inputs = %+v; want recording %d/site %q without reservation", in, id, f.Site)
		}
		return store.EncodePolicySnapshot{KeepOriginal: "always", EncodeProfiles: []string{"h264"}, CMDetect: true}, nil
	}, func() error { publishCalls++; return nil })
	if err != nil {
		t.Fatalf("CommitOriginal: %v", err)
	}
	if resolverCalls != 1 || publishCalls != 1 {
		t.Fatalf("resolver calls = %d, publisher calls = %d; want one each", resolverCalls, publishCalls)
	}
	if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); err != nil {
		t.Fatalf("committed original is not visible: %v", err)
	}
	policy, ok := mediaEncodePolicy(t, f, id)
	if !ok || policy.KeepOriginal != "always" || len(policy.EncodeProfiles) != 1 || policy.EncodeProfiles[0] != "h264" || !policy.CMDetect {
		t.Fatalf("Catalog.Export.RecordingEncodePolicies fields KeepOriginal/EncodeProfiles/CMDetect = (%q, %v, %t), found=%t; want always/[h264]/true",
			policy.KeepOriginal, policy.EncodeProfiles, policy.CMDetect, ok)
	}
	if written, expected := mediaIngestProgress(t, f, id); written != nil || expected != nil {
		t.Fatalf("RecordingQuery.Get.IngestWrittenBytes/IngestExpectedBytes remain after original commit: %s/%s", ptrText(written), ptrText(expected))
	}
}

func runOriginalPublicationRollback(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	expected := int64(2000)
	f.SetIngestProgress(t, id, 1000, &expected)
	wantErr := errors.New("publish failed")
	publishCalls := 0
	err := f.Publisher.CommitOriginal(ctx, store.CommitOriginalInput{
		RecordingID: id, RelPath: fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), SizeBytes: 1,
	}, func(store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
		return store.EncodePolicySnapshot{KeepOriginal: "always", EncodeProfiles: []string{}}, nil
	}, func() error { publishCalls++; return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("CommitOriginal error = %v, want publish failure", err)
	}
	if publishCalls != 1 {
		t.Fatalf("publisher calls = %d, want 1", publishCalls)
	}
	if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("original after rollback error = %v, want ErrNotFound", err)
	}
	if _, ok := mediaEncodePolicy(t, f, id); ok {
		t.Fatal("encode policy row survived callback rollback")
	}
	if written, gotExpected := mediaIngestProgress(t, f, id); written == nil || *written != 1000 || gotExpected == nil || *gotExpected != expected {
		t.Fatalf("RecordingQuery.Get.IngestWrittenBytes/IngestExpectedBytes after callback rollback = (%s, %s), want 1000/%d", ptrText(written), ptrText(gotExpected), expected)
	}
}

func runEncodedPublicationRechecks(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SetEncodeProfiles(t, id, []string{"h264"})
	attempt := f.BeginEncodeAttempt(t, id, "h264")
	input := store.EncodedPublishInput{
		RecordingID: id, Profile: "h264", Attempt: attempt + 1,
		RelPath: fmt.Sprintf("sites/%s/%d/encoded.mp4", f.Site, id), SizeBytes: 1,
	}
	publishCalls := 0
	published, err := f.Publisher.PublishEncoded(ctx, input, func() error { publishCalls++; return nil })
	if err != nil || published || publishCalls != 0 {
		t.Fatalf("stale attempt publish = (%v, %v), callback calls %d; want skip without callback", published, err, publishCalls)
	}
	input.Attempt = attempt
	f.SetEncodeProfiles(t, id, nil)
	published, err = f.Publisher.PublishEncoded(ctx, input, func() error { publishCalls++; return nil })
	if err != nil || published || publishCalls != 0 {
		t.Fatalf("removed desired profile publish = (%v, %v), callback calls %d; want skip without callback", published, err, publishCalls)
	}
	// 方針の行が無い録画は、どの profile も desired に無いものとして公開しない。
	noPolicyID := f.NewRecording(t)
	noPolicyInput := input
	noPolicyInput.RecordingID = noPolicyID
	noPolicyInput.RelPath = fmt.Sprintf("sites/%s/%d/encoded.mp4", f.Site, noPolicyID)
	noPolicyInput.Attempt = f.BeginEncodeAttempt(t, noPolicyID, "h264")
	published, err = f.Publisher.PublishEncoded(ctx, noPolicyInput, func() error { publishCalls++; return nil })
	if err != nil || published || publishCalls != 0 {
		t.Fatalf("publish without a policy row = (%v, %v), callback calls %d; want skip without callback", published, err, publishCalls)
	}
	f.SetEncodeProfiles(t, id, []string{"h264"})
	published, err = f.Publisher.PublishEncoded(ctx, input, func() error { publishCalls++; return nil })
	if err != nil || !published || publishCalls != 1 {
		t.Fatalf("current encoded publish = (%v, %v), callback calls %d; want one committed callback", published, err, publishCalls)
	}
	if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, "h264"); err != nil {
		t.Fatalf("published encoded asset is not visible: %v", err)
	}
}

func runEncodedPublicationRollback(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SetEncodeProfiles(t, id, []string{"h264"})
	attempt := f.BeginEncodeAttempt(t, id, "h264")
	wantErr := errors.New("publish failed")
	publishCalls := 0
	published, err := f.Publisher.PublishEncoded(ctx, store.EncodedPublishInput{
		RecordingID: id, Profile: "h264", Attempt: attempt,
		RelPath: fmt.Sprintf("sites/%s/%d/encoded.mp4", f.Site, id), SizeBytes: 1,
	}, func() error { publishCalls++; return wantErr })
	if published || !errors.Is(err, wantErr) || publishCalls != 1 {
		t.Fatalf("encoded publish with callback failure = (%v, %v), callback calls %d; want rollback after one callback", published, err, publishCalls)
	}
	if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, "h264"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("encoded asset after callback rollback = %v, want ErrNotFound", err)
	}
}

func runDerivedPublication(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	originalID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
	thumbnailCalls := 0
	thumbnailPath := fmt.Sprintf("thumbnails/%d.jpg", id)
	published, err := f.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
		RecordingID: id, RelPath: thumbnailPath, SizeBytes: 10, InputAssetID: originalID,
	}, func() error { thumbnailCalls++; return nil })
	if err != nil || !published || thumbnailCalls != 1 {
		t.Fatalf("thumbnail publish = (%v, %v), callback calls %d; want one committed callback", published, err, thumbnailCalls)
	}
	if _, err := f.Serving.GetThumbnailMediaAssetForServing(ctx, id); err != nil {
		t.Fatalf("published thumbnail is not visible: %v", err)
	}

	seekCalls := 0
	published, err = f.Publisher.PublishSeekTiles(ctx, store.SeekTilesPublishInput{
		RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d_tiles.jpg", id), SizeBytes: 20,
	}, func() error { seekCalls++; return nil })
	if err != nil || !published || seekCalls != 1 {
		t.Fatalf("seek tiles publish = (%v, %v), callback calls %d; want one committed callback", published, err, seekCalls)
	}
	if _, err := f.Serving.GetSeekTilesMediaAssetForServing(ctx, id); err != nil {
		t.Fatalf("published seek tiles are not visible: %v", err)
	}
}

func runDerivedPublicationRollback(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	originalID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
	wantErr := errors.New("publish failed")
	thumbnailCalls := 0
	published, err := f.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
		RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d.jpg", id), SizeBytes: 10, InputAssetID: originalID,
	}, func() error { thumbnailCalls++; return wantErr })
	if published || !errors.Is(err, wantErr) || thumbnailCalls != 1 {
		t.Fatalf("thumbnail publish with callback failure = (%v, %v), callback calls %d; want rollback after one callback", published, err, thumbnailCalls)
	}
	if _, err := f.Serving.GetThumbnailMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("thumbnail asset after callback rollback = %v, want ErrNotFound", err)
	}

	seekCalls := 0
	published, err = f.Publisher.PublishSeekTiles(ctx, store.SeekTilesPublishInput{
		RecordingID: id, RelPath: fmt.Sprintf("thumbnails/%d_tiles.jpg", id), SizeBytes: 20,
	}, func() error { seekCalls++; return wantErr })
	if published || !errors.Is(err, wantErr) || seekCalls != 1 {
		t.Fatalf("seek tiles publish with callback failure = (%v, %v), callback calls %d; want rollback after one callback", published, err, seekCalls)
	}
	if _, err := f.Serving.GetSeekTilesMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("seek tiles asset after callback rollback = %v, want ErrNotFound", err)
	}
}

func runInPlacePublication(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	input := f.NewInPlaceInput(t, fmt.Sprintf("sites/%s/imported/%d.m2ts", f.Site, time.Now().UnixNano()))
	first, err := f.Publisher.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatalf("first RegisterInPlace: %v", err)
	}
	second, err := f.Publisher.RegisterInPlace(ctx, input)
	if err != nil {
		t.Fatalf("second RegisterInPlace: %v", err)
	}
	if first.RecordingID != second.RecordingID || len(first.AssetIDs) != 1 || len(second.AssetIDs) != 1 || first.AssetIDs[0] != second.AssetIDs[0] {
		t.Fatalf("idempotent results = %+v then %+v", first, second)
	}

	path := fmt.Sprintf("sites/%s/imported/preferred-%d.m2ts", f.Site, time.Now().UnixNano())
	staleRecording := f.NewRecording(t)
	liveRecording := f.NewRecording(t)
	staleAsset := f.NewAsset(t, staleRecording, "original", nil, path, "deleted")
	liveAsset := f.NewAsset(t, liveRecording, "original", nil, path, "active")
	preferred := f.NewInPlaceInput(t, path)
	got, err := f.Publisher.RegisterInPlace(ctx, preferred)
	if err != nil {
		t.Fatalf("RegisterInPlace with deleted and active rows: %v", err)
	}
	if got.RecordingID != liveRecording || len(got.AssetIDs) != 1 || got.AssetIDs[0] != liveAsset || got.AssetIDs[0] == staleAsset {
		t.Fatalf("in-place preferred result = %+v, want active row recording %d/asset %d (deleted asset %d)", got, liveRecording, liveAsset, staleAsset)
	}
}

func runCurrentCutKeep(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SetCMRanges(t, id, []chapters.Range{{StartMs: 1000, EndMs: 2000}})
	if keep, owned, err := f.Reconcile.GetCurrentCutKeep(ctx, id); err != nil || owned || len(keep) != 0 {
		t.Fatalf("GetCurrentCutKeep(%d) without ownership = (%v, %v, %v), want (nil, false, nil)", id, keep, owned, err)
	}

	f.SetChapterSpans(t, id, []chapters.Span{
		{StartMs: 0, EndMs: 30030, Label: "main"},
		{StartMs: 30030, EndMs: 45045, Label: "CM", Cut: true},
		{StartMs: 45045, EndMs: 60060, Label: "end"},
	})
	want := []chapters.Range{{StartMs: 0, EndMs: 30030}, {StartMs: 45045, EndMs: 60060}}
	keep, owned, err := f.Reconcile.GetCurrentCutKeep(ctx, id)
	if err != nil || !owned || !chapters.SameRanges(keep, want) {
		t.Fatalf("GetCurrentCutKeep(%d) = (%v, %v, %v), want (%v, true, nil)", id, keep, owned, err, want)
	}

	allCutID := f.NewRecording(t)
	f.SetChapterSpans(t, allCutID, []chapters.Span{{StartMs: 0, EndMs: 60060, Label: "CM", Cut: true}})
	keep, owned, err = f.Reconcile.GetCurrentCutKeep(ctx, allCutID)
	if err != nil || !owned || len(keep) != 0 {
		t.Fatalf("GetCurrentCutKeep(%d) for all-cut timeline = (%v, %v, %v), want (nil, true, nil)", allCutID, keep, owned, err)
	}

	f.SetRecordingPurged(t, id)
	if _, _, err := f.Reconcile.GetCurrentCutKeep(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetCurrentCutKeep(%d) after purge error = %v, want ErrNotFound", id, err)
	}
}

func runRecordingEncodeTimes(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	times, err := f.Reconcile.GetRecordingEncodeTimes(ctx, id)
	if err != nil || times.StartedAt != nil || times.EndedAt != nil || times.ProgramDurationMs != 60_000 {
		t.Fatalf("GetRecordingEncodeTimes(%d) with NULL timestamps = (%+v, %v), want NULL/NULL/60000", id, times, err)
	}

	startedAt := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	endedAt := startedAt.Add(90 * time.Minute)
	f.SetRecordingEncodeTimes(t, id, &startedAt, &endedAt)
	times, err = f.Reconcile.GetRecordingEncodeTimes(ctx, id)
	if err != nil || times.StartedAt == nil || !times.StartedAt.Equal(startedAt) || times.EndedAt == nil || !times.EndedAt.Equal(endedAt) {
		t.Fatalf("GetRecordingEncodeTimes(%d) with timestamps = (%+v, %v), want %s..%s", id, times, err, startedAt, endedAt)
	}

	f.SetRecordingEncodeTimes(t, id, nil, &endedAt)
	times, err = f.Reconcile.GetRecordingEncodeTimes(ctx, id)
	if err != nil || times.StartedAt != nil || times.EndedAt == nil || !times.EndedAt.Equal(endedAt) {
		t.Fatalf("GetRecordingEncodeTimes(%d) with one NULL timestamp = (%+v, %v), want NULL/%s", id, times, err, endedAt)
	}
	f.SetRecordingEncodeTimes(t, id, &startedAt, nil)
	times, err = f.Reconcile.GetRecordingEncodeTimes(ctx, id)
	if err != nil || times.StartedAt == nil || !times.StartedAt.Equal(startedAt) || times.EndedAt != nil {
		t.Fatalf("GetRecordingEncodeTimes(%d) with inverse NULL timestamp = (%+v, %v), want %s/NULL", id, times, err, startedAt)
	}
	if _, err := f.Reconcile.GetRecordingEncodeTimes(ctx, int64(1<<63-1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingEncodeTimes for missing recording error = %v, want ErrNotFound", err)
	}
}

func runRecordingNotifySubject(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.SetRecordingPurged(t, id)
	subject, err := f.Reconcile.GetRecordingNotifySubject(ctx, id)
	if err != nil || subject.Site != f.Site || subject.Title != "store media contract fixture" {
		t.Fatalf("GetRecordingNotifySubject(%d) after purge = (%+v, %v), want site %q and fixture title", id, subject, err, f.Site)
	}
	if _, err := f.Reconcile.GetRecordingNotifySubject(ctx, int64(1<<63-1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingNotifySubject for missing recording error = %v, want ErrNotFound", err)
	}
}

func runHasOriginalMediaAsset(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	if has, err := f.Precheck.HasOriginalMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasOriginalMediaAsset(%d) without row = (%v, %v), want (false, nil)", id, has, err)
	}
	f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("sites/%s/%d/thumbnail.jpg", f.Site, id), "active")
	if has, err := f.Precheck.HasOriginalMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasOriginalMediaAsset(%d) with only a thumbnail = (%v, %v), want (false, nil)", id, has, err)
	}
	f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "deleted")
	if has, err := f.Precheck.HasOriginalMediaAsset(ctx, id); err != nil || !has {
		t.Fatalf("HasOriginalMediaAsset(%d) with deleted row = (%v, %v), want (true, nil)", id, has, err)
	}
}

func runLiveMediaAssetRecordingID(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	for _, state := range []string{"active", "deleting"} {
		id := f.NewRecording(t)
		relPath := fmt.Sprintf("sites/%s/%d/%s.m2ts", f.Site, id, state)
		f.NewAsset(t, id, "original", nil, relPath, state)
		got, err := f.Precheck.GetLiveMediaAssetRecordingID(ctx, relPath)
		if err != nil || got != id {
			t.Fatalf("GetLiveMediaAssetRecordingID(%q) for %s row = (%d, %v), want (%d, nil)", relPath, state, got, err, id)
		}
	}

	thumbnailID := f.NewRecording(t)
	thumbnailPath := fmt.Sprintf("sites/%s/%d/thumbnail.jpg", f.Site, thumbnailID)
	f.NewAsset(t, thumbnailID, "thumbnail", nil, thumbnailPath, "active")
	if got, err := f.Precheck.GetLiveMediaAssetRecordingID(ctx, thumbnailPath); err != nil || got != thumbnailID {
		t.Fatalf("GetLiveMediaAssetRecordingID(%q) for non-original row = (%d, %v), want (%d, nil)", thumbnailPath, got, err, thumbnailID)
	}

	deletedID := f.NewRecording(t)
	deletedPath := fmt.Sprintf("sites/%s/%d/deleted.m2ts", f.Site, deletedID)
	f.NewAsset(t, deletedID, "original", nil, deletedPath, "deleted")
	if _, err := f.Precheck.GetLiveMediaAssetRecordingID(ctx, deletedPath); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetLiveMediaAssetRecordingID(%q) for deleted-only row error = %v, want ErrNotFound", deletedPath, err)
	}
}

func runClearIngestProgress(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	otherID := f.NewRecording(t)
	f.SetIngestProgress(t, id, 100, nil)
	f.SetIngestProgress(t, otherID, 200, nil)
	if written, expected := mediaIngestProgress(t, f, id); written == nil || *written != 100 || expected != nil {
		t.Fatalf("RecordingQuery.Get.IngestWrittenBytes/IngestExpectedBytes before clear = (%s, %s), want 100/nil", ptrText(written), ptrText(expected))
	}
	if err := f.Publisher.ClearIngestProgress(ctx, id); err != nil {
		t.Fatalf("ClearIngestProgress(%d): %v", id, err)
	}
	if written, expected := mediaIngestProgress(t, f, id); written != nil || expected != nil {
		t.Fatalf("RecordingQuery.Get.IngestWrittenBytes/IngestExpectedBytes remain after clear: %s/%s", ptrText(written), ptrText(expected))
	}
	if written, expected := mediaIngestProgress(t, f, otherID); written == nil || *written != 200 || expected != nil {
		t.Fatalf("RecordingQuery.Get.IngestWrittenBytes/IngestExpectedBytes for other recording after clear = (%s, %s), want 200/nil", ptrText(written), ptrText(expected))
	}
	if err := f.Publisher.ClearIngestProgress(ctx, id); err != nil {
		t.Fatalf("ClearIngestProgress(%d) without row: %v", id, err)
	}
}

func runMediaJobInputs(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalMediaAsset(%d) error = %v, want ErrNotFound", id, err)
	}
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (false, nil)", id, has, err)
	}

	originalPath := fmt.Sprintf("sites/home/%d/original.m2ts", id)
	originalID := f.NewAsset(t, id, "original", nil, originalPath, "active")
	original, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id)
	if err != nil {
		t.Fatalf("GetActiveOriginalMediaAsset(%d): %v", id, err)
	}
	if original.ID != originalID || original.RelPath != originalPath {
		t.Fatalf("active original = %+v, want id %d and path %q", original, originalID, originalPath)
	}

	thumbnailID := f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || !has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) = (%v, %v), want (true, nil)", id, has, err)
	}
	f.SetAssetState(t, thumbnailID, "deleted")
	if has, err := f.Reconcile.HasActiveThumbnailMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveThumbnailMediaAsset(%d) after deletion = (%v, %v), want (false, nil)", id, has, err)
	}
	if has, err := f.Reconcile.HasActiveSeekTilesMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveSeekTilesMediaAsset(%d) = (%v, %v), want (false, nil)", id, has, err)
	}
	seekTilesID := f.NewAsset(t, id, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", id), "active")
	if has, err := f.Reconcile.HasActiveSeekTilesMediaAsset(ctx, id); err != nil || !has {
		t.Fatalf("HasActiveSeekTilesMediaAsset(%d) = (%v, %v), want (true, nil)", id, has, err)
	}
	f.SetAssetState(t, seekTilesID, "deleted")
	if has, err := f.Reconcile.HasActiveSeekTilesMediaAsset(ctx, id); err != nil || has {
		t.Fatalf("HasActiveSeekTilesMediaAsset(%d) after deletion = (%v, %v), want (false, nil)", id, has, err)
	}
	f.SetAssetState(t, originalID, "deleted")
	if _, err := f.Reconcile.GetActiveOriginalMediaAsset(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveOriginalMediaAsset(%d) after deletion error = %v, want ErrNotFound", id, err)
	}
}

func runMediaEncodePlanning(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	runMediaEncodePlanningContract(t, f)
}

func runMediaServingActiveOnly(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	for _, state := range []string{"deleting", "deleted"} {
		t.Run("original "+state, func(t *testing.T) {
			id := f.NewRecording(t)
			f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/original.m2ts", id), state)
			if _, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetOriginalMediaAssetForServing(%d) error = %v, want ErrNotFound", id, err)
			}
		})
		t.Run("encoded "+state, func(t *testing.T) {
			id := f.NewRecording(t)
			profile := "h264"
			f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/encoded.mp4", id), state)
			if _, err := f.Serving.GetEncodedMediaAssetForServing(ctx, id, profile); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetEncodedMediaAssetForServing(%d, %q) error = %v, want ErrNotFound", id, profile, err)
			}
		})
	}

	id := f.NewRecording(t)
	path := "sites/home/active.m2ts"
	assetID := f.NewAsset(t, id, "original", nil, path, "active")
	asset, err := f.Serving.GetOriginalMediaAssetForServing(ctx, id)
	if err != nil {
		t.Fatalf("GetOriginalMediaAssetForServing(%d): %v", id, err)
	}
	if asset.ID != assetID || asset.RelPath != path {
		t.Fatalf("served original = %+v, want id %d and path %q", asset, assetID, path)
	}
}

func runMediaServingDerived(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	kinds := []struct {
		kind string
		path func(id int64) string
		get  func(id int64) (store.MediaAsset, error)
	}{
		{"thumbnail", func(id int64) string { return fmt.Sprintf("thumbnails/%d.jpg", id) },
			func(id int64) (store.MediaAsset, error) { return f.Serving.GetThumbnailMediaAssetForServing(ctx, id) }},
		{"seek_tiles", func(id int64) string { return fmt.Sprintf("thumbnails/%d_tiles.jpg", id) },
			func(id int64) (store.MediaAsset, error) { return f.Serving.GetSeekTilesMediaAssetForServing(ctx, id) }},
	}
	for _, k := range kinds {
		for _, state := range []string{"deleting", "deleted"} {
			t.Run(k.kind+" "+state, func(t *testing.T) {
				id := f.NewRecording(t)
				f.NewAsset(t, id, k.kind, nil, k.path(id), state)
				if _, err := k.get(id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("%s %s error = %v, want ErrNotFound", k.kind, state, err)
				}
			})
		}
		t.Run(k.kind+" of a trashed recording", func(t *testing.T) {
			id := f.NewRecording(t)
			f.NewAsset(t, id, k.kind, nil, k.path(id), "active")
			f.SetRecordingTrashed(t, id)
			if _, err := k.get(id); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("trashed %s error = %v, want ErrNotFound", k.kind, err)
			}
		})
		t.Run(k.kind+" active", func(t *testing.T) {
			id := f.NewRecording(t)
			assetID := f.NewAsset(t, id, k.kind, nil, k.path(id), "active")
			got, err := k.get(id)
			if err != nil {
				t.Fatalf("%s: %v", k.kind, err)
			}
			if got.ID != assetID || got.RelPath != k.path(id) {
				t.Fatalf("%s = %+v, want id %d path %q", k.kind, got, assetID, k.path(id))
			}
		})
	}
}

func runMediaOriginalVODTarget(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")
	got, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site)
	if err != nil {
		t.Fatalf("GetOriginalVODTarget(own site): %v", err)
	}
	if got.ID != assetID || got.Site != f.Site {
		t.Fatalf("target = %+v, want asset %d in site %q", got, assetID, f.Site)
	}
	if _, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site+"-other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOriginalVODTarget(other site) error = %v, want ErrNotFound", err)
	}
	f.SetRecordingTrashed(t, id)
	if _, err := f.Serving.GetOriginalVODTarget(ctx, id, f.Site); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOriginalVODTarget(trashed) error = %v, want ErrNotFound", err)
	}
}

func runMediaRecordingHLSTarget(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	startedAt := time.Date(2026, 10, 25, 3, 4, 5, 0, time.UTC)
	id := f.NewRecording(t)
	f.SetRecordingEncodeTimes(t, id, &startedAt, nil)
	assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "active")

	target, err := f.Serving.GetRecordingHLSTarget(ctx, id)
	if err != nil {
		t.Fatalf("GetRecordingHLSTarget(%d): %v", id, err)
	}
	if target.Kind != store.RecordingHLSInputOriginal || target.ID != assetID || target.IngestGeneration != 0 || target.StartedAt == nil || !target.StartedAt.Equal(startedAt) {
		t.Fatalf("target = %+v, want asset %d, generation 0 and start %s", target, assetID, startedAt)
	}

	if _, err := f.Serving.GetRecordingHLSTarget(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingHLSTarget(missing) error = %v, want ErrNotFound", err)
	}

	missingOriginalID := f.NewRecording(t)
	if _, err := f.Serving.GetRecordingHLSTarget(ctx, missingOriginalID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingHLSTarget(without input) error = %v, want ErrNotFound", err)
	}

	removedAt := time.Date(2026, 10, 25, 3, 4, 5, 0, time.UTC)
	purgedAt := removedAt.Add(time.Hour)
	supersededAt := removedAt.Add(2 * time.Hour)
	for _, tc := range []struct {
		name                              string
		state                             string
		deletedAt, purgedAt, supersededAt *time.Time
	}{
		{name: "inactive original", state: "deleting"},
		{name: "trash", state: "active", deletedAt: &removedAt},
		{name: "purge", state: "active", deletedAt: &removedAt, purgedAt: &purgedAt},
		{name: "supersede", state: "active", supersededAt: &supersededAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removedID := f.NewRecording(t)
			f.NewAsset(t, removedID, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, removedID), tc.state)
			if tc.deletedAt != nil || tc.purgedAt != nil || tc.supersededAt != nil {
				f.SetRecordingRemovalTimes(t, removedID, tc.deletedAt, tc.purgedAt, tc.supersededAt)
			}
			if _, err := f.Serving.GetRecordingHLSTarget(ctx, removedID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetRecordingHLSTarget(%s) error = %v, want ErrNotFound", tc.name, err)
			}
		})
	}
}

func runMediaRecordingRemovalState(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	deleted := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	purged := deleted.Add(time.Hour)
	superseded := deleted.Add(2 * time.Hour)
	for _, tc := range []struct {
		name                        string
		deleted, purged, superseded *time.Time
	}{
		{"none", nil, nil, nil},
		{"deleted", &deleted, nil, nil},
		{"purged", &deleted, &purged, nil},
		{"superseded", nil, nil, &superseded},
	} {
		id := f.NewRecording(t)
		if tc.deleted != nil || tc.purged != nil || tc.superseded != nil {
			f.SetRecordingRemovalTimes(t, id, tc.deleted, tc.purged, tc.superseded)
		}
		got, err := f.Serving.GetRecordingRemovalState(ctx, id)
		if err != nil {
			t.Fatalf("GetRecordingRemovalState(%s): %v", tc.name, err)
		}
		for _, field := range []struct {
			name      string
			got, want *time.Time
		}{{"DeletedAt", got.DeletedAt, tc.deleted}, {"PurgedAt", got.PurgedAt, tc.purged}, {"SupersededAt", got.SupersededAt, tc.superseded}} {
			if (field.got == nil) != (field.want == nil) || (field.got != nil && !field.got.Equal(*field.want)) {
				t.Errorf("GetRecordingRemovalState(%s).%s = %s, want %s", tc.name, field.name, ptrText(field.got), ptrText(field.want))
			}
		}
	}
	if _, err := f.Serving.GetRecordingRemovalState(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordingRemovalState(missing) error = %v, want ErrNotFound", err)
	}
}

func runMediaChaseTarget(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	if _, err := f.Serving.GetChaseTarget(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChaseTarget without record_sync error = %v, want ErrNotFound", err)
	}
	f.NewRecordSync(t, id, f.Site, "record-1", "recording")
	got, err := f.Serving.GetChaseTarget(ctx, id)
	if err != nil {
		t.Fatalf("GetChaseTarget: %v", err)
	}
	if got.Site != f.Site || got.RecordID != "record-1" || got.Status != "recording" || got.RecordingStatus != "finished" || got.HasOriginalAsset {
		t.Fatalf("chase target = %+v, want site %q record-1 recording/finished without an original", got, f.Site)
	}
	f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d/original.m2ts", f.Site, id), "deleted")
	if got, err := f.Serving.GetChaseTarget(ctx, id); err != nil || !got.HasOriginalAsset {
		t.Fatalf("GetChaseTarget with a deleted original = (%+v, %v), want HasOriginalAsset regardless of state", got, err)
	}
	f.SetRecordingTrashed(t, id)
	if _, err := f.Serving.GetChaseTarget(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetChaseTarget of a trashed recording error = %v, want ErrNotFound", err)
	}
}

func runMediaNamespaceMalformed(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	validID := f.NewRecording(t)
	f.NewAsset(t, validID, "original", nil, "sites/home/archive/show.m2ts", "active")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() = (%+v, %v), want (nil, nil)", violation, err)
	}

	invalidID := f.NewRecording(t)
	assetID := f.NewAsset(t, invalidID, "original", nil, "sites//archive/show.m2ts", "active")
	violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx)
	if err != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath(): %v", err)
	}
	if violation == nil || violation.ID != assetID || violation.RelPath != "sites//archive/show.m2ts" {
		t.Fatalf("violation = %+v, want asset %d with malformed site path", violation, assetID)
	}
	f.SetAssetState(t, assetID, "deleted")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(ctx); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() after deleting malformed row = (%+v, %v), want (nil, nil)", violation, err)
	}
}

func runMediaNamespaceUnprefixed(t *testing.T, newFixture NewMediaFixture) {
	const unprefixed = "20240101/show.m2ts"
	for _, kind := range []string{"original", "encoded"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			var profile *string
			if kind == "encoded" {
				p := "h264"
				profile = &p
			}
			assetID := f.NewAsset(t, f.NewRecording(t), kind, profile, unprefixed, "active")
			violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(context.Background())
			if err != nil {
				t.Fatalf("FindInvalidMediaAssetRelPath(): %v", err)
			}
			if violation == nil || violation.ID != assetID || violation.Kind != kind || violation.RelPath != unprefixed {
				t.Fatalf("violation = %+v, want %s asset %d at %q", violation, kind, assetID, unprefixed)
			}
		})
	}
}

func runMediaNamespaceDerived(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	id := f.NewRecording(t)
	f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
	f.NewAsset(t, id, "seek_tiles", nil, fmt.Sprintf("thumbnails/%d_tiles.jpg", id), "active")
	if violation, err := f.Reconcile.FindInvalidMediaAssetRelPath(context.Background()); err != nil || violation != nil {
		t.Fatalf("FindInvalidMediaAssetRelPath() = (%+v, %v), want (nil, nil) for thumbnails/ paths", violation, err)
	}
}

func runMediaAuditPaths(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	want := []string{
		"sites/home/archive/20240101/show.m2ts",
		fmt.Sprintf("thumbnails/%d.jpg", id),
		fmt.Sprintf("thumbnails/%d_tiles.jpg", id),
	}
	f.NewAsset(t, id, "original", nil, want[0], "deleted")
	f.NewAsset(t, id, "thumbnail", nil, want[1], "active")
	f.NewAsset(t, id, "seek_tiles", nil, want[2], "active")
	paths, err := f.Storage.ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		t.Fatalf("ListAllMediaAssetRelPaths(): %v", err)
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("ListAllMediaAssetRelPaths() = %q, want it to include %q", paths, w)
		}
	}
}

func runMediaOrphanAgeing(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	const old, fresh = "sites/home/old.tmp", "sites/home/fresh.tmp"
	for _, p := range []string{old, fresh} {
		if err := f.Storage.UpsertOrphanFile(ctx, p); err != nil {
			t.Fatalf("UpsertOrphanFile(%q): %v", p, err)
		}
	}
	f.SetOrphanFirstSeenAgo(t, old, 2*time.Hour)
	got, err := f.Storage.ListAgedOrphanFiles(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ListAgedOrphanFiles(1h): %v", err)
	}
	if len(got) != 1 || got[0] != old {
		t.Fatalf("ListAgedOrphanFiles(1h) = %q, want only %q", got, old)
	}
	got, err = f.Storage.ListAgedOrphanFiles(ctx, 3*time.Hour)
	if err != nil {
		t.Fatalf("ListAgedOrphanFiles(3h): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListAgedOrphanFiles(3h) = %q, want none", got)
	}
}

func runMediaMissingAgeing(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/original.m2ts", id), "active")
	if err := f.Storage.UpsertMissingMediaAsset(ctx, assetID); err != nil {
		t.Fatalf("UpsertMissingMediaAsset(%d): %v", assetID, err)
	}
	f.SetMissingFirstSeenAgo(t, assetID, 2*time.Hour)
	got, err := f.Storage.ListAgedMissingMediaAssets(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ListAgedMissingMediaAssets(1h): %v", err)
	}
	if len(got) != 1 || got[0].ID != assetID || got[0].RecordingID != id || got[0].Kind != "original" {
		t.Fatalf("ListAgedMissingMediaAssets(1h) = %+v, want only asset %d", got, assetID)
	}
	if got, err := f.Storage.ListAgedMissingMediaAssets(ctx, 3*time.Hour); err != nil || len(got) != 0 {
		t.Fatalf("ListAgedMissingMediaAssets(3h) = (%+v, %v), want none", got, err)
	}
	f.SetAssetState(t, assetID, "deleting")
	if got, err := f.Storage.ListAgedMissingMediaAssets(ctx, time.Hour); err != nil || len(got) != 0 {
		t.Fatalf("ListAgedMissingMediaAssets(1h) after the asset left active = (%+v, %v), want none", got, err)
	}
}

func runMediaEncodePlanningContract(t *testing.T, f MediaFixture) {
	t.Helper()
	ctx := context.Background()
	id := f.NewRecording(t)
	profile := "cut"
	f.SetEncodeProfiles(t, id, []string{profile})
	assetID := f.NewAsset(t, id, "encoded", &profile, fmt.Sprintf("sites/home/%d/cut.mp4", id), "deleted")
	asset, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, false)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q): %v", id, profile, err)
	}
	if asset.EncodedAsset == nil || asset.EncodedAsset.ID != assetID || asset.EncodedAsset.State != "deleted" || asset.EncodedAsset.RelPath != fmt.Sprintf("sites/home/%d/cut.mp4", id) {
		t.Fatalf("encoded asset observation = %+v, want id %d with deleted state and its path", asset, assetID)
	}
	if len(asset.EncodedAsset.KeepRanges) != 0 {
		t.Fatalf("keep ranges before freezing = %v, want none", asset.EncodedAsset.KeepRanges)
	}
	if missing, err := f.Reconcile.GetEncodedPublishState(ctx, id, "missing-profile", false, false); err != nil || missing.EncodedAsset != nil {
		t.Fatalf("GetEncodedPublishState(unknown profile) = %+v, %v; want no asset", missing, err)
	}

	f.SetAssetState(t, assetID, "active")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	f.SetMediaAssetKeepRanges(t, assetID, keep)
	frozen, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, false)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q) after freezing: %v", id, profile, err)
	}
	if frozen.EncodedAsset == nil || frozen.EncodedAsset.State != "active" || !chapters.SameRanges(frozen.EncodedAsset.KeepRanges, keep) {
		t.Fatalf("observation after freezing = %+v, want active with keep ranges %v in the same read", frozen, keep)
	}

	withoutRequest, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, false)
	if err != nil {
		t.Fatalf("GetEncodedPublishState without a request: %v", err)
	}
	if !withoutRequest.Desired || withoutRequest.HasRebuildRequest || withoutRequest.EncodedAsset == nil ||
		withoutRequest.EncodedAsset.ID != assetID || !chapters.SameRanges(withoutRequest.EncodedAsset.KeepRanges, keep) {
		t.Fatalf("GetEncodedPublishState before request = %+v, want desired cut asset without request", withoutRequest)
	}
	f.SetEncodedAssetRebuildRequest(t, id, profile, keep)
	withRequest, err := f.Reconcile.GetEncodedPublishState(ctx, id, profile, true, false)
	if err != nil {
		t.Fatalf("GetEncodedPublishState with a request: %v", err)
	}
	if !withRequest.Desired || !withRequest.HasRebuildRequest || !chapters.SameRanges(withRequest.RebuildRequestKeep, keep) ||
		withRequest.EncodedAsset == nil || withRequest.EncodedAsset.ID != assetID {
		t.Fatalf("rebuild request observation = %+v, want desired request keep %v and encoded asset %d", withRequest, keep, assetID)
	}
}

func runMediaThumbnailCandidates(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	originalPath := fmt.Sprintf("sites/home/%d/original.m2ts", id)
	originalID := f.NewAsset(t, id, "original", nil, originalPath, "active")
	f.NewAsset(t, id, "thumbnail", nil, fmt.Sprintf("thumbnails/%d.jpg", id), "active")
	cutProfile, plainProfile := "cut", "h264"
	cutPath := fmt.Sprintf("sites/home/%d/cut.mp4", id)
	plainPath := fmt.Sprintf("sites/home/%d/h264.mp4", id)
	cutID := f.NewAsset(t, id, "encoded", &cutProfile, cutPath, "active")
	plainID := f.NewAsset(t, id, "encoded", &plainProfile, plainPath, "active")
	frozen := []chapters.Range{{StartMs: 0, EndMs: 1000}, {StartMs: 2000, EndMs: 3000}}
	f.SetMediaAssetKeepRanges(t, cutID, frozen)
	cm := []chapters.Range{{StartMs: 1000, EndMs: 2000}, {StartMs: 4000, EndMs: 5000}}
	f.SetCMRanges(t, id, cm)
	spans := []chapters.Span{{StartMs: 0, EndMs: 1500, Label: "main", Cut: false}, {StartMs: 1500, EndMs: 2500, Label: "CM", Cut: true}}
	f.SetChapterSpans(t, id, spans)

	rows, err := f.Reconcile.ListThumbnailReselectCandidates(ctx, store.MediaReconcilePage{AfterRecordingID: id - 1, RowLimit: 1})
	if err != nil {
		t.Fatalf("ListThumbnailReselectCandidates: %v", err)
	}
	if len(rows) != 1 || rows[0].RecordingID != id {
		t.Fatalf("candidates = %+v, want only recording %d", rows, id)
	}
	row := rows[0]
	if !row.Detected || !row.Owned || row.OriginalMediaAssetID != originalID || row.OriginalRelPath != originalPath {
		t.Fatalf("candidate = %+v, want detected and owned with original %d at %q", row, originalID, originalPath)
	}
	if !chapters.SameRanges(row.CMRanges, cm) {
		t.Fatalf("CMRanges = %v, want %v", row.CMRanges, cm)
	}
	if len(row.UserSpans) != len(spans) {
		t.Fatalf("UserSpans = %+v, want %+v", row.UserSpans, spans)
	}
	for i := range spans {
		if row.UserSpans[i] != spans[i] {
			t.Fatalf("UserSpans[%d] = %+v, want %+v", i, row.UserSpans[i], spans[i])
		}
	}
	if len(row.EncodedAssets) != 2 {
		t.Fatalf("EncodedAssets = %+v, want cut and h264 ordered by profile", row.EncodedAssets)
	}
	gotCut, gotPlain := row.EncodedAssets[0], row.EncodedAssets[1]
	if gotCut.MediaAssetID != cutID || gotCut.Profile != cutProfile || gotCut.RelPath != cutPath || !gotCut.Cut || !chapters.SameRanges(gotCut.KeepRanges, frozen) {
		t.Fatalf("cut input = %+v, want asset %d %q at %q with frozen %v", gotCut, cutID, cutProfile, cutPath, frozen)
	}
	if gotPlain.MediaAssetID != plainID || gotPlain.Profile != plainProfile || gotPlain.RelPath != plainPath || gotPlain.Cut || len(gotPlain.KeepRanges) != 0 {
		t.Fatalf("uncut input = %+v, want asset %d %q at %q without keep ranges", gotPlain, plainID, plainProfile, plainPath)
	}
}

func mediaEncodePolicy(t *testing.T, f MediaFixture, recordingID int64) (store.EncodePolicySnapshot, bool) {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export for recording %d encode policy: %v", recordingID, err)
	}
	for _, policy := range document.RecordingEncodePolicies {
		if policy.RecordingID == recordingID {
			return store.EncodePolicySnapshot{
				KeepOriginal:   policy.KeepOriginal,
				EncodeProfiles: append([]string(nil), policy.EncodeProfiles...),
				CMDetect:       policy.CMDetect,
			}, true
		}
	}
	return store.EncodePolicySnapshot{}, false
}

func mediaIngestProgress(t *testing.T, f MediaFixture, recordingID int64) (*int64, *int64) {
	t.Helper()
	row, err := f.Query.Get(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("RecordingQuery.Get(%d).IngestWrittenBytes/IngestExpectedBytes: %v", recordingID, err)
	}
	return row.IngestWrittenBytes, row.IngestExpectedBytes
}
