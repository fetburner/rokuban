package worker

import (
	"bufio"
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
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/webhook"
)

// EncodeJobArgs は encode ジョブの引数。録画 1 件 × プロファイル 1 つ。
//
// UniqueOpts は recording_id + profile の pending 状態で一意化する
// （ingest と同じ pendingJobStates。完了済みは再投入可 — 真実は media_assets）。
type EncodeJobArgs struct {
	RecordingID int64  `json:"recording_id"`
	Profile     string `json:"profile"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeJobArgs) Kind() string { return "encode" }

// InsertOpts は encode キューと UniqueOpts を返す。
func (EncodeJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: encodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeWorker は原本 media_asset から構造化プロファイルで派生物を作る。
//
// ストレージ契約（docs/storage.md §3）:
//  1. 原本は mediapath.Resolve で読む
//  2. ffmpeg 出力は scratch に書く（media_dir へ直接書かない）
//  3. 進捗は ffmpeg -progress pipe:1（stderr スクレイピング禁止）
//  4. 成功時のみ scratch → media へストリームコピー + fsync → media_assets INSERT
//  5. 失敗時はコミット行を残さず、scratch は best-effort で掃除
type EncodeWorker struct {
	river.WorkerDefaults[EncodeJobArgs]
	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	FFmpeg     string
	// Profiles は名前解決用。config.EncodeConfig.Profile を使う。
	Profiles config.EncodeConfig

	// Webhook は録画ライフサイクル通知用クライアント（M3-11）。nil 可。
	Webhook *webhook.Client
}

// Timeout は River の総時間タイムアウトを無効化する。
//
// エンコード所要は録画長とコーデックで決まり、既定 1 分では足りない。
// 進捗は -progress pipe:1 で観測する（ストール検知は将来拡張。M3-3 では
// プロセス終了を待つ）。
func (w *EncodeWorker) Timeout(*river.Job[EncodeJobArgs]) time.Duration {
	return -1
}

// Work は encode ジョブを実行する。
//
// 失敗パスが複数箇所に散っているため（runEncode 参照）、webhook 発火は
// ここ 1 箇所に統一する。ctx キャンセル（River の停止・タイムアウト）は
// ジョブの失敗ではないので通知しない。
func (w *EncodeWorker) Work(ctx context.Context, job *river.Job[EncodeJobArgs]) error {
	err := w.runEncode(ctx, job)
	if err != nil && ctx.Err() == nil {
		w.notify(ctx, job.Args.RecordingID, job.Args.Profile, webhook.EventEncodeFailed, "failed")
	}
	return err
}

// runEncode は encode ジョブの本体。
func (w *EncodeWorker) runEncode(ctx context.Context, job *river.Job[EncodeJobArgs]) error {
	args := job.Args
	log := slog.With("recording_id", args.RecordingID, "profile", args.Profile)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.EncodeDuration.Observe(time.Since(started).Seconds())
		metrics.EncodeJobs.WithLabelValues(result).Inc()
	}()

	// 冪等: 既に active な encoded があれば何もしない。
	already, err := w.hasActiveEncoded(ctx, args.RecordingID, args.Profile)
	if err != nil {
		return fmt.Errorf("checking existing encoded asset: %w", err)
	}
	if already {
		log.Info("encode: encoded asset already committed, skipping")
		result = "success"
		return nil
	}

	profile, ok := w.Profiles.Profile(args.Profile)
	if !ok {
		// 設定から消えたプロファイルは再試行しても直らない。
		return fmt.Errorf("unknown encode profile %q", args.Profile)
	}

	orig, err := w.loadOriginal(ctx, args.RecordingID)
	if err != nil {
		return err
	}

	inputPath, err := mediapath.Resolve(w.MediaDir, orig.RelPath)
	if err != nil {
		return fmt.Errorf("resolving original path: %w", err)
	}
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("original file %s: %w", inputPath, err)
	}

	relPath, err := EncodedRelPath(orig.RelPath, profile.Name, profile.Container)
	if err != nil {
		return fmt.Errorf("building encoded rel_path: %w", err)
	}
	finalPath, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return fmt.Errorf("resolving encoded path: %w", err)
	}

	scratchDir, scratchOut, err := w.scratchPaths(args.RecordingID, profile)
	if err != nil {
		return err
	}
	defer func() {
		// 成功・失敗を問わず scratch を best-effort で掃除（途中成果物は cleanup が
		// 回収する想定だが、正常系では残さない）。
		if rmErr := os.RemoveAll(scratchDir); rmErr != nil {
			log.Warn("encode: scratch cleanup failed", "dir", scratchDir, "err", rmErr)
		}
	}()

	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return fmt.Errorf("creating scratch dir: %w", err)
	}

	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	cmdArgs := BuildFFmpegArgs(profile, inputPath, scratchOut)
	cmd := exec.CommandContext(ctx, ffmpeg, cmdArgs...)
	// 進捗は stdout（-progress pipe:1）。stderr はエラー診断のみ（進捗に使わない）。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	log.Info("encode: starting ffmpeg", "input", inputPath, "scratch", scratchOut)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}

	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		parseFFmpegProgress(stdout, log)
	}()

	waitErr := cmd.Wait()
	<-progressDone
	if waitErr != nil {
		// コンテキストキャンセルは River の停止やタイムアウト。
		if ctx.Err() != nil {
			return ctx.Err()
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr != "" {
			return fmt.Errorf("ffmpeg failed: %w (stderr: %s)", waitErr, stderr)
		}
		return fmt.Errorf("ffmpeg failed: %w", waitErr)
	}

	info, err := os.Stat(scratchOut)
	if err != nil {
		return fmt.Errorf("stat scratch output: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch output is empty: %s", scratchOut)
	}

	size, err := streamCopyFile(scratchOut, finalPath)
	if err != nil {
		return fmt.Errorf("copying to media dir: %w", err)
	}

	if err := w.commitEncoded(ctx, args.RecordingID, profile.Name, relPath, size); err != nil {
		// DB コミット失敗: media 上のファイルは DB に載らないので孤児。cleanup が回収。
		return fmt.Errorf("committing encoded asset: %w", err)
	}

	log.Info("encode: committed", "rel_path", relPath, "bytes", size)
	result = "success"
	w.notify(ctx, args.RecordingID, args.Profile, webhook.EventEncodeFinished, "finished")
	return nil
}

// notify は webhook を送る。失敗はログのみ（本処理を止めない。M3-11）。
func (w *EncodeWorker) notify(ctx context.Context, recordingID int64, profile, eventType, status string) {
	if w.Webhook == nil {
		return
	}
	q := sqlcgen.New(w.Pool)
	rec, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		slog.Warn("webhook: loading recording for encode notify",
			"recording_id", recordingID, "err", err)
		return
	}
	ev := webhook.Event{
		Type:        eventType,
		RecordingID: recordingID,
		Site:        rec.Site,
		Title:       rec.Title,
		Status:      status,
		Profile:     profile,
	}
	if err := w.Webhook.Notify(ctx, ev); err != nil {
		slog.Error("webhook notify failed",
			"type", eventType, "recording_id", recordingID, "err", err)
	}
}

func (w *EncodeWorker) hasActiveEncoded(ctx context.Context, recordingID int64, profile string) (bool, error) {
	q := sqlcgen.New(w.Pool)
	_, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func (w *EncodeWorker) loadOriginal(ctx context.Context, recordingID int64) (sqlcgen.GetActiveOriginalMediaAssetRow, error) {
	q := sqlcgen.New(w.Pool)
	row, err := q.GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return row, fmt.Errorf("no active original media_asset for recording %d", recordingID)
		}
		return row, fmt.Errorf("loading original media_asset: %w", err)
	}
	return row, nil
}

func (w *EncodeWorker) scratchPaths(recordingID int64, profile config.EncodeProfile) (dir, out string, err error) {
	base := w.ScratchDir
	if base == "" {
		base = os.TempDir()
	}
	safeProfile, err := sanitizeProfileForPath(profile.Name)
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(base, "encode", fmt.Sprintf("%d_%s", recordingID, safeProfile))
	out = filepath.Join(dir, "out."+profile.Container)
	return dir, out, nil
}

func (w *EncodeWorker) commitEncoded(ctx context.Context, recordingID int64, profile, relPath string, size int64) error {
	q := sqlcgen.New(w.Pool)
	profileName := profile
	_, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID,
		Profile:     &profileName,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		return fmt.Errorf("upserting media_asset: %w", err)
	}
	return nil
}

// BuildFFmpegArgs は構造化 EncodeProfile から ffmpeg 引数を組み立てる。
//
// 自由形式の cmd 文字列は受け取らない（issue #64 / #65）。input / output は
// 呼び出し側が絶対パスで渡す。進捗は -progress pipe:1（stdout）で出す。
func BuildFFmpegArgs(profile config.EncodeProfile, input, output string) []string {
	args := []string{
		"-hide_banner",
		"-nostats",
		"-y",
		"-i", input,
		"-c:v", profile.VideoCodec,
		"-c:a", profile.AudioCodec,
	}
	if profile.Height > 0 {
		// 幅はアスペクト比維持（偶数に丸める -2）。
		args = append(args, "-vf", fmt.Sprintf("scale=-2:%d", profile.Height))
	}
	if profile.CRF != nil {
		args = append(args, "-crf", strconv.Itoa(*profile.CRF))
	}
	if profile.Preset != "" {
		args = append(args, "-preset", profile.Preset)
	}
	args = append(args, "-f", profile.Container)
	if len(profile.ExtraArgs) > 0 {
		args = append(args, profile.ExtraArgs...)
	}
	// -progress pipe:1 は stdout に key=value。stderr はログのみ。
	args = append(args, "-progress", "pipe:1", "-loglevel", "error", output)
	return args
}

// EncodedRelPath は派生物の相対パスを決める。
//
// 規約: 原本と同じディレクトリに、拡張子を除いた basename + "_{profile}.{container}"。
// 例: "20240101/120000_title_1024.m2ts" + h264 + mp4
//
//	→ "20240101/120000_title_1024_h264.mp4"
//
// profile 名はパス成分としてサニタイズする（contentpath）。階層は原本の dir のみ。
func EncodedRelPath(originalRel, profileName, container string) (string, error) {
	if originalRel == "" {
		return "", fmt.Errorf("empty original rel_path")
	}
	if container != "mp4" && container != "mkv" {
		return "", fmt.Errorf("unsupported container %q", container)
	}
	safeProfile, err := sanitizeProfileForPath(profileName)
	if err != nil {
		return "", err
	}

	// パス区切りは DB 上で '/' 規約。filepath は OS 依存なので ToSlash で揃える。
	originalRel = filepath.ToSlash(originalRel)
	dir := pathDirSlash(originalRel)
	base := pathBaseSlash(originalRel)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		stem = "encoded"
	}
	name := stem + "_" + safeProfile + "." + container
	if dir == "" || dir == "." {
		return contentpath.SanitizeContentPath(name), nil
	}
	return contentpath.SanitizeContentPath(dir + "/" + name), nil
}

func sanitizeProfileForPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty profile name")
	}
	// SanitizeContentPath は '/' を階層に保つので、プロファイル名内の区切りは潰す。
	s := contentpath.SanitizeContentPath(strings.ReplaceAll(name, "/", "_"))
	s = strings.ReplaceAll(s, "/", "_")
	if s == "" || s == "." || s == "_" {
		return "", fmt.Errorf("profile name %q sanitizes to empty", name)
	}
	return s, nil
}

func pathDirSlash(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func pathBaseSlash(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// parseFFmpegProgress は -progress pipe:1 の key=value 行を読む。
//
// out_time_ms / out_time_us をログに出す。単位表記はキー名に埋め込まれているので
// バージョンで単位が変わってもキーが変われば追随できる（stderr の human 表示は見ない）。
func parseFFmpegProgress(r io.Reader, log *slog.Logger) {
	sc := bufio.NewScanner(r)
	// 進捗行は短い。万一長い行があっても落とさないよう余裕を持たせる。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lastOutTimeMs int64
	var lastLog time.Time
	for sc.Scan() {
		line := sc.Text()
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_ms":
			// ffmpeg の out_time_ms は実際にはマイクロ秒単位の歴史的な誤名、
			// という話もあるが、キー名をそのまま記録する（計算に使わない）。
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				lastOutTimeMs = n
			}
		case "out_time_us":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				lastOutTimeMs = n / 1000
			}
		case "progress":
			// continue / end。end または 5 秒間隔でログ。
			now := time.Now()
			if val == "end" || lastLog.IsZero() || now.Sub(lastLog) >= 5*time.Second {
				log.Info("encode: progress", "out_time_ms", lastOutTimeMs, "progress", val)
				lastLog = now
			}
		}
	}
}

// streamCopyFile は src を dst へシーケンシャルにコピーし、ファイルと親ディレクトリを
// fsync する（ストレージ契約: 作業は scratch、置くのは一回）。
func streamCopyFile(src, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}

	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("open src: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create dst: %w", err)
	}

	n, copyErr := io.Copy(out, in)
	if copyErr != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return n, fmt.Errorf("copy: %w", copyErr)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return n, fmt.Errorf("fsync file: %w", err)
	}
	if err := out.Close(); err != nil {
		return n, fmt.Errorf("close dst: %w", err)
	}

	// ディレクトリエントリの永続化（best-effort。一部 FS では no-op）。
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return n, nil
}

// JobInserter は encode ジョブ投入に使う最小面（*river.Client が満たす）。
type JobInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// EnqueueMissingEncodes は desired（recordings.encode_profiles）− observed
// （active encoded media_assets）の差分を埋める encode ジョブを投入する。
//
// レベルトリガー: 呼び出し側は「いつでも」呼んでよい。既に asset があるプロファイル
// や pending なジョブはスキップされる（UniqueOpts）。ingest 成功後のヒント投入と、
// 将来の reconcile ループの両方から使う。
func EnqueueMissingEncodes(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64) error {
	if inserter == nil {
		return fmt.Errorf("encode enqueue: inserter is nil")
	}
	q := sqlcgen.New(pool)

	// 原本が無ければエンコード対象外（ingest 前・原本削除後）。
	if _, err := q.GetActiveOriginalMediaAsset(ctx, recordingID); err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("loading original for encode enqueue: %w", err)
	}

	rec, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading recording %d: %w", recordingID, err)
	}
	if len(rec.EncodeProfiles) == 0 {
		return nil
	}

	for _, name := range rec.EncodeProfiles {
		if name == "" {
			continue
		}
		_, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{
			RecordingID: recordingID,
			Profile:     &name,
		})
		if err == nil {
			continue // 既に active encoded あり
		}
		if !errors.Is(err, pgx5.ErrNoRows) {
			return fmt.Errorf("checking encoded asset %q: %w", name, err)
		}

		if _, err := inserter.Insert(ctx, EncodeJobArgs{
			RecordingID: recordingID,
			Profile:     name,
		}, nil); err != nil {
			return fmt.Errorf("inserting encode job profile=%s: %w", name, err)
		}
	}
	return nil
}

// enqueueMissingEncodesFromContext は River ワーカーの ctx から client を取り、
// 欠けている encode ジョブを投入する。client が無い（単体で Work を呼んだ）場合は
// 何もしない。失敗はログのみ（ingest 本体の成功を巻き戻さない）。
func enqueueMissingEncodesFromContext(ctx context.Context, pool *pgxpool.Pool, recordingID int64) {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return
	}
	if err := EnqueueMissingEncodes(ctx, client, pool, recordingID); err != nil {
		slog.Error("encode: failed to enqueue missing encodes",
			"recording_id", recordingID, "err", err)
	}
}
