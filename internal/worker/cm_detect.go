package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

const (
	cmDetectMaxTries         = 3
	cmDetectStaleAfter       = time.Minute
	cmDetectRowLimit   int32 = 1000
	cmDetectRulePath         = config.CMDetectRulePath
)

var trimCall = regexp.MustCompile(`Trim\s*\(\s*(-?\d+)\s*,\s*(-?\d+)\s*\)`)

// CMDetectWorker analyzes a committed original and stores its commercial ranges.
type CMDetectWorker struct {
	river.WorkerDefaults[jobs.CMDetectJobArgs]
	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	CMDetect   config.CMDetectConfig
	// FFprobe は原本の実尺を読む ffprobe のパス（空なら PATH の ffprobe）。
	FFprobe string
}

// Timeout disables River's fixed timeout; Work applies a timeout from program duration.
func (w *CMDetectWorker) Timeout(*river.Job[jobs.CMDetectJobArgs]) time.Duration { return -1 }

// Work learns a missing station logo, runs JL analysis, and commits only a complete result.
func (w *CMDetectWorker) Work(ctx context.Context, job *river.Job[jobs.CMDetectJobArgs]) error {
	started := time.Now()
	defer func() { metrics.CMDetectDuration.Observe(time.Since(started).Seconds()) }()

	jobLock, acquired, err := acquireEncodeJobLock(ctx, w.Pool, job.ID, defaultJobLockTimeout)
	if err != nil {
		return fmt.Errorf("CM detection: acquiring job lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("CM detection: job %d advisory lock is held by another session", job.ID)
	}
	defer jobLock.release()

	q := sqlcgen.New(w.Pool)
	item, err := q.GetCMDetectionWorkItem(ctx, job.Args.RecordingID)
	if err != nil {
		return fmt.Errorf("CM detection: loading recording %d: %w", job.Args.RecordingID, err)
	}
	if !item.CmDetect || item.IsTrashed || item.OriginalMissing || item.Detected || item.RelPath == nil {
		return nil
	}
	desired, err := q.IsCMDetectionDesired(ctx, job.Args.RecordingID)
	if err != nil {
		return fmt.Errorf("CM detection: checking desired state for recording %d: %w", job.Args.RecordingID, err)
	}
	if !desired {
		return nil
	}
	if err := q.MarkCMDetectionRunning(ctx, job.Args.RecordingID); err != nil {
		return fmt.Errorf("CM detection: marking recording %d running: %w", job.Args.RecordingID, err)
	}

	timeout := cmDetectionTimeout(item.ProgramDurationMs)
	workCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := w.detect(workCtx, job.ID, item); err != nil {
		state := "retrying"
		maxAttempts := job.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = cmDetectMaxTries
		}
		if job.Attempt >= maxAttempts {
			state = "failed"
		}
		message := err.Error()
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer failureCancel()
		if markErr := q.MarkCMDetectionFailure(failureCtx, sqlcgen.MarkCMDetectionFailureParams{
			State:       state,
			Error:       &message,
			RecordingID: job.Args.RecordingID,
		}); markErr != nil {
			return errors.Join(fmt.Errorf("CM detection for recording %d: %w", job.Args.RecordingID, err),
				fmt.Errorf("marking CM detection %s: %w", state, markErr))
		}
		return fmt.Errorf("CM detection for recording %d: %w", job.Args.RecordingID, err)
	}
	return nil
}

func (w *CMDetectWorker) detect(ctx context.Context, jobID int64, item sqlcgen.GetCMDetectionWorkItemRow) error {
	if item.RelPath == nil {
		return fmt.Errorf("active original is missing")
	}
	original, err := mediapath.Resolve(w.MediaDir, *item.RelPath)
	if err != nil {
		return fmt.Errorf("resolving original path: %w", err)
	}
	jobRoot := filepath.Join(w.ScratchDir, "cm-detect")
	if err := os.MkdirAll(jobRoot, 0o700); err != nil {
		return fmt.Errorf("creating scratch root: %w", err)
	}
	jobDir := filepath.Join(jobRoot, strconv.FormatInt(jobID, 10))
	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("cleaning previous scratch directory: %w", err)
	}
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return fmt.Errorf("creating job scratch directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(jobDir); err != nil {
			slog.Warn("cm_detect: failed to remove scratch directory", "path", jobDir, "err", err)
		}
	}()

	inputPath := filepath.Join(jobDir, "input.ts")
	if err := os.Symlink(original, inputPath); err != nil {
		return fmt.Errorf("linking original into scratch: %w", err)
	}
	channel := fmt.Sprintf("n%d-s%d", item.NetworkID, item.ServiceID)
	logoDir := filepath.Join(jobDir, "logos")
	if err := os.Mkdir(logoDir, 0o700); err != nil {
		return fmt.Errorf("creating temporary logo directory: %w", err)
	}
	if logo, err := sqlcgen.New(w.Pool).GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
		NetworkID: item.NetworkID,
		ServiceID: item.ServiceID,
	}); err == nil {
		if err := writeStationLogo(logoDir, channel, logo); err != nil {
			return fmt.Errorf("writing learned station logo: %w", err)
		}
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return fmt.Errorf("loading station logo: %w", err)
	}

	logoFrames := filepath.Join(jobDir, "logoframe.txt")
	chapters := filepath.Join(jobDir, "chapter_exe.txt")
	cutAvs := filepath.Join(jobDir, "obs_cut.avs")
	tools := func(name string) string { return filepath.Join(w.CMDetect.BinaryDir, name) }
	if err := runCMTool(ctx, jobDir, tools("logoframe"), inputPath,
		"-channel", channel, "-logo-dir", logoDir, "-logo-match", "0", "-oa", logoFrames); err != nil {
		return fmt.Errorf("running logoframe: %w", err)
	}
	if err := w.persistNewStationLogo(ctx, item, channel, logoDir); err != nil {
		return err
	}
	if err := runCMTool(ctx, jobDir, tools("chapter_exe"), "-v", inputPath, "-s", "8", "-e", "4", "-o", chapters); err != nil {
		return fmt.Errorf("running chapter_exe: %w", err)
	}
	if err := runCMTool(ctx, jobDir, tools("join_logo_scp"), "-inlogo", logoFrames,
		"-inscp", chapters, "-incmd", cmDetectRuleFile(), "-o", cutAvs); err != nil {
		return fmt.Errorf("running join_logo_scp: %w", err)
	}
	cutText, err := os.ReadFile(cutAvs)
	if err != nil {
		return fmt.Errorf("reading obs_cut.avs: %w", err)
	}
	// 総尺は EPG の尺ではなく原本の実尺から取る。録画は EIT 追従で延長されうるので、
	// program_duration_ms で打ち切ると延長分の本編と CM を捨てる。
	videoDuration, err := probeVideoDuration(ctx, commandOutput, w.FFprobe, inputPath)
	if err != nil {
		return fmt.Errorf("probing original duration: %w", err)
	}
	totalMs := videoDuration.Milliseconds()
	ranges, err := cmRangesFromCutAVS(string(cutText), totalMs)
	if err != nil {
		return fmt.Errorf("parsing obs_cut.avs: %w", err)
	}
	multirange := encodeInt8Multirange(ranges, totalMs)
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning CM result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := sqlcgen.New(tx)
	// **結果を書く前に recordings の行をロックする。** チャプターの引き取り
	// （PUT /api/recordings/{id}/chapter-edits）も同じ行を先頭でロックしてから
	// 「検出が終端に達しているか」を評価するので、両者は直列化される。ロックが
	// 無いと READ COMMITTED で条件が文の開始時点のスナップショットから評価され、
	// この commit が見えないまま空の自動層で引き取られる窓が開く
	// （docs/storage/retention.md §7「復元と即時削除要求の競合」と同じ形）。
	if _, err := q.LockRecording(ctx, item.ID); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("locking recording for CM result: %w", err)
	}
	desired, err := q.IsCMDetectionDesired(ctx, item.ID)
	if err != nil {
		return fmt.Errorf("rechecking CM detection policy: %w", err)
	}
	if !desired {
		if err := q.DeleteCMDetectionAttempt(ctx, item.ID); err != nil {
			return fmt.Errorf("clearing disabled CM detection attempt: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing disabled CM detection state: %w", err)
		}
		return nil
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: item.ID, CmRanges: multirange}); err != nil {
		return fmt.Errorf("saving CM ranges: %w", err)
	}
	if err := q.DeleteCMDetectionAttempt(ctx, item.ID); err != nil {
		return fmt.Errorf("clearing CM attempt state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing CM result: %w", err)
	}
	return nil
}

func (w *CMDetectWorker) persistNewStationLogo(ctx context.Context, item sqlcgen.GetCMDetectionWorkItemRow, channel, dir string) error {
	q := sqlcgen.New(w.Pool)
	_, err := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{NetworkID: item.NetworkID, ServiceID: item.ServiceID})
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx5.ErrNoRows) {
		return fmt.Errorf("checking learned station logo: %w", err)
	}
	logo, err := readStationLogo(dir, channel)
	if err != nil {
		return fmt.Errorf("reading newly learned station logo: %w", err)
	}
	// プレビューは見た目の確認用。作れなくてもロゴ自体は保存する（preview_png は nullable）。
	preview, previewErr := lgdPreviewPNG(logo)
	if previewErr != nil {
		slog.Warn("cm_detect: failed to render logo preview", "recording_id", item.ID, "err", previewErr)
		preview = nil
	}
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: item.NetworkID, ServiceID: item.ServiceID, Lgd: logo,
		PreviewPng: preview, LearnedFrom: &item.ID,
	}); err != nil {
		return fmt.Errorf("saving learned station logo: %w", err)
	}
	return nil
}

func cmDetectRuleFile() string { return cmDetectRulePath }

func runCMTool(ctx context.Context, dir, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 4096 {
			message = message[len(message)-4096:]
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(binary), err, message)
	}
	return nil
}

func writeStationLogo(dir, channel string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty LGD")
	}
	name := channel + "-v0001.lgd"
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, channel+".latest"), []byte(name+"\n"), 0o600)
}

func readStationLogo(dir, channel string) ([]byte, error) {
	latest, err := os.ReadFile(filepath.Join(dir, channel+".latest"))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(string(latest))
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".lgd") {
		return nil, fmt.Errorf("invalid latest logo pointer %q", name)
	}
	return os.ReadFile(filepath.Join(dir, name))
}

type frameRange struct{ start, end int64 }

func cmDetectionTimeout(durationMs int64) time.Duration {
	minimum := 30 * time.Minute
	if durationMs <= 0 || durationMs > int64((time.Duration(math.MaxInt64)/2)/time.Millisecond) {
		return minimum
	}
	d := 2 * time.Duration(durationMs) * time.Millisecond
	if d < minimum {
		return minimum
	}
	return d
}

// cmRangesFromCutAVS は obs_cut.avs の Trim() を本編区間とみなし、その補集合を CM として返す。
// durationMs は原本の実尺。
func cmRangesFromCutAVS(avs string, durationMs int64) ([]frameRange, error) {
	total := chapters.MsToFrame(durationMs)
	if total <= 0 {
		return nil, fmt.Errorf("video duration must be positive")
	}
	matches := trimCall.FindAllStringSubmatch(avs, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no Trim() intervals found")
	}
	var main []frameRange
	for _, match := range matches {
		start, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing Trim start frame: %w", err)
		}
		inclusiveEnd, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing Trim end frame: %w", err)
		}
		start = max(0, min(start, total))
		// AviSynth: Trim(a, 0) は終端まで、Trim(a, -n) は a から n フレーム、
		// それ以外は終端フレームを含む区間。
		var end int64
		switch {
		case inclusiveEnd == 0:
			end = total
		case inclusiveEnd < 0:
			end = start - inclusiveEnd
		default:
			end = inclusiveEnd + 1
		}
		end = max(start, min(end, total))
		if end > start {
			main = append(main, frameRange{start: start, end: end})
		}
	}
	main = mergeFrameRanges(main)
	var commercials []frameRange
	cursor := int64(0)
	for _, interval := range main {
		if interval.start > cursor {
			commercials = append(commercials, frameRange{start: cursor, end: interval.start})
		}
		cursor = max(cursor, interval.end)
	}
	if cursor < total {
		commercials = append(commercials, frameRange{start: cursor, end: total})
	}
	return commercials, nil
}

func mergeFrameRanges(ranges []frameRange) []frameRange {
	if len(ranges) == 0 {
		return nil
	}
	// JL output is ordered, but sorting also makes the parser tolerant of reordered Trim calls.
	for i := 1; i < len(ranges); i++ {
		for j := i; j > 0 && ranges[j].start < ranges[j-1].start; j-- {
			ranges[j], ranges[j-1] = ranges[j-1], ranges[j]
		}
	}
	merged := []frameRange{ranges[0]}
	for _, current := range ranges[1:] {
		last := &merged[len(merged)-1]
		if current.start <= last.end {
			last.end = max(last.end, current.end)
		} else {
			merged = append(merged, current)
		}
	}
	return merged
}

func encodeInt8Multirange(ranges []frameRange, durationMs int64) string {
	if len(ranges) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		start := min(chapters.FrameToMs(r.start), durationMs)
		end := min(chapters.FrameToMs(r.end), durationMs)
		if end <= start {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%d,%d)", start, end))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
