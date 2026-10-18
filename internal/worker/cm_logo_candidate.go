package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// CMLogoCandidateWorker creates one station-level candidate from the taught area.
// It deliberately has no retry budget: saving the area again is the user-visible
// retry operation and leaves the previous candidate available for inspection.
type CMLogoCandidateWorker struct {
	Store      store.CMStation
	MediaDir   string
	ScratchDir string
	CMDetect   config.CMDetectConfig
	FFprobe    string
}

// Work persists running before touching the original. Every analysis failure after
// that point becomes a failed candidate row, so the desired view cannot hot-loop.
func (w *CMLogoCandidateWorker) Work(ctx context.Context, args jobs.CMLogoCandidateJobArgs) error {
	station := store.CMStationRef{NetworkID: args.NetworkID, ServiceID: args.ServiceID}
	area, err := w.Store.GetLogoArea(ctx, station)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("CM logo candidate: loading taught area: %w", err)
	}
	if !area.UpdatedAt.Equal(args.AreaUpdatedAt) {
		return nil
	}
	attemptedAt, started, err := w.Store.BeginLogoCandidate(ctx, station, args.RecordingID, args.AreaUpdatedAt)
	if err != nil {
		return err
	}
	if !started {
		return nil
	}
	item, err := w.Store.GetCandidateRecording(ctx, args.RecordingID)
	if err != nil {
		return w.fail(ctx, args, attemptedAt, err)
	}
	if item.NetworkID != args.NetworkID || item.ServiceID != args.ServiceID {
		return w.fail(ctx, args, attemptedAt, cmFailure("setup", fmt.Errorf("analysis recording belongs to another station")))
	}
	if item.IsTrashed || item.OriginalMissing || item.RelPath == nil {
		return w.fail(ctx, args, attemptedAt, cmFailure("setup", fmt.Errorf("active original is missing")))
	}

	if err := w.analyze(ctx, item, area, args, attemptedAt); err != nil {
		return w.fail(ctx, args, attemptedAt, err)
	}
	return nil
}

func (w *CMLogoCandidateWorker) fail(ctx context.Context, args jobs.CMLogoCandidateJobArgs, attemptedAt time.Time, err error) error {
	failureCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	station := store.CMStationRef{NetworkID: args.NetworkID, ServiceID: args.ServiceID}
	if errors.Is(ctx.Err(), context.Canceled) {
		if markErr := w.Store.DiscardLogoCandidate(failureCtx, station, args.AreaUpdatedAt, attemptedAt); markErr != nil {
			return errors.Join(err, fmt.Errorf("clearing canceled CM logo candidate: %w", markErr))
		}
		return err
	}
	if _, markErr := w.Store.MarkLogoCandidateFailed(failureCtx, station, args.AreaUpdatedAt, attemptedAt, cmFailureStage(err), err.Error()); markErr != nil {
		return errors.Join(err, fmt.Errorf("marking CM logo candidate failed: %w", markErr))
	}
	return err
}

func (w *CMLogoCandidateWorker) analyze(
	ctx context.Context,
	item store.CMLogoCandidateRecording,
	area store.CMLogoArea,
	args jobs.CMLogoCandidateJobArgs,
	attemptedAt time.Time,
) error {
	original, err := mediapath.Resolve(w.MediaDir, *item.RelPath)
	if err != nil {
		return cmFailure("setup", fmt.Errorf("resolving original path: %w", err))
	}
	jobDir, err := newWorkerScratchDir(w.ScratchDir, "cm-logo-candidate",
		fmt.Sprintf("%d-%d-", args.RecordingID, attemptedAt.UnixMicro()))
	if err != nil {
		return cmFailure("setup", fmt.Errorf("creating scratch directory: %w", err))
	}
	defer func() {
		if err := os.RemoveAll(jobDir); err != nil {
			slog.Warn("cm_logo_candidate: failed to remove scratch directory", "path", jobDir, "err", err)
		}
	}()

	inputPath := filepath.Join(jobDir, "input.ts")
	if err := os.Symlink(original, inputPath); err != nil {
		return cmFailure("setup", fmt.Errorf("linking original into scratch: %w", err))
	}
	geometry, err := probeVideoGeometry(ctx, commandOutput, w.FFprobe, inputPath)
	if err != nil {
		return cmFailure("probe", fmt.Errorf("probing original size: %w", err))
	}
	if area.CodedWidth != int32(geometry.width) || area.CodedHeight != int32(geometry.height) {
		return cmFailure("area", fmt.Errorf("the taught logo area is for %dx%d but this recording is %dx%d",
			area.CodedWidth, area.CodedHeight, geometry.width, geometry.height))
	}

	logoDir := filepath.Join(jobDir, "logos")
	if err := os.Mkdir(logoDir, 0o700); err != nil {
		return cmFailure("setup", fmt.Errorf("creating temporary logo directory: %w", err))
	}
	channel := fmt.Sprintf("n%d-s%d", args.NetworkID, args.ServiceID)
	logoFrames := filepath.Join(jobDir, "logoframe.txt")
	tools := func(name string) string { return filepath.Join(w.CMDetect.BinaryDir, name) }
	// The empty logo directory is intentional: an existing LGD would skip logo
	// generation when -logo-match is 0. No -seek/-frames are supplied; #961 fixes
	// candidate analysis to the whole original.
	output, err := runCMTool(ctx, jobDir, tools("logoframe"), inputPath,
		"-channel", channel,
		"-logo-dir", logoDir,
		"-logo-match", "0",
		"-oa", logoFrames,
		"-logo-area", fmt.Sprintf("%d,%d,%d,%d", area.X, area.Y, area.W, area.H),
	)
	if err != nil {
		return cmFailure("logo", fmt.Errorf("running logoframe: %w", err))
	}
	matchPercent, err := cmLogoMatchPercent(output)
	if err != nil {
		return cmFailure("logo", err)
	}
	if matchPercent < cmDetectMinLogoMatchPercent {
		return cmFailure("match", fmt.Errorf("station logo match %.2f%% is below %.2f%%", matchPercent, cmDetectMinLogoMatchPercent))
	}
	logo, err := readStationLogo(logoDir, channel)
	if err != nil {
		return cmFailure("logo", fmt.Errorf("reading generated station logo: %w", err))
	}
	preview, previewErr := lgdPreviewPNG(logo)
	if previewErr != nil {
		slog.Warn("cm_logo_candidate: failed to render logo preview", "recording_id", item.ID, "err", previewErr)
		preview = nil
	}
	station := store.CMStationRef{NetworkID: args.NetworkID, ServiceID: args.ServiceID}
	ready, err := w.Store.MarkLogoCandidateReady(ctx, station, args.AreaUpdatedAt, attemptedAt, logo, preview)
	if err != nil {
		return cmFailure("save", fmt.Errorf("saving CM logo candidate: %w", err))
	}
	if !ready {
		// An area save deletes the old candidate and queues a new version. If a
		// caller changed the area outside that transaction, remove only this old
		// version so a new candidate cannot be touched by a late worker.
		if err := w.Store.DiscardLogoCandidate(ctx, station, args.AreaUpdatedAt, attemptedAt); err != nil {
			return cmFailure("save", fmt.Errorf("discarding stale CM logo candidate: %w", err))
		}
	}
	return nil
}
