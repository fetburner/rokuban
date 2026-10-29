package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
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
	// Media はサムネイル入力の読み取り契約。nil は該当経路を呼ばない部分テストでのみ許容する。
	Media            store.MediaReconcile
	Publisher        store.MediaPublish
	Objects          objstore.Store
	ObjectPublisher  store.DerivedMediaPublisher
	MediaDir         string
	MediaStoreName   string
	DerivedStoreName string
	ScratchDir       string
	FFmpeg           string
	FFprobe          string

	// runCmd はテストで差し替える実行フック。nil なら exec.CommandContext。
	// stdout を返す（ffprobe）。ffmpeg はファイル副作用だけを使う。
	runCmd func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type thumbnailWorkPlan struct {
	inputID      int64
	inputRelPath string
	inputStore   string
	inputSeek    time.Duration
	seekMs       int64
	relPath      string
	generation   int64
	expected     *thumbnailObserved
}

// Work は thumbnail ジョブを実行する。
//
// レベルトリガー: 初回生成では active original が無ければ成功扱いで終える。
// 既存 thumbnail は、チャプター上で抽出位置が CM 区間に入ったときだけ選び直す。
func (w *ThumbnailWorker) Work(ctx context.Context, args jobs.ThumbnailJobArgs) error {
	recordingID := args.RecordingID
	log := slog.With("recording_id", recordingID, "job", "thumbnail")

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.ThumbnailDuration.Observe(time.Since(started).Seconds())
		metrics.ThumbnailJobs.WithLabelValues(result).Inc()
	}()
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "starting thumbnail generation"); err != nil {
		return err
	}

	plan, ready, err := w.buildThumbnailPlan(ctx, recordingID, log)
	if err != nil {
		return err
	}
	if !ready {
		result = "success"
		return nil
	}

	scratchDir, err := newWorkerScratchDir(w.ScratchDir, "thumbnail", fmt.Sprintf("%d-", recordingID))
	if err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratchDir) }()
	scratchPath := filepath.Join(scratchDir, "thumbnail.jpg")
	inputPath, err := w.resolveThumbnailInput(ctx, plan.inputRelPath, plan.inputStore, scratchDir)
	if err != nil {
		return fmt.Errorf("resolving thumbnail input: %w", err)
	}

	if err := w.extractFrame(ctx, inputPath, scratchPath, plan.inputSeek); err != nil {
		return fmt.Errorf("extracting frame: %w", err)
	}

	info, err := os.Stat(scratchPath)
	if err != nil {
		return fmt.Errorf("stat scratch thumbnail: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch thumbnail is empty")
	}

	expected := (*store.ThumbnailObservation)(nil)
	if plan.expected != nil {
		expected = &store.ThumbnailObservation{
			ID: plan.expected.ID, RelPath: plan.expected.RelPath,
			SeekMs: plan.expected.SeekMs, DerivedGeneration: plan.expected.DerivedGeneration,
		}
	}
	var size int64
	var published bool
	if w.Objects != nil {
		pending := store.PendingMediaAssetCopy{
			RecordingID: recordingID, Kind: "thumbnail", RelPath: plan.relPath,
			DerivedGeneration: plan.generation,
		}
		size, published, err = publishGeneratedObject(ctx, w.Objects, w.ObjectPublisher, pending, scratchPath, objectContentType(plan.relPath), false,
			func(size int64, sum [sha256.Size]byte) (bool, error) {
				return w.ObjectPublisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: plan.relPath, SizeBytes: size,
					SeekMs: plan.seekMs, InputAssetID: plan.inputID, InputStore: plan.inputStore,
					DerivedGeneration: plan.generation, Expected: expected,
					SHA256: sum[:], PendingCopy: true,
				}, func() error { return nil })
			})
	} else {
		size, published, err = publishGeneratedMediaAsset(ctx, w.MediaDir, w.MediaStoreName, plan.relPath, scratchPath,
			func(size int64, publish func() error) (bool, error) {
				return w.Publisher.PublishThumbnail(ctx, store.ThumbnailPublishInput{
					RecordingID: recordingID, RelPath: plan.relPath, SizeBytes: size,
					SeekMs: plan.seekMs, InputAssetID: plan.inputID,
					DerivedGeneration: plan.generation, Expected: expected,
				}, publish)
			})
	}
	if err != nil {
		return fmt.Errorf("publishing thumbnail: %w", err)
	}
	if !published {
		log.Info("thumbnail: already committed or original no longer active, skipping")
		result = "success"
		return nil
	}
	log.Info("thumbnail: committed", "rel_path", plan.relPath, "size_bytes", size,
		"input_seek_ms", plan.inputSeek.Milliseconds(), "seek_ms", plan.seekMs)
	result = "success"
	return nil
}

func (w *ThumbnailWorker) buildThumbnailPlan(
	ctx context.Context,
	recordingID int64,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	activeThumbnail, err := w.Media.HasActiveThumbnailMediaAsset(ctx, recordingID)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("checking thumbnail: %w", err)
	}
	if !activeThumbnail {
		// ごみ箱は公開時に MediaPublish.PublishThumbnail が録画行をロックして弾く。
		return w.initialThumbnailPlan(ctx, recordingID, log)
	}
	// 候補クエリは reconcile と同じ 1 本。ごみ箱・チャプター無し・purged は除外済み。
	rows, err := w.Media.ListThumbnailReselectCandidates(ctx, store.MediaReconcilePage{
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
	return w.reselectionThumbnailPlan(ctx, rows[0], log)
}

func (w *ThumbnailWorker) initialThumbnailPlan(
	ctx context.Context,
	recordingID int64,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	// 初回の仮サムネイルは従来どおり原本だけを使い、CM 検出を待たずに作る。
	orig, err := w.Media.GetActiveOriginalMediaAsset(ctx, recordingID)
	if errors.Is(err, store.ErrNotFound) {
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
	generation, found, err := w.Media.GetDerivedMediaAssetGeneration(ctx, recordingID, "thumbnail")
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("loading thumbnail generation: %w", err)
	}
	if found {
		generation++
	}
	relPath, generation, err := w.nextUnusedThumbnailRelPath(ctx, recordingID, generation)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("choosing thumbnail path: %w", err)
	}
	return thumbnailWorkPlan{
		inputID: orig.ID, inputRelPath: orig.RelPath, inputStore: orig.Store, inputSeek: inputSeek,
		seekMs: seekMs, relPath: relPath, generation: generation,
	}, true, nil
}

func (w *ThumbnailWorker) reselectionThumbnailPlan(
	ctx context.Context,
	state store.ThumbnailReselectCandidate,
	log *slog.Logger,
) (thumbnailWorkPlan, bool, error) {
	timeline, hasTimeline := thumbnailTimeline(state)
	if !hasTimeline {
		log.Info("thumbnail: no chapter timeline, keeping current thumbnail")
		return thumbnailWorkPlan{}, false, nil
	}
	inputs := thumbnailInputsOf(state)
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
	if state.ThumbnailRelPath == nil || state.ThumbnailMediaAssetID == nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("active thumbnail planning state is incomplete")
	}
	expected := &thumbnailObserved{
		ID:                *state.ThumbnailMediaAssetID,
		RelPath:           *state.ThumbnailRelPath,
		SeekMs:            state.SeekMs,
		DerivedGeneration: state.ThumbnailGeneration,
	}
	generation := state.ThumbnailGeneration + 1
	relPath, generation, err := w.nextUnusedThumbnailRelPath(ctx, state.RecordingID, generation)
	if err != nil {
		return thumbnailWorkPlan{}, false, fmt.Errorf("choosing thumbnail replacement path: %w", err)
	}
	return thumbnailWorkPlan{
		inputID: plan.Source.MediaAssetID, inputRelPath: plan.Source.RelPath, inputStore: plan.Source.Store,
		inputSeek: time.Duration(plan.InputSeekMs) * time.Millisecond,
		seekMs:    plan.RecordedSeek, relPath: relPath, generation: generation,
		expected: expected,
	}, true, nil
}

// resolveThumbnailInput はサムネイルの入力を ffmpeg が読める scratch 上のパスにする。
// FS の置き場の入力は media_dir 上のパスをそのまま返し、S3 の置き場の encoded は scratch へ取得する。
func (w *ThumbnailWorker) resolveThumbnailInput(ctx context.Context, relPath, storeName, scratchDir string) (string, error) {
	if w.Objects == nil || storeName == w.MediaStoreName {
		return mediapath.Resolve(w.MediaDir, relPath)
	}
	if w.DerivedStoreName == "" || storeName != w.DerivedStoreName {
		return "", fmt.Errorf("thumbnail input store %q is not readable by this worker", storeName)
	}
	reader, ok := w.Objects.(objstore.Reader)
	if !ok {
		return "", fmt.Errorf("object store %q does not support streaming reads", storeName)
	}
	body, err := reader.Get(ctx, relPath)
	if err != nil {
		metrics.RecordObjectStoreError("get", err)
		return "", fmt.Errorf("getting thumbnail input object %q: %w", relPath, err)
	}
	defer func() { _ = body.Close() }()
	inputPath := filepath.Join(scratchDir, "input"+filepath.Ext(relPath))
	file, err := os.OpenFile(inputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating scratch thumbnail input: %w", err)
	}
	n, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	if copyErr != nil {
		metrics.RecordObjectStoreError("get", copyErr)
		return "", fmt.Errorf("downloading thumbnail input object %q: %w", relPath, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("closing scratch thumbnail input: %w", closeErr)
	}
	if n == 0 {
		return "", fmt.Errorf("thumbnail input object %q is empty", relPath)
	}
	return inputPath, nil
}

// thumbnailRelPath returns the path for a persisted generation. Generation zero
// retains the original filename for compatibility with existing files.
func thumbnailRelPath(recordingID, generation int64) string {
	if generation <= 0 {
		return fmt.Sprintf("thumbnails/%d.jpg", recordingID)
	}
	return fmt.Sprintf("thumbnails/%d.g%d.jpg", recordingID, generation)
}

func (w *ThumbnailWorker) nextUnusedThumbnailRelPath(ctx context.Context, recordingID, generation int64) (string, int64, error) {
	for {
		candidate := thumbnailRelPath(recordingID, generation)
		if w.Objects != nil {
			if _, err := mediapath.Resolve(w.MediaDir, candidate); err != nil {
				return "", 0, err
			}
			_, err := w.Objects.Head(ctx, candidate)
			if errors.Is(err, objstore.ErrNotFound) {
				return candidate, generation, nil
			}
			if err != nil {
				metrics.RecordObjectStoreError("head", err)
				return "", 0, err
			}
		} else {
			path, err := mediapath.Resolve(w.MediaDir, candidate)
			if err != nil {
				return "", 0, err
			}
			_, err = os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				return candidate, generation, nil
			}
			if err != nil {
				return "", 0, err
			}
		}
		if generation == int64(^uint64(0)>>1) {
			return "", 0, fmt.Errorf("thumbnail generation is exhausted")
		}
		generation++
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
	ID                int64
	RelPath           string
	SeekMs            *int64
	DerivedGeneration int64
}

func (w *ThumbnailWorker) probeDuration(ctx context.Context, inputPath string) (time.Duration, error) {
	return probeDuration(ctx, w.FFprobe, inputPath, w.commandOutput)
}

func (w *ThumbnailWorker) extractFrame(ctx context.Context, inputPath, outputPath string, seek time.Duration) error {
	ffmpeg := ffargs.FFmpegPath(w.FFmpeg)
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
		"-vf", ffargs.SquarePixelsFilter,
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

// copyScratchFileFsync は scratch 上の src を scratch 上の dst へコピーし、dst を fsync する。
// 公開アセットは stageMediaFile で同じディレクトリへ置き、MediaPublish の callback で rename する。
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
