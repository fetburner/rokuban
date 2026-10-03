package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// サムネイルの代表フレーム位置ポリシー（固定。設定キーは設けない）:
//
//	seek = min(duration × 10%, 30s)
//
// オープニング直後のロゴ寄りを避けつつ、長尺でも 30 秒で頭打ちにする。
// duration が取れない / 0 のときは 0 秒（先頭フレーム）に落とす。
// docs/storage/contract.md §5.1。
const (
	thumbnailSeekFraction = 0.10
	thumbnailSeekMax      = 30 * time.Second
	thumbnailTimeout      = 5 * time.Minute
)

// ThumbnailWorker は原本または利用可能なエンコード版から代表フレームを JPEG 抽出し、
// media_assets（kind = 'thumbnail'）としてコミットする。
//
// ストレージ契約: ジョブ固有 scratch に ffmpeg 出力 → 同じディレクトリの staged file
// へ fsync → rel_path lock と DB transaction の下で DB 行予約 → rename + 親 dir fsync
// → commit（docs/storage/contract.md §3）。
//
// site 照合ガード（issue #139）は不要と判断: EncodeWorker と同じ理由
// （ThumbnailJobArgs は recording_id のみで site を持たず、原本読み取りは
// mediapath.Resolve 経由の単一 MediaDir、mirakc には触れない）。EncodeWorker
// の doc コメント参照。
type ThumbnailWorker struct {
	river.WorkerDefaults[jobs.ThumbnailJobArgs]

	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	FFmpeg     string
	FFprobe    string

	// runCmd はテストで差し替える実行フック。nil なら exec.CommandContext。
	// stdout を返す（ffprobe）。ffmpeg はファイル副作用だけを使う。
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type thumbnailWorkPlan struct {
	inputID   int64
	inputPath string
	inputSeek time.Duration
	seekMs    int64
	relPath   string
	expected  *thumbnailObserved
	replaced  string
}

// Timeout はサムネイル 1 件の上限。大容量 TS への入力シークでも 5 分あれば足りる。
func (w *ThumbnailWorker) Timeout(*river.Job[jobs.ThumbnailJobArgs]) time.Duration {
	return thumbnailTimeout
}

// Work は thumbnail ジョブを実行する。
//
// レベルトリガー: 初回生成では active original が無ければ成功扱いで終える。
// 既存 thumbnail は、チャプター上で抽出位置が CM 区間に入ったときだけ選び直す。
func (w *ThumbnailWorker) Work(ctx context.Context, job *river.Job[jobs.ThumbnailJobArgs]) error {
	recordingID := job.Args.RecordingID
	log := slog.With("recording_id", recordingID, "job", "thumbnail")

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.ThumbnailDuration.Observe(time.Since(started).Seconds())
		metrics.ThumbnailJobs.WithLabelValues(result).Inc()
	}()

	q := sqlcgen.New(w.Pool)

	plan, ready, err := w.buildThumbnailPlan(ctx, q, recordingID, log)
	if err != nil {
		return err
	}
	if !ready {
		result = "success"
		return nil
	}

	scratchDir, err := newWorkerScratchDir(w.ScratchDir, "thumbnail", job.ID, job.Attempt)
	if err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()
	scratchPath := filepath.Join(scratchDir, "thumbnail.jpg")

	if err := w.extractFrame(ctx, plan.inputPath, scratchPath, plan.inputSeek); err != nil {
		return fmt.Errorf("extracting frame: %w", err)
	}

	info, err := os.Stat(scratchPath)
	if err != nil {
		return fmt.Errorf("stat scratch thumbnail: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch thumbnail is empty")
	}

	size, published, err := publishGeneratedMediaAsset(ctx, w.Pool, w.MediaDir, plan.relPath, scratchPath,
		func(ctx context.Context, q *sqlcgen.Queries) (bool, error) {
			return skipThumbnailPlanPublish(ctx, q, recordingID, plan.expected, plan.inputID)
		},
		func(ctx context.Context, q *sqlcgen.Queries, size int64) error {
			var assetID int64
			if plan.expected == nil {
				var err error
				assetID, err = q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
					RecordingID: recordingID, RelPath: plan.relPath, SizeBytes: size,
				})
				if err != nil {
					return err
				}
			} else {
				assetID = plan.expected.ID
				if err := q.UpdateThumbnailMediaAssetPath(ctx, sqlcgen.UpdateThumbnailMediaAssetPathParams{
					RelPath: plan.relPath, SizeBytes: size, ID: assetID,
				}); err != nil {
					return err
				}
			}
			return q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
				MediaAssetID: assetID,
				SeekMs:       plan.seekMs,
			})
		})
	if err != nil {
		return fmt.Errorf("publishing thumbnail: %w", err)
	}
	if !published {
		log.Info("thumbnail: already committed or original no longer active, skipping")
		result = "success"
		return nil
	}
	if plan.replaced != "" {
		w.removeReplacedThumbnail(plan.replaced, log)
	}

	log.Info("thumbnail: committed", "rel_path", plan.relPath, "size_bytes", size,
		"input_seek_ms", plan.inputSeek.Milliseconds(), "seek_ms", plan.seekMs)
	result = "success"
	return nil
}

func (w *ThumbnailWorker) buildThumbnailPlan(
	ctx context.Context,
	q *sqlcgen.Queries,
	recordingID int64,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	_, err := q.GetActiveThumbnailMediaAssetID(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		// ごみ箱は commit 時の skipThumbnailPlanPublish が LockRecording で弾く。
		return w.initialThumbnailPlan(ctx, q, recordingID, log)
	}
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("checking thumbnail: %w", err)
	}
	// 候補クエリは reconcile と同じ 1 本。ごみ箱・チャプター無し・purged は除外済み。
	rows, err := q.ListThumbnailReselectCandidates(ctx, sqlcgen.ListThumbnailReselectCandidatesParams{
		AfterRecordingID: recordingID - 1,
		RowLimit:         1,
	})
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("loading thumbnail planning state: %w", err)
	}
	if len(rows) == 0 || rows[0].RecordingID != recordingID {
		log.Info("thumbnail: not a reselection candidate, skipping")
		return thumbnailWorkPlan{}, false, nil
	}
	return w.reselectionThumbnailPlan(rows[0], log)
}

func (w *ThumbnailWorker) initialThumbnailPlan(
	ctx context.Context,
	q *sqlcgen.Queries,
	recordingID int64,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	// 初回の仮サムネイルは従来どおり原本だけを使い、CM 検出を待たずに作る。
	orig, err := q.GetActiveOriginalMediaAsset(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		log.Info("thumbnail: no active original, skipping")
		return thumbnailWorkPlan{}, false, nil
	}
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("loading original media asset: %w", err)
	}
	inputPath, err := mediapath.Resolve(w.MediaDir, orig.RelPath)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("resolving original path: %w", err)
	}
	duration, err := w.probeDuration(ctx, inputPath)
	if err != nil {
		// 長さが取れなくても先頭フレームで続行する（壊れたメタデータへの保険）。
		log.Warn("thumbnail: ffprobe duration failed, seeking to 0", "err", err)
		duration = 0
	}
	seek := thumbnailSeek(duration)
	seekMs := int64(math.Round(float64(seek) / float64(time.Millisecond)))
	inputSeek := time.Duration(seekMs) * time.Millisecond
	return thumbnailWorkPlan{
		inputID: orig.ID, inputPath: inputPath, inputSeek: inputSeek,
		seekMs: seekMs, relPath: thumbnailRelPath(recordingID),
	}, true, nil
}

func (w *ThumbnailWorker) reselectionThumbnailPlan(
	state sqlcgen.ListThumbnailReselectCandidatesRow,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	timeline, hasTimeline, err := thumbnailTimeline(state)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("decoding chapter timeline: %w", err)
	}
	if !hasTimeline {
		log.Info("thumbnail: no chapter timeline, keeping current thumbnail")
		return thumbnailWorkPlan{}, false, nil
	}
	inputs, err := thumbnailInputsOf(state)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("decoding thumbnail inputs: %w", err)
	}
	if !thumbnailNeedsReselect(state.SeekMs, inputs, timeline) {
		log.Info("thumbnail: no reselection needed or no usable input, skipping")
		return thumbnailWorkPlan{}, false, nil
	}
	plan, ok := thumbnailPlanForInputs(inputs, chapters.KeepRanges(timeline))
	if !ok {
		// 入力が戻るまで同じジョブを失敗扱いにせず、reconcile の次の窓に任せる。
		log.Info("thumbnail: no usable input, skipping")
		return thumbnailWorkPlan{}, false, nil
	}
	inputPath, err := mediapath.Resolve(w.MediaDir, plan.Source.RelPath)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("resolving thumbnail input path: %w", err)
	}
	if state.ThumbnailRelPath == nil || state.ThumbnailMediaAssetID == nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("active thumbnail planning state is incomplete")
	}
	expected := &thumbnailObserved{
		ID:      *state.ThumbnailMediaAssetID,
		RelPath: *state.ThumbnailRelPath,
		SeekMs:  state.SeekMs,
	}
	relPath, err := w.nextUnusedThumbnailRelPath(state.RecordingID, expected.RelPath)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("choosing thumbnail replacement path: %w", err)
	}
	return thumbnailWorkPlan{
		inputID: plan.Source.MediaAssetID, inputPath: inputPath,
		inputSeek: time.Duration(plan.InputSeekMs) * time.Millisecond,
		seekMs:    plan.RecordedSeek, relPath: relPath,
		expected: expected, replaced: expected.RelPath,
	}, true, nil
}

// thumbnailRelPath は初回サムネイルの相対パスを返す。recording_id を使うため、
// 原本の contentPath や原本削除に依存しない。
func thumbnailRelPath(recordingID int64) string {
	return fmt.Sprintf("thumbnails/%d.jpg", recordingID)
}

// nextThumbnailRelPath gives each replacement a never-reused path. The initial
// thumbnail keeps the legacy path without a generation suffix.
func nextThumbnailRelPath(recordingID int64, previous string) string {
	stem := strings.TrimSuffix(filepath.Base(previous), filepath.Ext(previous))
	prefix := fmt.Sprintf("%d.g", recordingID)
	generation := 1
	if strings.HasPrefix(stem, prefix) {
		if n, err := strconv.Atoi(strings.TrimPrefix(stem, prefix)); err == nil && n > 0 {
			generation = n + 1
		}
	}
	return fmt.Sprintf("thumbnails/%d.g%d.jpg", recordingID, generation)
}

func (w *ThumbnailWorker) nextUnusedThumbnailRelPath(recordingID int64, previous string) (string, error) {
	candidate := nextThumbnailRelPath(recordingID, previous)
	for {
		path, err := mediapath.Resolve(w.MediaDir, candidate)
		if err != nil {
			return "", err
		}
		_, err = os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		candidate = nextThumbnailRelPath(recordingID, candidate)
	}
}

// thumbnailSeek は代表フレーム位置を返す（min(duration×10%, 30s)）。
func thumbnailSeek(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	seek := time.Duration(float64(duration) * thumbnailSeekFraction)
	if seek > thumbnailSeekMax {
		return thumbnailSeekMax
	}
	return seek
}

type thumbnailObserved struct {
	ID      int64
	RelPath string
	SeekMs  *int64
}

// skipThumbnailPlanPublish rechecks the observed row under the same transaction
// that publishes the new file. For replacements it locks the existing media row so
// two workers cannot publish over the same generation.
func skipThumbnailPlanPublish(
	ctx context.Context,
	q *sqlcgen.Queries,
	recordingID int64,
	expected *thumbnailObserved,
	inputAssetID int64,
) (bool, error) {
	recording, err := q.LockRecording(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if recording.IsTrashed || recording.IsPurged {
		return true, nil
	}

	if expected == nil {
		if _, err := q.GetActiveThumbnailMediaAssetID(ctx, recordingID); err == nil {
			return true, nil
		} else if !errors.Is(err, pgx5.ErrNoRows) {
			return false, err
		}
		original, err := q.GetActiveOriginalMediaAsset(ctx, recordingID)
		if errors.Is(err, pgx5.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return original.ID != inputAssetID, nil
	}

	current, err := q.LockActiveThumbnailMediaAsset(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if current.ID != expected.ID || current.RelPath != expected.RelPath || !sameThumbnailSeek(current.SeekMs, expected.SeekMs) {
		return true, nil
	}
	active, err := q.IsActiveThumbnailInput(ctx, inputAssetID)
	if err != nil {
		return false, err
	}
	return !active, nil
}

func sameThumbnailSeek(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (w *ThumbnailWorker) removeReplacedThumbnail(relPath string, log *slog.Logger) {
	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Warn("thumbnail: could not resolve replaced path", "rel_path", relPath, "err", err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("thumbnail: removing replaced file failed; orphan collection will pick it up",
			"rel_path", relPath, "err", err)
	}
}

func (w *ThumbnailWorker) probeDuration(ctx context.Context, inputPath string) (time.Duration, error) {
	return probeDuration(ctx, w.FFprobe, inputPath, w.commandOutput)
}

func probeDuration(ctx context.Context, ffprobe, inputPath string, run func(context.Context, string, ...string) ([]byte, error)) (time.Duration, error) {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	}
	out, err := run(ctx, ffprobe, args...)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("ffprobe returned empty duration")
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duration %q: %w", s, err)
	}
	if sec < 0 {
		return 0, fmt.Errorf("negative duration %v", sec)
	}
	return time.Duration(sec * float64(time.Second)), nil
}

func (w *ThumbnailWorker) extractFrame(ctx context.Context, inputPath, outputPath string, seek time.Duration) error {
	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	// -ss を -i の前に置き入力シーク（大容量 TS で速い）。
	// -frames:v 1 で 1 枚、-q:v 2 で高品質 JPEG。
	// SAR（ピクセル縦横比）を偶数幅の正方形ピクセルへ焼き込む。JPEG は SAR を
	// 運ばないため、これが無いと anamorphic な地デジ（1440x1080 SAR 4:3 →
	// DAR 16:9）がブラウザで横に潰れて見える。解像度は見ず SAR だけで正規化するので
	// BS の 1920x1080 SAR 1:1 は no-op。setsar=1 は端数丸め後も正方形を保証する。
	args := []string{
		"-y",
		"-ss", formatSeekSeconds(seek),
		"-i", inputPath,
		"-frames:v", "1",
		"-vf", "scale=round(iw*sar/2)*2:ih,setsar=1",
		"-q:v", "2",
		outputPath,
	}
	if _, err := w.commandOutput(ctx, ffmpeg, args...); err != nil {
		return err
	}
	return nil
}

func formatSeekSeconds(d time.Duration) string {
	// ffmpeg は秒の小数を受け付ける。整数秒で足りるが 10% 計算の端数を残す。
	return strconv.FormatFloat(d.Seconds(), 'f', 3, 64)
}

func (w *ThumbnailWorker) commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if w.runCmd != nil {
		return w.runCmd(ctx, name, args...)
	}
	return commandOutput(ctx, name, args...)
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	setWorkerExecWaitDelay(cmd)
	// ffprobe の stdout は duration や stream index などの機械可読な値を
	// 返す。stderr の診断メッセージを混ぜると、その値をパースできなくなるため、
	// stdout だけを返し、stderr はコマンド失敗時の診断にだけ使う。
	out, err := cmd.Output()
	if err != nil {
		// ctx キャンセルを WaitDelay-success 分岐より先に見る（encode.go の
		// runEncode と同型）。watchCtx の Cancel が os.ErrProcessDone を返す
		// 競合（プロセスは既に exit 0 していた）では err は一旦 nil のままで、
		// 後から WaitDelay の分岐だけが ErrWaitDelay を立てる。ここで
		// ctx.Err() を先に見ないと、River のシャットダウンが ffmpeg/ffprobe の
		// exit 0 直後に当たったケースを黙って成功扱いにしてしまう。
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// exit 0 の完走後、孫プロセスが fd を握ったままで WaitDelay が
			// 先に切れた場合（encode.go の runEncode と同型）。コピー
			// goroutine はプロセスが書いた分をプロセス生存中に drain し
			// 続けているので、out がプロセス自身の出力より短く切れることは
			// ない。到達しうるのは逆方向 --- fd を継承した孫プロセスが
			// 強制クローズまでの WaitDelay の窓の間に out へ追記しうること。
			// extractFrame の呼び出しでは out 自体を捨てるので無害。
			// probeDuration は out を ParseFloat するので、追記があれば
			// パース自体が失敗する（未観測 --- 実運用の ffprobe 出力はごく
			// 短時間で読み切れるため、この窓に孫が居合わせた例は無い）。
			// 再試行ループから見分けられるよう記録は残す。
			slog.Warn("commandOutput: process exited successfully but WaitDelay expired before I/O completed",
				"name", name, "wait_delay", workerExecWaitDelay)
			return out, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return out, fmt.Errorf("%s %v: %w\nstderr: %s", name, args, err, truncateOutput([]byte(stderr)))
			}
		}
		return out, fmt.Errorf("%s %v: %w", name, args, err)
	}
	return out, nil
}

func truncateOutput(b []byte) string {
	const max = 2 << 10
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// copyScratchFileFsync は scratch 上の src を scratch 上の dst へコピーし、dst を fsync する。
// 公開アセットへのコピーは publishGeneratedMediaAsset の stage + rename 経路を使う。
func copyScratchFileFsync(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create dst: %w", err)
	}
	// Close 前に Sync。Close のエラーも返す。
	var copyErr error
	n, err := io.Copy(out, in)
	if err != nil {
		copyErr = fmt.Errorf("copy: %w", err)
	} else if err := out.Sync(); err != nil {
		copyErr = fmt.Errorf("fsync: %w", err)
	}
	if closeErr := out.Close(); closeErr != nil && copyErr == nil {
		copyErr = fmt.Errorf("close: %w", closeErr)
	}
	if copyErr != nil {
		_ = os.Remove(dst)
		return 0, copyErr
	}
	return n, nil
}

// EnqueueThumbnailIfNeeded は original があり active thumbnail が無く、かつ
// ごみ箱に入っていないときだけ unique な thumbnail ジョブを投入する
// （レベルトリガー。issue #66）。
//
// 既に thumbnail がある・original が無い・ごみ箱（recordings.deleted_at）に
// 入っている場合は no-op。River の UniqueOpts が進行中ジョブの二重投入も吸収する。
//
// ごみ箱チェックは ListRecordingIDsMissingThumbnail（issue #109）と条件を揃える
// （docs/storage.md §5.1）。呼び出し元は ingest 直後の original コミット後のみ
// だが、SoftDeleteRecording（internal/db/queries/recordings_trash.sql）に
// status ガードは無く ingest 進行中の録画もごみ箱に入れられるため、到達しうる
// 経路として扱う。GetActiveOriginalMediaAsset に続けてもう 1 クエリ増えるが、
// 既に 2 クエリ投げている経路なので追加コストはほぼゼロ。
func EnqueueThumbnailIfNeeded(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx], recordingID int64) error {
	if riverClient == nil {
		return fmt.Errorf("river client is nil")
	}
	q := sqlcgen.New(pool)

	if _, err := q.GetActiveThumbnailMediaAssetID(ctx, recordingID); err == nil {
		return nil
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return fmt.Errorf("checking thumbnail: %w", err)
	}

	if _, err := q.GetActiveOriginalMediaAsset(ctx, recordingID); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking original: %w", err)
	}

	if rec, err := q.GetRecordingByID(ctx, recordingID); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("checking recording: %w", err)
	} else if rec.DeletedAt != nil {
		return nil
	}

	if _, err := riverClient.Insert(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID}, nil); err != nil {
		return fmt.Errorf("inserting thumbnail job: %w", err)
	}
	return nil
}

// EnqueueMissingThumbnails は original があり thumbnail が無い全 recording に
// ジョブを積む。復旧・テスト用。通常は original コミット後のヒント投入で足りる。
func EnqueueMissingThumbnails(ctx context.Context, pool *pgxpool.Pool, riverClient *river.Client[pgx5.Tx]) (int, error) {
	if riverClient == nil {
		return 0, fmt.Errorf("river client is nil")
	}
	ids, err := sqlcgen.New(pool).ListRecordingIDsMissingThumbnail(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing missing thumbnails: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	params := make([]river.InsertManyParams, len(ids))
	for i, id := range ids {
		params[i] = river.InsertManyParams{Args: jobs.ThumbnailJobArgs{RecordingID: id}}
	}
	results, err := riverClient.InsertMany(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("inserting thumbnail jobs: %w", err)
	}
	return len(results), nil
}
