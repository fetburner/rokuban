package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// サムネイルの代表フレーム位置ポリシー（固定。設定キーは設けない）:
//
//	seek = min(duration × 10%, 30s)
//
// オープニング直後のロゴ寄りを避けつつ、長尺でも 30 秒で頭打ちにする。
// duration が取れない / 0 のときは 0 秒（先頭フレーム）に落とす。
// docs/storage.md「サムネイル」。
const (
	thumbnailSeekFraction = 0.10
	thumbnailSeekMax      = 30 * time.Second
	thumbnailTimeout      = 5 * time.Minute
)

// ThumbnailJobArgs は thumbnail ジョブの引数。録画 1 本につき 1 アセット
// （kind = 'thumbnail'。UNIQUE (recording_id, kind, profile)）。
type ThumbnailJobArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// InsertOpts は thumbnail キューへ投入し、recording_id で一意化する。
//
// ByState は pendingJobStates に絞る（既定は completed を含むため、一度成功した
// recording を再投入できなくなる。コミット済みかどうかは media_assets 行が真実）。
func (ThumbnailJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: thumbnailQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// ThumbnailWorker は原本から代表フレームを JPEG 抽出し、media_assets
// （kind = 'thumbnail'）としてコミットする。
//
// ストレージ契約: scratch に ffmpeg 出力 → メディアへストリームコピー + fsync →
// DB 行 INSERT（公開の定義は rename ではなく DB。docs/storage.md §3）。
type ThumbnailWorker struct {
	river.WorkerDefaults[ThumbnailJobArgs]

	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	FFmpeg     string
	FFprobe    string

	// runCmd はテストで差し替える実行フック。nil なら exec.CommandContext。
	// stdout を返す（ffprobe）。ffmpeg はファイル副作用だけを使う。
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Timeout はサムネイル 1 件の上限。大容量 TS への入力シークでも 5 分あれば足りる。
func (w *ThumbnailWorker) Timeout(*river.Job[ThumbnailJobArgs]) time.Duration {
	return thumbnailTimeout
}

// Work は thumbnail ジョブを実行する。
//
// レベルトリガー: original が無くても / active thumbnail が既にあっても成功扱い
// で終える（desired − observed が空なら何もしない）。
func (w *ThumbnailWorker) Work(ctx context.Context, job *river.Job[ThumbnailJobArgs]) error {
	recordingID := job.Args.RecordingID
	log := slog.With("recording_id", recordingID, "job", "thumbnail")

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.ThumbnailDuration.Observe(time.Since(started).Seconds())
		metrics.ThumbnailJobs.WithLabelValues(result).Inc()
	}()

	q := sqlcgen.New(w.Pool)

	// 既に active thumbnail があるなら再生成しない（冪等）。
	if _, err := q.GetActiveThumbnailMediaAssetID(ctx, recordingID); err == nil {
		log.Info("thumbnail: already committed, skipping")
		result = "success"
		return nil
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return fmt.Errorf("checking existing thumbnail: %w", err)
	}

	orig, err := q.GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			// original がまだ無い（ingest 前）なら desired が空。再試行しても
			// 埋まらないので成功扱いで捨てる。original コミット後のレベルトリガー
			// 投入が改めて積む。
			log.Info("thumbnail: no active original, skipping")
			result = "success"
			return nil
		}
		return fmt.Errorf("loading original media asset: %w", err)
	}

	inputPath, err := mediapath.Resolve(w.MediaDir, orig.RelPath)
	if err != nil {
		return fmt.Errorf("resolving original path: %w", err)
	}

	duration, err := w.probeDuration(ctx, inputPath)
	if err != nil {
		// 長さが取れなくても先頭フレームで続行する（壊れたメタデータへの保険）。
		log.Warn("thumbnail: ffprobe duration failed, seeking to 0", "err", err)
		duration = 0
	}
	seek := thumbnailSeek(duration)

	scratchPath, err := w.scratchOutputPath(recordingID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(scratchPath), 0o755); err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}
	// 前回残骸があっても ffmpeg -y で上書きする。完了後に消す。
	defer func() { _ = os.Remove(scratchPath) }()

	if err := w.extractFrame(ctx, inputPath, scratchPath, seek); err != nil {
		return fmt.Errorf("extracting frame: %w", err)
	}

	info, err := os.Stat(scratchPath)
	if err != nil {
		return fmt.Errorf("stat scratch thumbnail: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch thumbnail is empty")
	}

	relPath := thumbnailRelPath(recordingID)
	destPath, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return fmt.Errorf("resolving thumbnail dest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating media dir for thumbnail: %w", err)
	}

	size, err := copyFileFsync(scratchPath, destPath)
	if err != nil {
		return fmt.Errorf("copying thumbnail to media: %w", err)
	}

	if err := w.commit(ctx, recordingID, relPath, size); err != nil {
		// DB コミット失敗時はメディア側のファイルを残す（cleanup が孤児回収）。
		// 消すと「再実行でファイルも DB も無い」窓が広がる。
		return fmt.Errorf("committing thumbnail: %w", err)
	}

	log.Info("thumbnail: committed", "rel_path", relPath, "size_bytes", size, "seek", seek)
	result = "success"
	return nil
}

func (w *ThumbnailWorker) commit(ctx context.Context, recordingID int64, relPath string, size int64) error {
	_, err := sqlcgen.New(w.Pool).InsertMediaAssetIfAbsent(ctx, sqlcgen.InsertMediaAssetIfAbsentParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			// 競合で既に行がある。ファイルは二重書きの残骸になりうるが、
			// UNIQUE (recording_id, kind, profile) の勝者の行が真実。
			return nil
		}
		return fmt.Errorf("inserting media_asset: %w", err)
	}
	return nil
}

func (w *ThumbnailWorker) scratchOutputPath(recordingID int64) (string, error) {
	if w.ScratchDir == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	// ジョブ単位で一意な名前。並列ジョブが同じ scratch を踏まないようにする。
	return filepath.Join(w.ScratchDir, "thumbnail", fmt.Sprintf("%d.jpg", recordingID)), nil
}

// thumbnailRelPath はメディアストレージ上の相対パスを返す。
// recording_id をファイル名に使い、原本の contentPath に依存しない
// （原本削除後もパスが安定する。docs/storage.md の until_encoded）。
func thumbnailRelPath(recordingID int64) string {
	return fmt.Sprintf("thumbnails/%d.jpg", recordingID)
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

func (w *ThumbnailWorker) probeDuration(ctx context.Context, inputPath string) (time.Duration, error) {
	ffprobe := w.FFprobe
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	}
	out, err := w.commandOutput(ctx, ffprobe, args...)
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
	args := []string{
		"-y",
		"-ss", formatSeekSeconds(seek),
		"-i", inputPath,
		"-frames:v", "1",
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
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w\n%s", name, args, err, truncateOutput(out))
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

// copyFileFsync は src を dst へシーケンシャルコピーし、dst を fsync する。
// ストレージ契約ルール 1・2（書き込みは一発、置くのは一回）。
func copyFileFsync(src, dst string) (int64, error) {
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

	if _, err := riverClient.Insert(ctx, ThumbnailJobArgs{RecordingID: recordingID}, nil); err != nil {
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
	n := 0
	for _, id := range ids {
		if _, err := riverClient.Insert(ctx, ThumbnailJobArgs{RecordingID: id}, nil); err != nil {
			return n, fmt.Errorf("inserting thumbnail job for recording %d: %w", id, err)
		}
		n++
	}
	return n, nil
}
