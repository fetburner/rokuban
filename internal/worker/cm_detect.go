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

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
)

const (
	cmDetectMaxTries       = 3
	cmDetectRowLimit int32 = 1000
	cmDetectRulePath       = config.CMDetectRulePath
)

var trimCall = regexp.MustCompile(`Trim\s*\(\s*(-?\d+)\s*,\s*(-?\d+)\s*\)`)

var cmLogoMatchLine = regexp.MustCompile(`(?i)managed\s+logo:\s+v\d+\s+match=([0-9]+(?:\.[0-9]+)?)%`)

// cmDetectMinLogoMatchPercent は logoframe の一致率がこれ未満なら、ロゴが
// 録画にほとんど映っていないとみなす下限。判定は logoframe の -logo-match
// ではなく rokuban が成功出力を読んで行う。
//
// 未検証: 10.0 は実録画の測定に基づかない暫定値で、正常な局ロゴの最小値と
// 異解像度ロゴの値のどちらも測っていない。測定で分離を確かめるまで、
// この値が正常な録画を弾かない・誤った成功を止めるとは言えない。
const cmDetectMinLogoMatchPercent = 10.0

// cmDetectFailure keeps the worker-observed failure stage next to the error that
// caused it. The stage is an observation, not a best-effort derivation from the
// tool's free-form stderr.
type cmDetectFailure struct {
	stage string
	err   error
}

func (e *cmDetectFailure) Error() string { return e.err.Error() }

func (e *cmDetectFailure) Unwrap() error { return e.err }

func cmFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &cmDetectFailure{stage: stage, err: err}
}

func cmFailureStage(err error) *string {
	var failure *cmDetectFailure
	if !errors.As(err, &failure) {
		return nil
	}
	stage := failure.stage
	return &stage
}

// CMDetectWorker analyzes a committed original and stores its commercial ranges.
type CMDetectWorker struct {
	Enqueuer       jobq.Enqueuer
	DetectionStore store.CMDetection
	MediaDir       string
	ScratchDir     string
	CMDetect       config.CMDetectConfig
	// FFprobe は原本の実尺を読む ffprobe のパス（空なら PATH の ffprobe）。
	FFprobe string
}

// Work learns a missing station logo, runs JL analysis, and commits only a complete result.
func (w *CMDetectWorker) Work(ctx context.Context, args jobs.CMDetectJobArgs) error {
	started := time.Now()
	defer func() { metrics.CMDetectDuration.Observe(time.Since(started).Seconds()) }()

	item, err := w.DetectionStore.GetWorkItem(ctx, args.RecordingID)
	if err != nil {
		return fmt.Errorf("CM detection: loading recording %d: %w", args.RecordingID, err)
	}
	if !item.Enabled || item.IsTrashed || item.OriginalMissing || item.Detected || item.RelPath == nil {
		return nil
	}
	desired, err := w.DetectionStore.IsDetectionDesired(ctx, args.RecordingID)
	if err != nil {
		return fmt.Errorf("CM detection: checking desired state for recording %d: %w", args.RecordingID, err)
	}
	if !desired {
		return nil
	}
	attempt, shouldRun, err := w.DetectionStore.BeginAttempt(ctx, args.RecordingID, cmDetectMaxTries)
	if err != nil {
		return fmt.Errorf("CM detection: beginning attempt for recording %d: %w", args.RecordingID, err)
	}
	if !shouldRun {
		return nil
	}
	if err := w.detect(ctx, item, attempt); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			failureCtx, failureCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer failureCancel()
			// 世代は試行行自身の識別子。方針表をロックせず、世代の違う行には何も書かない。
			var cleanupErr error
			if attempt.AttemptCount == 1 {
				cleanupErr = w.DetectionStore.AbortAttempt(failureCtx, attempt)
			} else {
				cleanupErr = w.DetectionStore.CancelAttempt(failureCtx, attempt)
			}
			if cleanupErr != nil {
				return errors.Join(err, fmt.Errorf("clearing CM detection running state: %w", cleanupErr))
			}
			return err
		}
		state := "retrying"
		if attempt.AttemptCount >= cmDetectMaxTries {
			state = "failed"
		}
		message := err.Error()
		stage := cmFailureStage(err)
		terminalAdoptionWait := stage != nil && *stage == "adopt"
		if terminalAdoptionWait {
			// A station with a taught area but no adopted logo is waiting for a
			// human decision. Retrying this recording cannot make progress and
			// would keep producing the same attempt until the candidate is adopted.
			state = "failed"
		}
		failureCtx, failureCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer failureCancel()
		marked, markErr := w.DetectionStore.FailAttempt(failureCtx, attempt, state, stage, message)
		if markErr != nil {
			return errors.Join(fmt.Errorf("CM detection for recording %d: %w", args.RecordingID, err),
				fmt.Errorf("marking CM detection %s: %w", state, markErr))
		}
		if !marked {
			return nil
		}
		if terminalAdoptionWait {
			return nil
		}
		return fmt.Errorf("CM detection for recording %d: %w", args.RecordingID, err)
	}
	return nil
}

func (w *CMDetectWorker) detect(
	ctx context.Context,
	item store.CMDetectionWorkItem,
	attempt store.CMDetectionAttempt,
) error {
	recordingID := item.ID
	if item.RelPath == nil {
		return cmFailure("setup", fmt.Errorf("active original is missing"))
	}
	original, err := mediapath.Resolve(w.MediaDir, *item.RelPath)
	if err != nil {
		return cmFailure("setup", fmt.Errorf("resolving original path: %w", err))
	}
	jobDir, err := newDomainScratchDir(w.ScratchDir, "cm-detect", fmt.Sprintf("%d-%d", recordingID, attempt.AttemptCount))
	if err != nil {
		return cmFailure("setup", fmt.Errorf("creating scratch directory: %w", err))
	}
	defer func() {
		if err := os.RemoveAll(jobDir); err != nil {
			slog.Warn("cm_detect: failed to remove scratch directory", "path", jobDir, "err", err)
		}
	}()

	inputPath := filepath.Join(jobDir, "input.ts")
	if err := os.Symlink(original, inputPath); err != nil {
		return cmFailure("setup", fmt.Errorf("linking original into scratch: %w", err))
	}
	channel := fmt.Sprintf("n%d-s%d", item.Station.NetworkID, item.Station.ServiceID)
	// 枠は「次のロゴを作る」意図であり、既存ロゴの検出には使わない。
	// 先に両方の資産を読むことで、枠あり・ロゴなしの録画を logoframe に
	// 渡さず、採用待ちの attempt として明示できる。
	area, err := w.DetectionStore.GetLogoArea(ctx, item.Station)
	areaExists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return cmFailure("setup", fmt.Errorf("loading taught logo area: %w", err))
	}
	var areaVersion *time.Time
	if areaExists {
		updatedAt := area.UpdatedAt
		areaVersion = &updatedAt
	}
	hadLogo := false
	if logo, err := w.DetectionStore.GetLogo(ctx, item.Station); err == nil {
		hadLogo = true
		learnedAt := logo.LearnedAt
		attempt.LogoLearnedAt = &learnedAt
	} else if !errors.Is(err, store.ErrNotFound) {
		return cmFailure("logo", fmt.Errorf("loading station logo: %w", err))
	}
	if areaExists && !hadLogo {
		return cmFailure("adopt", fmt.Errorf("the station has a taught logo area but no adopted logo"))
	}
	// 人が教えた枠は記録上の解像度の座標なので、まず原本の実際の大きさを取る。
	// poster やシークタイルの座標は使えない（あちらは SAR を焼き込んでいる）。
	geometry, err := probeVideoGeometry(ctx, commandOutput, w.FFprobe, inputPath)
	if err != nil {
		return cmFailure("probe", fmt.Errorf("probing original size: %w", err))
	}
	logoDir := filepath.Join(jobDir, "logos")
	if err := os.Mkdir(logoDir, 0o700); err != nil {
		return cmFailure("setup", fmt.Errorf("creating temporary logo directory: %w", err))
	}
	if hadLogo {
		logo, err := w.DetectionStore.GetLogo(ctx, item.Station)
		if err != nil {
			return cmFailure("logo", fmt.Errorf("reloading station logo: %w", err))
		}
		if logo.CodedWidth != int32(geometry.width) || logo.CodedHeight != int32(geometry.height) {
			return cmFailure("resolution", fmt.Errorf("the station logo is for %dx%d but this recording is %dx%d",
				logo.CodedWidth, logo.CodedHeight, geometry.width, geometry.height))
		}
		if err := writeStationLogo(logoDir, channel, logo.LGD); err != nil {
			return cmFailure("logo", fmt.Errorf("writing learned station logo: %w", err))
		}
	}
	logoFrames := filepath.Join(jobDir, "logoframe.txt")
	chapters := filepath.Join(jobDir, "chapter_exe.txt")
	cutAvs := filepath.Join(jobDir, "obs_cut.avs")
	tools := func(name string) string { return filepath.Join(w.CMDetect.BinaryDir, name) }
	logoArgs := []string{inputPath, "-channel", channel, "-logo-dir", logoDir,
		"-logo-match", "0", "-oa", logoFrames}
	logoframeOutput, err := runCMTool(ctx, jobDir, tools("logoframe"), logoArgs...)
	if err != nil {
		return cmFailure("logo", fmt.Errorf("running logoframe: %w", err))
	}
	matchPercent, err := cmLogoMatchPercent(logoframeOutput)
	if err != nil {
		return cmFailure("logo", err)
	}
	if matchPercent < cmDetectMinLogoMatchPercent {
		return cmFailure("match", fmt.Errorf("station logo match %.2f%% is below %.2f%%", matchPercent, cmDetectMinLogoMatchPercent))
	}
	if !hadLogo {
		learnedAt, err := w.persistNewStationLogo(ctx, attempt, channel, logoDir, areaVersion, geometry)
		if err != nil {
			return cmFailure("logo", err)
		}
		if learnedAt != nil {
			attempt.LogoLearnedAt = learnedAt
		}
	}
	if _, err := runCMTool(ctx, jobDir, tools("chapter_exe"), "-v", inputPath, "-s", "8", "-e", "4", "-o", chapters); err != nil {
		return cmFailure("chapter", fmt.Errorf("running chapter_exe: %w", err))
	}
	if _, err := runCMTool(ctx, jobDir, tools("join_logo_scp"), "-inlogo", logoFrames,
		"-inscp", chapters, "-incmd", cmDetectRuleFile(), "-o", cutAvs); err != nil {
		return cmFailure("join", fmt.Errorf("running join_logo_scp: %w", err))
	}
	cutText, err := os.ReadFile(cutAvs)
	if err != nil {
		return cmFailure("parse", fmt.Errorf("reading obs_cut.avs: %w", err))
	}
	// 総尺は EPG の尺ではなく原本の実尺から取る。録画は EIT 追従で延長されうるので、
	// program_duration_ms で打ち切ると延長分の本編と CM を捨てる。
	videoDuration, err := probeVideoDuration(ctx, commandOutput, w.FFprobe, inputPath)
	if err != nil {
		return cmFailure("probe", fmt.Errorf("probing original duration: %w", err))
	}
	totalMs := videoDuration.Milliseconds()
	frameRanges, err := cmRangesFromCutAVS(string(cutText), totalMs)
	if err != nil {
		return cmFailure("parse", fmt.Errorf("parsing obs_cut.avs: %w", err))
	}
	ranges := chapterRangesFromFrames(frameRanges, totalMs)
	saved, err := w.DetectionStore.SaveDetection(ctx, attempt, ranges)
	if err != nil {
		return cmFailure("save", err)
	}
	if saved {
		// CM ranges make an existing thumbnail eligible for reselection. This is only
		// a hint; thumbnail_reconcile remains the durable backstop if enqueueing fails.
		if enqueueErr := enqueueJob(ctx, w.Enqueuer, jobs.ThumbnailJobArgs{RecordingID: item.ID}); enqueueErr != nil {
			slog.Warn("cm_detect: failed to enqueue thumbnail reselection hint",
				"recording_id", item.ID, "err", enqueueErr)
		}
	}
	return nil
}

// persistNewStationLogo は logoframe が学習したロゴを保存する。呼ぶのはジョブ開始時に
// ロゴが無かったときだけ。**枠の保存（PUT）と直列化し、ロゴ不在と枠の更新時刻が
// ジョブの読んだ値のままであることを INSERT の同じ文で再評価する。** 枠が変わって
// いれば、このロゴは古い枠で学習されたものなので捨てる（次の検出が新しい枠で学習する）。
func (w *CMDetectWorker) persistNewStationLogo(
	ctx context.Context,
	attempt store.CMDetectionAttempt,
	channel, dir string,
	observedAreaUpdatedAt *time.Time,
	geometry videoGeometry,
) (*time.Time, error) {
	logo, err := readStationLogo(dir, channel)
	if err != nil {
		return nil, fmt.Errorf("reading newly learned station logo: %w", err)
	}
	// プレビューは見た目の確認用。作れなくてもロゴ自体は保存する（preview_png は nullable）。
	preview, previewErr := lgdPreviewPNG(logo)
	if previewErr != nil {
		slog.Warn("cm_detect: failed to render logo preview", "recording_id", attempt.RecordingID, "err", previewErr)
		preview = nil
	}
	learnedAt, saved, err := w.DetectionStore.SaveLearnedLogo(ctx, attempt, store.CMDetectionLogo{
		LGD: logo, PreviewPNG: preview,
		CodedWidth: int32(geometry.width), CodedHeight: int32(geometry.height),
	}, observedAreaUpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("saving learned station logo: %w", err)
	}
	if !saved {
		slog.Info("cm_detect: discarded a learned logo because the station logo state changed during the job",
			"recording_id", attempt.RecordingID, "network_id", attempt.Station.NetworkID, "service_id", attempt.Station.ServiceID)
	}
	return learnedAt, nil
}

// videoGeometry は映像ストリームの記録上の大きさ（SAR を掛ける前の画素数）。
type videoGeometry struct{ width, height int }

// probeVideoGeometry は最初の映像ストリームの大きさを返す。run は commandOutput か、
// テストで差し替えた実行フック。問い合わせと出力の読み方は ffargs にあり、
// streamer の /frame と共有する。
//
// **stream=width,height は SAR を掛けない。** 1440x1080 の地上波 HD は SAR 4:3 でも
// width=1440 を返す（SAR は stream=sample_aspect_ratio 側）。人が教える枠も
// logoframe が見るのもこの座標なので、ここが基準になる。
func probeVideoGeometry(
	ctx context.Context,
	run func(context.Context, string, ...string) ([]byte, error),
	ffprobe, inputPath string,
) (videoGeometry, error) {
	ffprobe = ffargs.FFprobePath(ffprobe)
	out, err := run(ctx, ffprobe, ffargs.VideoGeometryProbeArgs(inputPath)...)
	if err != nil {
		return videoGeometry{}, err
	}
	width, height, err := ffargs.ParseVideoGeometry(out)
	if err != nil {
		return videoGeometry{}, err
	}
	return videoGeometry{width: width, height: height}, nil
}

func cmDetectRuleFile() string { return cmDetectRulePath }

func runCMTool(ctx context.Context, dir, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "HOME=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+dir)
	// 孫プロセスが stdout を握ったままでも、ctx キャンセル後に Work が戻れるようにする。
	setWorkerExecWaitDelay(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// ctx キャンセルを WaitDelay-success 分岐より先に見る（commandOutput と同型）。
		if ctx.Err() != nil {
			return output, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			slog.Warn("runCMTool: process exited successfully but WaitDelay expired before I/O completed",
				"binary", filepath.Base(binary), "wait_delay", workerExecWaitDelay)
			return output, nil
		}
		message := strings.TrimSpace(string(output))
		if len(message) > 4096 {
			message = message[len(message)-4096:]
		}
		return output, fmt.Errorf("%s: %w: %s", filepath.Base(binary), err, message)
	}
	return output, nil
}

func cmLogoMatchPercent(output []byte) (float64, error) {
	match := cmLogoMatchLine.FindSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("logoframe output does not contain a managed logo match percentage")
	}
	percent, err := strconv.ParseFloat(string(match[1]), 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		if err == nil {
			err = fmt.Errorf("match percentage is outside 0..100 or not finite")
		}
		return 0, fmt.Errorf("invalid logoframe match percentage %q: %w", match[1], err)
	}
	return percent, nil
}

// writeStationLogo は学習済みの局ロゴを dir へ書く。
//
// **上書きが原子的でないことは問題にならない。** dir は呼び出し元が作る domain
// attempt ごとの scratch（`{scratch}/cm-detect/{recording_id}-{attempt}/logos`）で、
// timeout 後に queue が再配送した実行は新しい domain attempt を使う。古い Work が
// 並走しても scratch は分かれる。読み手も同じ実行の
// readStationLogo だけで、書いた内容はそのまま DB の Upsert へ渡す。encode の公開のように共有の canonical へ書く経路が
// 無いので、ここへ temp + rename は持ち込まない。
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

// readStationLogo は .latest が指す LGD を読む。logoframe は
// "%d\n%s\n"（version 行 + 名前行）で書き、rokuban 自身は名前 1 行で書く。
// どちらも最後の非空行が名前なので、それを取る。
func readStationLogo(dir, channel string) ([]byte, error) {
	latest, err := os.ReadFile(filepath.Join(dir, channel+".latest"))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(latest)), "\n")
	name := strings.TrimSpace(lines[len(lines)-1])
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".lgd") {
		return nil, fmt.Errorf("invalid latest logo pointer %q", name)
	}
	return os.ReadFile(filepath.Join(dir, name))
}

type frameRange struct{ start, end int64 }

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

func chapterRangesFromFrames(ranges []frameRange, durationMs int64) []chapters.Range {
	result := make([]chapters.Range, 0, len(ranges))
	for _, r := range ranges {
		start := min(chapters.FrameToMs(r.start), durationMs)
		end := min(chapters.FrameToMs(r.end), durationMs)
		if end > start {
			result = append(result, chapters.Range{StartMs: start, EndMs: end})
		}
	}
	return result
}
