package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
)

// シークプレビュー用タイルの形（固定。設定キーは設けない。poster と同じ流儀）:
//
//	間隔 10 秒 / 1 枚 160x90 / 10 列 / 上限 1080 枚（= 3 時間）
//
// 3 時間を超える部分はタイルが無く、クライアントはプレビューを出さない。
// 尺に応じて間隔を変える方式は採らない --- ffprobe の長さと <video> の長さの
// ずれが境界でタイル位置を狂わせる。
//
// **これらの値は web/src/lib/seek-tiles.ts にも同じ値がある。** 値の伝達経路が
// 無い（メディア配信は openapi.yaml の対象外）ので手で揃える。**値を変えたら
// 既存のタイルは位置がずれるため、再生成が必要になる**（タイルは rel_path も
// 行も同じまま中身だけが変わる）。
const (
	seekTilesInterval = 10 * time.Second
	seekTilesWidth    = 160
	seekTilesHeight   = 90
	seekTilesColumns  = 10
	seekTilesMaxTiles = 1080
	// seekTilesTimeout はタイル 1 件の上限。合成 TS で測った 45 ms/枚を
	// 1080 枚へ外挿すると約 49 秒で、合成とコピーを足しても十分な余裕がある。
	// 外挿も実録画・J4125 での生成時間も未検証（測定は 10 分・60 枚）。
	seekTilesTimeout = 15 * time.Minute
)

// SeekTilesWorker は原本からシークプレビュー用のタイル画像を生成し、
// media_assets（kind = 'seek_tiles'）としてコミットする。
//
// 生成方式は「タイルごとに入力シーク（-ss を -i の前）で 1 枚ずつ取り、
// 最後に tile フィルタで 1 枚に並べる」。読む量が枚数にだけ比例し、番組長に
// 比例しない（合成 TS 10 分・60 枚の実測で 2.7 秒。全デコード方式の 23.6 秒に対して）。
// そのため「先頭 N 分に限る」ような打ち切りは要らず、上限は枚数だけで決まる。
//
// **投入は `ThumbnailReconcileWorker` の定期パスだけである**（ingest 直後の
// ヒントは積まない）。poster は一覧に出るので即時性が要るが、タイルは利用者が
// 詳細を開いてホバーして初めて要る。そのため ingest の followup に
// `EnqueueThumbnailIfNeeded` と同型の関数をもう 1 本足す代わりに、定期パス
// （既定 15 分）に任せる。**録画直後の 15 分はプレビューが出ない**という代償を
// 受け入れる。待たせないことは要求の経路（配信）で担保している（404 → poster だけの見た目）。
//
// ストレージ契約: scratch に ffmpeg 出力 → メディアへストリームコピー + fsync →
// DB 行 INSERT（公開の定義は rename ではなく DB。docs/storage/contract.md §3）。
//
// 失敗したら scratch を捨ててやり直す。部分成果はコミットしない ---
// 行の存在 = タイルが全部そろっている（不変条件 10）。
type SeekTilesWorker struct {
	river.WorkerDefaults[jobs.SeekTilesJobArgs]

	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	FFmpeg     string
	FFprobe    string

	// runCmd はテストで差し替える実行フック。nil なら exec.CommandContext。
	// ThumbnailWorker と同じ契約（stdout を返すのは ffprobe 用）。
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Timeout はタイル 1 件の上限。
func (w *SeekTilesWorker) Timeout(*river.Job[jobs.SeekTilesJobArgs]) time.Duration {
	return seekTilesTimeout
}

// Work は seek_tiles ジョブを実行する。
//
// レベルトリガー: original が無くても / active な seek_tiles が既にあっても
// 成功扱いで終える（desired − observed が空なら何もしない）。
func (w *SeekTilesWorker) Work(ctx context.Context, job *river.Job[jobs.SeekTilesJobArgs]) error {
	recordingID := job.Args.RecordingID
	log := slog.With("recording_id", recordingID, "job", "seek_tiles")

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.SeekTilesDuration.Observe(time.Since(started).Seconds())
		metrics.SeekTilesJobs.WithLabelValues(result).Inc()
	}()

	q := sqlcgen.New(w.Pool)

	if _, err := q.GetActiveSeekTilesMediaAssetID(ctx, recordingID); err == nil {
		log.Info("seek_tiles: already committed, skipping")
		result = "success"
		return nil
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return fmt.Errorf("checking existing seek tiles: %w", err)
	}

	orig, err := q.GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			// original が無い（ingest 前・削除済み）なら desired が空。
			// ThumbnailWorker と同じ扱い（再試行しても埋まらない）。
			log.Info("seek_tiles: no active original, skipping")
			result = "success"
			return nil
		}
		return fmt.Errorf("loading original media asset: %w", err)
	}

	inputPath, err := mediapath.Resolve(w.MediaDir, orig.RelPath)
	if err != nil {
		return fmt.Errorf("resolving original path: %w", err)
	}

	// poster と違い、長さが取れないまま続行しない（0 秒も含む）。1 枚だけの格子を
	// コミットすると行の存在が「全部そろっている」を主張し、定期パスが二度と作り
	// 直さず、until_encoded の原本削除の条件まで満たしてしまう。River の再試行に任せる。
	duration, err := w.probeVideoDuration(ctx, inputPath)
	if err != nil {
		return fmt.Errorf("probing video duration: %w", err)
	}
	if duration <= 0 {
		return fmt.Errorf("video duration is %s", duration)
	}
	tiles := seekTileCount(duration)
	rows := (tiles + seekTilesColumns - 1) / seekTilesColumns

	// 前回の残骸ごと捨ててから作る。部分成果を残さない。
	framesDir, sheetPath, err := w.scratchPaths(recordingID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(framesDir); err != nil {
		return fmt.Errorf("clearing scratch frames dir: %w", err)
	}
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return fmt.Errorf("creating scratch frames dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(framesDir) }()
	defer func() { _ = os.Remove(sheetPath) }()

	for i := range tiles {
		framePath := filepath.Join(framesDir, fmt.Sprintf("%06d.jpg", i))
		at := seekTileAt(i, duration)
		err := w.extractTile(ctx, inputPath, framePath, at)
		if err != nil && i > 0 && i == tiles-1 {
			// 最後のタイルだけは、取れなければ直前のタイルで埋める。H.264 でキー
			// フレームの間隔が長いと、最後のキーフレームより後ろへの入力シークは
			// 終端の 2 秒手前でも 1 フレームも出せない（x264 GOP 5 秒で測定）。
			// 何秒戻れば足りるかは GOP で決まり、定数の余白では塞げない。直前の
			// タイルは同じ最後の区間の少し前なので、見た目の誤差は小さい。
			log.Warn("seek_tiles: last tile not extractable, reusing the previous tile",
				"tile", i, "at", formatSeekSeconds(at), "err", err)
			prev := filepath.Join(framesDir, fmt.Sprintf("%06d.jpg", i-1))
			if _, err = copyFileFsync(prev, framePath); err != nil {
				return fmt.Errorf("reusing tile %d for the last tile: %w", i-1, err)
			}
		}
		if err != nil {
			return fmt.Errorf("extracting tile %d at %s: %w", i, formatSeekSeconds(at), err)
		}
	}

	if err := w.composeSheet(ctx, framesDir, sheetPath, rows); err != nil {
		return fmt.Errorf("composing tile sheet: %w", err)
	}

	info, err := os.Stat(sheetPath)
	if err != nil {
		return fmt.Errorf("stat scratch tile sheet: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch tile sheet is empty")
	}

	relPath := seekTilesRelPath(recordingID)
	destPath, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return fmt.Errorf("resolving seek tiles dest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating media dir for seek tiles: %w", err)
	}

	size, err := copyFileFsync(sheetPath, destPath)
	if err != nil {
		return fmt.Errorf("copying seek tiles to media: %w", err)
	}

	if _, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: recordingID,
		RelPath:     relPath,
		SizeBytes:   size,
	}); err != nil {
		// DB コミット失敗時はメディア側のファイルを残す（cleanup が孤児回収）。
		// ThumbnailWorker と同じ判断。
		return fmt.Errorf("committing seek tiles: %w", err)
	}

	log.Info("seek_tiles: committed",
		"rel_path", relPath, "size_bytes", size, "tiles", tiles, "rows", rows, "duration", duration)
	result = "success"
	return nil
}

func (w *SeekTilesWorker) scratchPaths(recordingID int64) (framesDir, sheetPath string, err error) {
	if w.ScratchDir == "" {
		return "", "", fmt.Errorf("scratch dir is empty")
	}
	base := filepath.Join(w.ScratchDir, "seek_tiles")
	return filepath.Join(base, strconv.FormatInt(recordingID, 10)),
		filepath.Join(base, fmt.Sprintf("%d-tiles.jpg", recordingID)), nil
}

// seekTilesRelPath はメディアストレージ上の相対パスを返す。poster と同じく
// recording_id から決定的に導出し、原本の contentPath に依存しない。
// `thumbnails/` の中に置くので、rel_path の名前空間の検査は変わらない
// （docs/storage/contract.md §5）。
func seekTilesRelPath(recordingID int64) string {
	return fmt.Sprintf("thumbnails/%d_tiles.jpg", recordingID)
}

// seekTileCount は生成するタイル枚数を返す。Work は 0 秒以下を渡さない
// （長さが取れないときは失敗させる）。
func seekTileCount(duration time.Duration) int {
	if duration <= 0 {
		return 1
	}
	n := int((duration + seekTilesInterval - 1) / seekTilesInterval)
	if n < 1 {
		return 1
	}
	if n > seekTilesMaxTiles {
		return seekTilesMaxTiles
	}
	return n
}

// seekTileTailMargin は抜き出し位置を映像の終端から離す幅。映像ストリームの
// 長さちょうど（合成 TS 30 秒で 29.9966 秒）を指す入力シークは 1 フレームも
// 出せずに失敗し、1 秒手前なら成功した（ffmpeg 9.0.2 で測定）。枚数を切り上げで
// 決めるので、長さが 10 秒の倍数をわずかに超える録画では最後のタイルが必ずここに当たる。
// この余白で足りるのは MPEG-2 と GOP 1 秒程度の H.264 までで、GOP の長い H.264 は
// Work が最後のタイルを直前のタイルで埋めて扱う。
const seekTileTailMargin = time.Second

// seekTileAt は i 枚目のタイルを抜き出す位置を返す。原則 i×間隔だが、映像の
// 終端から seekTileTailMargin より後ろには置かない（下限は 0）。
func seekTileAt(i int, videoDuration time.Duration) time.Duration {
	at := time.Duration(i) * seekTilesInterval
	return max(0, min(at, videoDuration-seekTileTailMargin))
}

// probeVideoDuration は最初の映像ストリームの長さを返す。format の長さは最も長い
// ストリーム（音声など）で決まり映像の終端より後ろになるので、抜き出し位置の
// 上限には使えない。
func (w *SeekTilesWorker) probeVideoDuration(ctx context.Context, inputPath string) (time.Duration, error) {
	ffprobe := w.FFprobe
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	out, err := w.commandOutput(ctx, ffprobe,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	)
	if err != nil {
		return 0, err
	}
	s, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("ffprobe returned empty video duration")
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing video duration %q: %w", s, err)
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// extractTile は 1 枚のタイルを scratch へ書き出す。
//
// -ss を -i の前に置く入力シーク（大容量 TS で速い）。SAR を偶数幅の正方形
// ピクセルへ焼き込んでから 16:9 の枠へ収め、余白は pad で埋める（4:3 の映像は
// 左右が黒帯になる）。JPEG は SAR を運ばないので、これが無いと anamorphic な
// 地デジがブラウザで横に潰れて見える（poster と同じ理由）。
func (w *SeekTilesWorker) extractTile(ctx context.Context, inputPath, outputPath string, at time.Duration) error {
	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	args := []string{
		"-y",
		"-ss", formatSeekSeconds(at),
		"-i", inputPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf(
			"scale=round(iw*sar/2)*2:ih,setsar=1,"+
				"scale=%d:%d:force_original_aspect_ratio=decrease,"+
				"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1",
			seekTilesWidth, seekTilesHeight, seekTilesWidth, seekTilesHeight),
		"-q:v", "2",
		outputPath,
	}
	if _, err := w.commandOutput(ctx, ffmpeg, args...); err != nil {
		return err
	}
	// 版によっては ffmpeg が 1 フレームも出せずに終了コード 0 で終わるかもしれない
	// （未検証。ffmpeg 9.0.2 は非 0 で終わった）。ffmpeg は運用者が用意するので版は
	// 決まらない。連番に穴があると image2 はそこで読むのをやめ、以降のタイルが黒の
	// ままコミットされる。
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("tile not written: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("tile is empty")
	}
	return nil
}

// composeSheet はタイルを 1 枚の格子画像に並べる。枚数が列数の倍数でないとき、
// 余りは ffmpeg の tile フィルタが黒で埋める（クライアントは列数と 1 枚の
// 大きさだけを知っていれば位置を計算できる）。
func (w *SeekTilesWorker) composeSheet(ctx context.Context, framesDir, outputPath string, rows int) error {
	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	args := []string{
		"-y",
		"-start_number", "0",
		"-framerate", "1",
		"-i", filepath.Join(framesDir, "%06d.jpg"),
		"-vf", fmt.Sprintf("tile=%dx%d", seekTilesColumns, rows),
		"-frames:v", "1",
		"-q:v", "3",
		outputPath,
	}
	_, err := w.commandOutput(ctx, ffmpeg, args...)
	return err
}

func (w *SeekTilesWorker) commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if w.runCmd != nil {
		return w.runCmd(ctx, name, args...)
	}
	return commandOutput(ctx, name, args...)
}
