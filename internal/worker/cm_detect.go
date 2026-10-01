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
	"github.com/fetburner/rokuban/internal/ffargs"
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
		if markErr := q.MarkCMDetectionFailure(failureCtx, sqlcgen.MarkCMDetectionFailureParams{
			State:       state,
			Stage:       stage,
			Error:       &message,
			RecordingID: job.Args.RecordingID,
		}); markErr != nil {
			return errors.Join(fmt.Errorf("CM detection for recording %d: %w", job.Args.RecordingID, err),
				fmt.Errorf("marking CM detection %s: %w", state, markErr))
		}
		if terminalAdoptionWait {
			return nil
		}
		return fmt.Errorf("CM detection for recording %d: %w", job.Args.RecordingID, err)
	}
	return nil
}

func (w *CMDetectWorker) detect(ctx context.Context, jobID int64, item sqlcgen.GetCMDetectionWorkItemRow) error {
	if item.RelPath == nil {
		return cmFailure("setup", fmt.Errorf("active original is missing"))
	}
	original, err := mediapath.Resolve(w.MediaDir, *item.RelPath)
	if err != nil {
		return cmFailure("setup", fmt.Errorf("resolving original path: %w", err))
	}
	jobRoot := filepath.Join(w.ScratchDir, "cm-detect")
	if err := os.MkdirAll(jobRoot, 0o700); err != nil {
		return cmFailure("setup", fmt.Errorf("creating scratch root: %w", err))
	}
	jobDir := filepath.Join(jobRoot, strconv.FormatInt(jobID, 10))
	if err := os.RemoveAll(jobDir); err != nil {
		return cmFailure("setup", fmt.Errorf("cleaning previous scratch directory: %w", err))
	}
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return cmFailure("setup", fmt.Errorf("creating job scratch directory: %w", err))
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
	channel := fmt.Sprintf("n%d-s%d", item.NetworkID, item.ServiceID)
	// 枠は「次のロゴを作る」意図であり、既存ロゴの検出には使わない。
	// 先に両方の資産を読むことで、枠あり・ロゴなしの録画を logoframe に
	// 渡さず、採用待ちの attempt として明示できる。
	area, err := sqlcgen.New(w.Pool).GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{
		NetworkID: item.NetworkID, ServiceID: item.ServiceID,
	})
	areaExists := err == nil
	if err != nil && !errors.Is(err, pgx5.ErrNoRows) {
		return cmFailure("area", fmt.Errorf("loading taught logo area: %w", err))
	}
	var areaPtr *sqlcgen.GetCMLogoAreaRow
	if areaExists {
		areaPtr = &area
	}
	hadLogo := false
	var startedLogoLearnedAt *time.Time
	if logo, err := sqlcgen.New(w.Pool).GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
		NetworkID: item.NetworkID,
		ServiceID: item.ServiceID,
	}); err == nil {
		hadLogo = true
		learnedAt := logo.LearnedAt
		startedLogoLearnedAt = &learnedAt
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return cmFailure("logo", fmt.Errorf("loading station logo: %w", err))
	}
	if areaPtr != nil && !hadLogo {
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
		logo, err := sqlcgen.New(w.Pool).GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
			NetworkID: item.NetworkID, ServiceID: item.ServiceID,
		})
		if err != nil {
			return cmFailure("logo", fmt.Errorf("reloading station logo: %w", err))
		}
		if logo.CodedWidth != int32(geometry.width) || logo.CodedHeight != int32(geometry.height) {
			return cmFailure("resolution", fmt.Errorf("the station logo is for %dx%d but this recording is %dx%d",
				logo.CodedWidth, logo.CodedHeight, geometry.width, geometry.height))
		}
		if err := writeStationLogo(logoDir, channel, logo.Lgd); err != nil {
			return cmFailure("logo", fmt.Errorf("writing learned station logo: %w", err))
		}
	}
	var observedAreaUpdatedAt *time.Time
	if areaPtr != nil {
		observedAreaUpdatedAt = &areaPtr.UpdatedAt
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
		if err := w.persistNewStationLogo(ctx, item, channel, logoDir, observedAreaUpdatedAt, geometry); err != nil {
			return cmFailure("logo", err)
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
	ranges, err := cmRangesFromCutAVS(string(cutText), totalMs)
	if err != nil {
		return cmFailure("parse", fmt.Errorf("parsing obs_cut.avs: %w", err))
	}
	multirange := encodeInt8Multirange(ranges, totalMs)
	return w.saveCMDetectionResult(ctx, item, hadLogo, startedLogoLearnedAt, multirange)
}

func (w *CMDetectWorker) saveCMDetectionResult(
	ctx context.Context,
	item sqlcgen.GetCMDetectionWorkItemRow,
	hadLogo bool,
	startedLogoLearnedAt *time.Time,
	multirange string,
) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return cmFailure("save", fmt.Errorf("beginning CM result transaction: %w", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: item.NetworkID, ServiceID: item.ServiceID}); err != nil {
		return cmFailure("save", fmt.Errorf("locking station logo state for CM result: %w", err))
	}
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
		return cmFailure("save", fmt.Errorf("locking recording for CM result: %w", err))
	}
	desired, err := q.IsCMDetectionDesired(ctx, item.ID)
	if err != nil {
		return cmFailure("save", fmt.Errorf("rechecking CM detection policy: %w", err))
	}
	if !desired {
		if err := q.DeleteCMDetectionAttempt(ctx, item.ID); err != nil {
			return cmFailure("save", fmt.Errorf("clearing disabled CM detection attempt: %w", err))
		}
		if err := tx.Commit(ctx); err != nil {
			return cmFailure("save", fmt.Errorf("committing disabled CM detection state: %w", err))
		}
		return nil
	}
	if hadLogo {
		currentLogo, logoErr := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
			NetworkID: item.NetworkID, ServiceID: item.ServiceID,
		})
		if errors.Is(logoErr, pgx5.ErrNoRows) ||
			(logoErr == nil && startedLogoLearnedAt != nil && !currentLogo.LearnedAt.Equal(*startedLogoLearnedAt)) {
			if err := q.DeleteCMDetectionAttempt(ctx, item.ID); err != nil {
				return cmFailure("save", fmt.Errorf("discarding stale CM result attempt: %w", err))
			}
			if err := tx.Commit(ctx); err != nil {
				return cmFailure("save", fmt.Errorf("committing stale CM result discard: %w", err))
			}
			return nil
		}
		if logoErr != nil {
			return cmFailure("save", fmt.Errorf("checking station logo version: %w", logoErr))
		}
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: item.ID, CmRanges: multirange}); err != nil {
		return cmFailure("save", fmt.Errorf("saving CM ranges: %w", err))
	}
	if err := q.DeleteCMDetectionAttempt(ctx, item.ID); err != nil {
		return cmFailure("save", fmt.Errorf("clearing CM attempt state: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return cmFailure("save", fmt.Errorf("committing CM result: %w", err))
	}
	return nil
}

// persistNewStationLogo は logoframe が学習したロゴを保存する。呼ぶのはジョブ開始時に
// ロゴが無かったときだけ。**枠の保存（PUT）と直列化し、ロゴ不在と枠の更新時刻が
// ジョブの読んだ値のままであることを INSERT の同じ文で再評価する。** 枠が変わって
// いれば、このロゴは古い枠で学習されたものなので捨てる（次の検出が新しい枠で学習する）。
func (w *CMDetectWorker) persistNewStationLogo(
	ctx context.Context,
	item sqlcgen.GetCMDetectionWorkItemRow,
	channel, dir string,
	observedAreaUpdatedAt *time.Time,
	geometry videoGeometry,
) error {
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
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning learned station logo save: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: item.NetworkID, ServiceID: item.ServiceID}); err != nil {
		return fmt.Errorf("locking station logo state: %w", err)
	}
	n, err := q.InsertLearnedCMLogo(ctx, sqlcgen.InsertLearnedCMLogoParams{
		NetworkID: item.NetworkID, ServiceID: item.ServiceID, Lgd: logo,
		PreviewPng: preview, LearnedFrom: item.ID, ObservedAreaUpdatedAt: observedAreaUpdatedAt,
		CodedWidth: int32(geometry.width), CodedHeight: int32(geometry.height),
	})
	if err != nil {
		return fmt.Errorf("saving learned station logo: %w", err)
	}
	if n == 0 {
		slog.Info("cm_detect: discarded a learned logo because the station logo state changed during the job",
			"recording_id", item.ID, "network_id", item.NetworkID, "service_id", item.ServiceID)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing learned station logo: %w", err)
	}
	return nil
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
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
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
	output, err := cmd.CombinedOutput()
	if err != nil {
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
// **上書きが原子的でないことは問題にならない。** dir は呼び出し元が作る job ID
// ごとの scratch（`{scratch}/cm-detect/{job_id}/logos`）で、同じ (recording) の
// 並走実行も別ジョブ ID なので別ディレクトリになる（job lock を失った実行と
// reconcile の代替実行が並走しうるのは encode と同じだが、衝突するのは scratch の
// 中身だけである）。読み手も同じ実行の readStationLogo だけで、書いた内容は
// そのまま DB の Upsert へ渡す。encode の公開のように共有の canonical へ書く経路が
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
