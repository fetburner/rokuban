package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

type mediaReadSpy struct {
	store.MediaReconcile

	originalCalls          int
	originalOverride       bool
	originalResult         store.MediaAsset
	originalErr            error
	encodedPlanCalls       int
	encodedPlanOverride    bool
	encodedPlanResult      store.EncodedAssetObservation
	encodedPlanErr         error
	keepRangesCalls        int
	keepRangesOverride     bool
	keepRangesResult       []chapters.Range
	keepRangesErr          error
	rebuildMatchesCalls    int
	rebuildMatchesOverride bool
	rebuildMatchesResult   bool
	rebuildMatchesErr      error
	hasThumbnailCalls      int
	thumbnailListCalls     int
	thumbnailListOverride  bool
	thumbnailListResult    []store.ThumbnailReselectCandidate
}

func (s *mediaReadSpy) GetEncodedAssetForPlan(ctx context.Context, recordingID int64, profile string) (store.EncodedAssetObservation, error) {
	s.encodedPlanCalls++
	if s.encodedPlanOverride {
		return s.encodedPlanResult, s.encodedPlanErr
	}
	return s.MediaReconcile.GetEncodedAssetForPlan(ctx, recordingID, profile)
}

func (s *mediaReadSpy) GetMediaAssetKeepRanges(ctx context.Context, mediaAssetID int64) ([]chapters.Range, error) {
	s.keepRangesCalls++
	if s.keepRangesOverride {
		return s.keepRangesResult, s.keepRangesErr
	}
	return s.MediaReconcile.GetMediaAssetKeepRanges(ctx, mediaAssetID)
}

func (s *mediaReadSpy) EncodedAssetRebuildRequestMatches(ctx context.Context, recordingID int64, profile string, keep []chapters.Range) (bool, error) {
	s.rebuildMatchesCalls++
	if s.rebuildMatchesOverride {
		return s.rebuildMatchesResult, s.rebuildMatchesErr
	}
	return s.MediaReconcile.EncodedAssetRebuildRequestMatches(ctx, recordingID, profile, keep)
}

func (s *mediaReadSpy) GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	s.originalCalls++
	if s.originalOverride {
		return s.originalResult, s.originalErr
	}
	return s.MediaReconcile.GetActiveOriginalMediaAsset(ctx, recordingID)
}

func (s *mediaReadSpy) HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	s.hasThumbnailCalls++
	return s.MediaReconcile.HasActiveThumbnailMediaAsset(ctx, recordingID)
}

func (s *mediaReadSpy) ListThumbnailReselectCandidates(ctx context.Context, page store.MediaReconcilePage) ([]store.ThumbnailReselectCandidate, error) {
	s.thumbnailListCalls++
	if s.thumbnailListOverride {
		return s.thumbnailListResult, nil
	}
	return s.MediaReconcile.ListThumbnailReselectCandidates(ctx, page)
}

func TestEncodeWorkerReadsOriginalThroughMediaContract(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/encode.m2ts", nil, []byte("original"))
	media := &mediaReadSpy{
		MediaReconcile:   newTestMediaStore(pool),
		originalOverride: true,
		originalErr:      store.ErrNotFound,
	}
	w := &EncodeWorker{
		Pool:     pool,
		Media:    media,
		MediaDir: mediaDir,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "h264"}}},
	}

	_, _, _, _, err := w.prepareEncodeInput(context.Background(), jobs.EncodeJobArgs{
		RecordingID: recordingID,
		Profile:     "h264",
	}, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "no active original media_asset") {
		t.Fatalf("prepareEncodeInput() error = %v, want missing original from media contract", err)
	}
	if media.originalCalls != 1 {
		t.Fatalf("GetActiveOriginalMediaAsset calls = %d, want 1", media.originalCalls)
	}
}

func TestEncodeWorkerReadsPlanningStateThroughMediaContract(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/plan.m2ts", nil, []byte("original"))
	profile := "cut"
	assetID := seedEncodedAsset(t, pool, recordingID, profile, "media-reads/plan-cut.mp4")
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	if err := setFrozenCuts(t, pool, assetID, keep); err != nil {
		t.Fatalf("seeding frozen keep ranges: %v", err)
	}
	media := &mediaReadSpy{
		MediaReconcile:      newTestMediaStore(pool),
		encodedPlanOverride: true,
		encodedPlanResult: store.EncodedAssetObservation{
			ID: assetID, RelPath: "media-reads/plan-cut.mp4", State: "active",
		},
		keepRangesOverride: true,
		keepRangesResult:   []chapters.Range{{StartMs: 0, EndMs: 900}},
	}
	w := &EncodeWorker{Pool: pool, Media: media}
	observed, err := w.planEncodeAsset(ctx, recordingID, profile, &cutContext{keep: keep}, nil)
	if err != nil {
		t.Fatalf("planEncodeAsset() error = %v", err)
	}
	if observed.skip {
		t.Fatalf("planEncodeAsset() = %+v, want store-provided frozen ranges to keep asset stale", observed)
	}
	if media.encodedPlanCalls != 1 || media.keepRangesCalls != 1 {
		t.Fatalf("media calls: encoded plan=%d frozen ranges=%d, want 1 each", media.encodedPlanCalls, media.keepRangesCalls)
	}
}

func TestEncodeWorkerReadsRebuildRequestThroughMediaContract(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/request.m2ts", nil, []byte("original"))
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}
	requested := seedEncodedAssetRebuildRequest(t, pool, recordingID, keep)
	media := &mediaReadSpy{
		MediaReconcile:         newTestMediaStore(pool),
		rebuildMatchesOverride: true,
		rebuildMatchesResult:   false,
	}
	w := &EncodeWorker{
		Pool:     pool,
		Media:    media,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{cutFFmpegProfile()}},
	}
	_, _, _, skip, err := w.loadRequestedEncodeCut(ctx, jobs.EncodeJobArgs{
		RecordingID:      recordingID,
		Profile:          "cut",
		RequestedCutKeep: requested,
	})
	if err != nil {
		t.Fatalf("loadRequestedEncodeCut() error = %v", err)
	}
	if !skip {
		t.Fatal("loadRequestedEncodeCut() accepted a request the media contract reported as changed")
	}
	if media.rebuildMatchesCalls != 1 {
		t.Fatalf("EncodedAssetRebuildRequestMatches calls = %d, want 1", media.rebuildMatchesCalls)
	}
}

func TestThumbnailWorkerReadsPlanningStateThroughMediaContract(t *testing.T) {
	t.Run("initial thumbnail", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		mediaDir := t.TempDir()
		recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/thumbnail.m2ts", nil, []byte("original"))
		media := &mediaReadSpy{
			MediaReconcile:   newTestMediaStore(pool),
			originalOverride: true,
			originalErr:      store.ErrNotFound,
		}
		commands := 0
		w := &ThumbnailWorker{
			Pool:     pool,
			Media:    media,
			MediaDir: mediaDir,
			runCmd: func(context.Context, string, ...string) ([]byte, error) {
				commands++
				return nil, errors.New("unexpected command")
			},
		}

		if err := runWorkerViaJobQFake(context.Background(), w, jobs.ThumbnailJobArgs{RecordingID: recordingID}); err != nil {
			t.Fatalf("Work() = %v, want missing original to skip", err)
		}
		if media.hasThumbnailCalls != 1 || media.originalCalls != 1 {
			t.Fatalf("media calls: has thumbnail=%d original=%d, want 1 each", media.hasThumbnailCalls, media.originalCalls)
		}
		if commands != 0 {
			t.Errorf("ffmpeg/ffprobe calls = %d, want 0", commands)
		}
		if _, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID); err == nil {
			t.Fatal("thumbnail was created even though the media contract reported no original")
		}
	})

	t.Run("reselection candidate", func(t *testing.T) {
		pool := setupTestPool(t)
		if pool == nil {
			return
		}
		mediaDir := t.TempDir()
		recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "media-reads/reselection.m2ts", nil, []byte("original"))
		if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
			RecordingID: recordingID,
			Kind:        db.AssetKindThumbnail,
			RelPath:     thumbnailRelPath(recordingID),
			SizeBytes:   1,
		}); err != nil {
			t.Fatalf("seeding thumbnail: %v", err)
		}
		media := &mediaReadSpy{
			MediaReconcile:        newTestMediaStore(pool),
			thumbnailListOverride: true,
			thumbnailListResult:   []store.ThumbnailReselectCandidate{},
		}
		w := &ThumbnailWorker{
			Pool:     pool,
			Media:    media,
			MediaDir: mediaDir,
			runCmd: func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("no candidate from the media contract must not run ffmpeg/ffprobe")
				return nil, nil
			},
		}

		if err := runWorkerViaJobQFake(context.Background(), w, jobs.ThumbnailJobArgs{RecordingID: recordingID}); err != nil {
			t.Fatalf("Work() = %v, want empty candidate page to skip", err)
		}
		if media.hasThumbnailCalls != 1 || media.thumbnailListCalls != 1 {
			t.Fatalf("media calls: has thumbnail=%d candidates=%d, want 1 each", media.hasThumbnailCalls, media.thumbnailListCalls)
		}
	})
}
