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
	"github.com/fetburner/rokuban/internal/ffargs"
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
//
// # site 照合ガード（issue #139）は不要と判断
//
// EncodeJobArgs は recording_id + profile のみで site を持たない。エンコードは
// 原本 media_asset（mediapath.Resolve で解決する単一の MediaDir 配下）を読んで
// FS に書くだけで mirakc には一切触れない（不変条件 4「ffmpeg/ffprobe の exec は
// worker / streamer パッケージのみ」であって mirakc 呼び出しではない）。
// アーカイブは複数サイト構成でも単一（site に従属しない。docs/storage.md）ため、
// 他サイトの worker が拾っても mediapath.Resolve が解決する先は変わらず、
// 「別インスタンスの id を投げる」形の壊れ方が起きない。
type EncodeWorker struct {
	river.WorkerDefaults[EncodeJobArgs]
	Pool       *pgxpool.Pool
	MediaDir   string
	ScratchDir string
	FFmpeg     string
	FFprobe    string
	// Profiles は名前解決用。config.EncodeConfig.Profile を使う。
	Profiles config.EncodeConfig

	// Webhook は録画ライフサイクル通知用クライアント（M3-11）。nil 可。
	Webhook *webhook.Client
}

// probeEncodeDuration は進捗の分母を timeout 以内に取得する。
// 進捗は best-effort の観測なので、ffprobe の停止でエンコード開始を塞がない。
func probeEncodeDuration(
	ctx context.Context,
	ffprobe, inputPath string,
	timeout time.Duration,
	run func(context.Context, string, ...string) ([]byte, error),
) (time.Duration, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return probeDuration(probeCtx, ffprobe, inputPath, run)
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
// 失敗パスが複数箇所に散っているため（runEncode 参照）、encode.failed の発火は
// ここ 1 箇所に集約する（encode.finished は成功地点が 1 つなので runEncode 内で
// 発火する。冪等スキップでは発火しない）。ctx キャンセル（River の停止・
// タイムアウト）はジョブの失敗ではないので通知しない。
//
// このジョブは River が再試行するので、恒久的に失敗するエンコードでは
// encode.failed が試行ごとに配送される。受け側が最終試行を見分けられるよう
// attempt / maxAttempts をペイロードに載せる（M3-11）。
func (w *EncodeWorker) Work(ctx context.Context, job *river.Job[EncodeJobArgs]) error {
	err := w.runEncode(ctx, job)
	if shouldNotifyEncodeFailure(err, ctx.Err()) {
		ev := webhook.Event{
			Type:        webhook.EventEncodeFailed,
			RecordingID: job.Args.RecordingID,
			Status:      "failed",
			Profile:     job.Args.Profile,
			Attempt:     job.Attempt,
			MaxAttempts: job.MaxAttempts,
		}
		w.notify(ctx, ev)
	}
	return err
}

// runEncode は encode ジョブの本体。
//
// err は名前付き戻り値 --- 直後の defer（試行状態の観測、issue #316）が
// 全ての return 文の結果を横取りして recording_encode_attempts に反映するため。
func (w *EncodeWorker) runEncode(ctx context.Context, job *river.Job[EncodeJobArgs]) (err error) {
	args := job.Args
	log := slog.With("recording_id", args.RecordingID, "profile", args.Profile)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.EncodeDuration.Observe(time.Since(started).Seconds())
		metrics.EncodeJobs.WithLabelValues(result).Inc()
	}()

	// 冪等: 既に active な encoded があれば何もしない。リークした古い試行行
	// （不変条件 10: 完了しているのに failed/running を名乗る行を残さない）が
	// あれば掃除する。
	already, err := w.hasActiveEncoded(ctx, args.RecordingID, args.Profile)
	if err != nil {
		return fmt.Errorf("checking existing encoded asset: %w", err)
	}
	if already {
		log.Info("encode: encoded asset already committed, skipping")
		result = "success"
		w.clearEncodeAttempt(ctx, args.RecordingID, args.Profile)
		return nil
	}

	// この試行の観測（issue #316）。running を書き、この呼び出しが返るときに
	// 成功（media_asset 行を作った）か失敗かで消す/failed に上書きする。
	// shouldNotifyEncodeFailure と**同じ判定関数**を使う（bespoke な条件を
	// もう 1 つ増やさない）--- ctx キャンセル（River の停止・タイムアウト）は
	// ジョブの失敗扱いにしないので running のまま残し、次の実行が上書きする。
	// markEncodeAttemptFailed の書き込みは job の ctx から切り離してある
	// （attemptWriteContext）ので、running が残るのはこのガードのおかげで、
	// 「DB 書き込み自体が ctx キャンセルで失敗する」という偶然ではない。
	w.markEncodeAttemptRunning(ctx, args.RecordingID, args.Profile)
	defer func() {
		if err == nil {
			w.clearEncodeAttempt(ctx, args.RecordingID, args.Profile)
			return
		}
		if !shouldNotifyEncodeFailure(err, ctx.Err()) {
			return
		}
		w.markEncodeAttemptFailed(ctx, args.RecordingID, args.Profile, err)
	}()

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

	duration, probeErr := probeEncodeDuration(
		ctx, w.FFprobe, inputPath, encodeDurationProbeTimeout, commandOutput,
	)
	if probeErr != nil {
		log.Warn("encode: probing input duration failed; progress percentage disabled", "err", probeErr)
	}
	var reportProgress func(time.Duration)
	if duration > 0 {
		reporter := encodeProgressReporter{
			recordingID: args.RecordingID,
			profile:     args.Profile,
			duration:    duration,
			interval:    encodeProgressInterval,
			notify: func(ctx context.Context, payload string) error {
				return sqlcgen.New(w.Pool).NotifyTopic(ctx, payload)
			},
			log: log,
		}
		var stopProgress context.CancelFunc
		reportProgress, stopProgress = reporter.start(ctx)
		defer stopProgress()
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
	// ffmpeg は字幕ストリームが無い状態で WebVTT 出力を要求すると終了する。
	// 先に ffprobe で存在を確認し、字幕がある録画だけサイドカー出力を有効に
	// することで、局や番組によって字幕 PID が無い録画でもエンコード本体を
	// 落とさない（issue #430 の optional map の罠）。
	subtitleOut := ""
	withSubtitles := false
	if profile.Subtitles == "webvtt" {
		subtitleOut = filepath.Join(scratchDir, "out.vtt")
		withSubtitles, err = probeHasSubtitlesWithTimeout(ctx, w.FFprobe, inputPath, commandOutput)
		if err != nil {
			log.Warn("encode: probing subtitle streams failed; continuing without subtitle sidecar", "err", err)
		}
	}
	cmdArgs := BuildFFmpegArgsForSubtitle(profile, inputPath, scratchOut, subtitleOut, withSubtitles)
	cmd := exec.CommandContext(ctx, ffmpeg, cmdArgs...)
	setWorkerExecWaitDelay(cmd)
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
		parseFFmpegProgress(stdout, log, reportProgress)
	}()

	waitErr := cmd.Wait()
	<-progressDone
	if waitErr != nil {
		// コンテキストキャンセルは River の停止やタイムアウト。
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// ffmpeg 自体は exit 0 で完走したが、孫プロセスが stdout/stderr の
			// fd を握ったままで WaitDelay が先に切れた（この PR が扱う
			// ハングの exit 0 版）。以降の os.Stat / サイズ検査が出力を守るので
			// 失敗にはしないが、再試行ループから見分けられるよう記録は残す。
			log.Warn("encode: ffmpeg exited successfully but WaitDelay expired before I/O completed", "wait_delay", workerExecWaitDelay)
		} else {
			stderr := strings.TrimSpace(stderrBuf.String())
			if stderr != "" {
				return fmt.Errorf("ffmpeg failed: %w (stderr: %s)", waitErr, stderr)
			}
			return fmt.Errorf("ffmpeg failed: %w", waitErr)
		}
	}

	info, err := os.Stat(scratchOut)
	if err != nil {
		return fmt.Errorf("stat scratch output: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("scratch output is empty: %s", scratchOut)
	}
	if withSubtitles {
		vttInfo, statErr := os.Stat(subtitleOut)
		if statErr != nil {
			return fmt.Errorf("stat subtitle sidecar: %w", statErr)
		}
		if vttInfo.Size() == 0 {
			return fmt.Errorf("subtitle sidecar is empty: %s", subtitleOut)
		}
	}

	size, err := streamCopyFile(scratchOut, finalPath)
	if err != nil {
		return fmt.Errorf("copying to media dir: %w", err)
	}
	if withSubtitles {
		subtitleRelPath, pathErr := SubtitleRelPath(relPath)
		if pathErr != nil {
			return fmt.Errorf("building subtitle rel_path: %w", pathErr)
		}
		subtitleFinalPath, pathErr := mediapath.Resolve(w.MediaDir, subtitleRelPath)
		if pathErr != nil {
			return fmt.Errorf("resolving subtitle path: %w", pathErr)
		}
		if _, copyErr := streamCopyFile(subtitleOut, subtitleFinalPath); copyErr != nil {
			return fmt.Errorf("copying subtitle sidecar: %w", copyErr)
		}
	}

	if err := w.commitEncoded(ctx, args.RecordingID, profile.Name, relPath, size); err != nil {
		// DB コミット失敗: media 上のファイルは DB に載らないので孤児。cleanup が回収。
		return fmt.Errorf("committing encoded asset: %w", err)
	}

	log.Info("encode: committed", "rel_path", relPath, "bytes", size)
	result = "success"
	w.notify(ctx, webhook.Event{
		Type:        webhook.EventEncodeFinished,
		RecordingID: args.RecordingID,
		Status:      "finished",
		Profile:     args.Profile,
	})
	return nil
}

// shouldNotifyEncodeFailure は encode.failed を発火すべきかを返す。ctxErr は
// runEncode が返った時点の ctx.Err()。
//
// ctx キャンセル（River の停止・ジョブタイムアウト）はジョブの失敗ではないので
// 発火しない。この経路で落とした通知は失われない — ジョブは available に戻り、
// 次に実行されたときに成功か失敗のどちらかを発火する。
func shouldNotifyEncodeFailure(err, ctxErr error) bool {
	return err != nil && ctxErr == nil
}

// notify は ev に録画のスナップショット（site / title）を足して webhook を送る。
// 失敗はログのみ（本処理を止めない。M3-11）。
func (w *EncodeWorker) notify(ctx context.Context, ev webhook.Event) {
	if w.Webhook == nil {
		return
	}
	q := sqlcgen.New(w.Pool)
	rec, err := q.GetRecordingByID(ctx, ev.RecordingID)
	if err != nil {
		slog.Warn("webhook: loading recording for encode notify",
			"recording_id", ev.RecordingID, "err", err)
		return
	}
	ev.Site = rec.Site
	ev.Title = rec.Title
	if err := w.Webhook.Notify(ctx, ev); err != nil {
		slog.Error("webhook notify failed",
			"type", ev.Type, "recording_id", ev.RecordingID, "err", err)
	}
}

// encodeAttemptErrorMaxLen は recording_encode_attempts.error に書くバイト数の
// 上限。ffmpeg の stderr を丸ごと含むエラーが際限なく育つのを防ぐ。
//
// 読み手は API ではなく運用者の SELECT（docs/runbook/troubleshooting.md
// 「エンコードが失敗している」）。全文は worker のログに出ているので、
// ここで切り詰めても失われる情報は無い。
const encodeAttemptErrorMaxLen = 2000

// encodeAttemptWriteTimeout は recording_encode_attempts への書き込み
// （試行状態の観測）に使うタイムアウト。job の ctx から切り離す
// （attemptWriteContext）理由を参照。
const encodeAttemptWriteTimeout = 5 * time.Second

// subtitleProbeTimeout は字幕サイドカーの有無を調べる ffprobe の上限。
// 進捗分母の probe と同じく best-effort だが、字幕機能を有効にしたことで
// エンコード全体が無期限に止まることは許さない。
const subtitleProbeTimeout = 30 * time.Second

// attemptWriteContext は recording_encode_attempts への書き込み用に、job の
// ctx から切り離した（ただし無期限には待たない）ctx を返す。
// 使うのは markEncodeAttemptFailed と clearEncodeAttempt の 2 箇所
// （それぞれの doc コメントに切り離す理由がある）。
//
// markEncodeAttemptFailed の呼び出しは shouldNotifyEncodeFailure と同じ
// ガード（ctx キャンセル時は呼ばない）の後段にあるが、書き込み自体が job の
// ctx に紐付いていると「ガードが無くても、キャンセル済み ctx での DB 書き込みが
// 失敗するので running が残る」という偶然の結果とガードが区別できなくなる
// （レビュー issue #316 で判明）。切り離すことでガードを実際に load-bearing に
// する --- ガードを外すと、キャンセル後でも書き込みが成功して failed に
// 上書きされ、テストが検出できる。
func attemptWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), encodeAttemptWriteTimeout)
}

// truncateEncodeAttemptError は msg を encodeAttemptErrorMaxLen バイト以内に
// 切り詰める。バイト境界で切ると末尾がマルチバイト文字の途中で切れて不正な
// UTF-8 になり得る（Postgres が INSERT/UPDATE を拒否し、試行行が書けなくなる
// --- 失敗が「エンコード中」のまま見え続ける）。strings.ToValidUTF8 で末尾の
// 不完全なシーケンスを取り除く。
func truncateEncodeAttemptError(msg string) string {
	if len(msg) <= encodeAttemptErrorMaxLen {
		return msg
	}
	return strings.ToValidUTF8(msg[:encodeAttemptErrorMaxLen], "")
}

// markEncodeAttemptRunning は recording_encode_attempts に running を書く
// （issue #316）。失敗はログのみ --- この表は表示専用の観測で、書き込みに
// 失敗してもエンコード本体（ffmpeg 実行・media_assets への commit）は続けられる。
func (w *EncodeWorker) markEncodeAttemptRunning(ctx context.Context, recordingID int64, profile string) {
	q := sqlcgen.New(w.Pool)
	if err := q.UpsertRecordingEncodeAttemptRunning(ctx, sqlcgen.UpsertRecordingEncodeAttemptRunningParams{
		RecordingID: recordingID,
		Profile:     profile,
	}); err != nil {
		slog.Warn("encode: marking attempt running failed",
			"recording_id", recordingID, "profile", profile, "err", err)
	}
}

// markEncodeAttemptFailed は recording_encode_attempts に failed を書く
// （issue #316）。失敗はログのみ（markEncodeAttemptRunning と同じ理由）。
// 書き込みは job の ctx から切り離す（attemptWriteContext 参照）。
func (w *EncodeWorker) markEncodeAttemptFailed(ctx context.Context, recordingID int64, profile string, encodeErr error) {
	msg := truncateEncodeAttemptError(encodeErr.Error())
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	q := sqlcgen.New(w.Pool)
	if err := q.UpsertRecordingEncodeAttemptFailed(writeCtx, sqlcgen.UpsertRecordingEncodeAttemptFailedParams{
		RecordingID: recordingID,
		Profile:     profile,
		Error:       &msg,
	}); err != nil {
		slog.Warn("encode: marking attempt failed failed",
			"recording_id", recordingID, "profile", profile, "err", err)
	}
}

// clearEncodeAttempt は recording_encode_attempts の行を消す（issue #316）。
// 呼ぶのは runEncode の defer（成功時。commitEncoded と webhook 通知の後で、
// 派生物 INSERT の直後ではない）と、既に active な encoded がある冪等スキップ
// 経路。失敗はログのみ（markEncodeAttemptRunning と同じ理由）。
//
// 書き込みは job の ctx から切り離す（attemptWriteContext）。commitEncoded の
// 成功後〜defer の間（webhook 通知を挟む）に ctx がキャンセルされると、job の
// ctx では DELETE が失敗して running を主張する行が恒久的に残る --- この
// プロファイルは active な encoded を持つので ListRecordingsMissingEncodes の
// 候補から外れ、冪等スキップ経路の掃除も二度と走らない（不変条件 10:
// 何も主張していない/嘘の行を残さない）。
func (w *EncodeWorker) clearEncodeAttempt(ctx context.Context, recordingID int64, profile string) {
	writeCtx, cancel := attemptWriteContext(ctx)
	defer cancel()
	q := sqlcgen.New(w.Pool)
	if err := q.DeleteRecordingEncodeAttempt(writeCtx, sqlcgen.DeleteRecordingEncodeAttemptParams{
		RecordingID: recordingID,
		Profile:     profile,
	}); err != nil {
		slog.Warn("encode: clearing attempt failed",
			"recording_id", recordingID, "profile", profile, "err", err)
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

func probeHasSubtitles(ctx context.Context, ffprobe, input string, run func(context.Context, string, ...string) ([]byte, error)) (bool, error) {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	out, err := run(ctx, ffprobe,
		"-v", "error", "-select_streams", "s",
		"-show_entries", "stream=index", "-of", "csv=p=0", input,
	)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func probeHasSubtitlesWithTimeout(ctx context.Context, ffprobe, input string, run func(context.Context, string, ...string) ([]byte, error)) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, subtitleProbeTimeout)
	defer cancel()
	return probeHasSubtitles(probeCtx, ffprobe, input, run)
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
//
// argv の順序（issue #321 決定コメント §3）:
//
//	-hide_banner -nostats -y                      # アプリ
//	[hwaccel ブロック] [input_extra_args…]         # -i より前
//	-i INPUT
//	-c:v VC -c:a AC
//	[-vf <scaler が決めた filter>]                 # height>0 のときだけ、常に 1 個
//	[-crf N | -qp N] [-preset P]
//	[extra_args…]                                  # ユーザー（出力側）
//	-f CONTAINER -progress pipe:1 -loglevel error OUTPUT  # アプリ所有の末尾
//	[-map 0:s? -c:s webvtt -f webvtt SUBTITLE_OUTPUT]   # subtitles=webvtt のとき
//
// **extra_args は -f の前に置く。** 以前は -f の後ろだった（旧位置に依存する
// config は無い前提 --- -f は許可済みオプションに含まれないので、ユーザーが
// 相対順序に依存する余地は無い）。VOD と live で「ユーザーのオプション
// はコーデック/品質/スケール指定の後・アプリ所有の末尾の前」という 1 つの規則に
// するための移動（BuildLiveFFmpegArgs と同じ形にする）。
func BuildFFmpegArgs(profile config.EncodeProfile, input, output string) []string {
	if profile.Subtitles == "webvtt" {
		return BuildFFmpegArgsForSubtitle(profile, input, output, subtitleOutputPath(output), true)
	}
	return BuildFFmpegArgsForSubtitle(profile, input, output, "", false)
}

// BuildFFmpegArgsForSubtitle は通常の映像出力に加えて、字幕ストリームが確認
// できた場合だけ WebVTT サイドカーを出力する引数を組み立てる。
func BuildFFmpegArgsForSubtitle(profile config.EncodeProfile, input, output, subtitleOutput string, withSubtitles bool) []string {
	args := []string{
		"-hide_banner",
		"-nostats",
		"-y",
	}
	args = append(args, ffargs.PreInput(profile.HWAccel, profile.InputExtraArgs)...)
	args = append(args, "-i", input)
	args = append(args,
		"-c:v", profile.VideoCodec,
		"-c:a", profile.AudioCodec,
	)
	if filter, ok := ffargs.ScaleArgs(profile.Scaler, profile.Height); ok {
		args = append(args, "-vf", filter)
	}
	args = append(args, ffargs.QualityArgs(profile.CRF, profile.QP)...)
	if profile.Preset != "" {
		args = append(args, "-preset", profile.Preset)
	}
	if len(profile.ExtraArgs) > 0 {
		args = append(args, profile.ExtraArgs...)
	}
	args = append(args, "-f", profile.Container)
	// -progress pipe:1 は stdout に key=value。stderr はログのみ。
	args = append(args, "-progress", "pipe:1", "-loglevel", "error", output)
	if profile.Subtitles == "webvtt" && withSubtitles && subtitleOutput != "" {
		args = append(args,
			"-map", "0:s?",
			"-c:s", "webvtt",
			"-f", "webvtt",
			subtitleOutput,
		)
	}
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

// SubtitleRelPath は encoded アセットに隣接する WebVTT サイドカーのパスを返す。
// 字幕は独立した media_assets 行を持たず、encoded アセットと同じ basename の
// .vtt として管理する（issue #430 の永続化案 b）。
func SubtitleRelPath(encodedRel string) (string, error) {
	if encodedRel == "" {
		return "", fmt.Errorf("empty encoded rel_path")
	}
	encodedRel = filepath.ToSlash(encodedRel)
	ext := filepath.Ext(encodedRel)
	if ext == "" {
		return "", fmt.Errorf("encoded rel_path has no extension: %q", encodedRel)
	}
	return strings.TrimSuffix(encodedRel, ext) + ".vtt", nil
}

func subtitleOutputPath(encodedPath string) string {
	ext := filepath.Ext(encodedPath)
	if ext == "" {
		return encodedPath + ".vtt"
	}
	return strings.TrimSuffix(encodedPath, ext) + ".vtt"
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
func parseFFmpegProgress(r io.Reader, log *slog.Logger, onProgress func(time.Duration)) {
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
			// ffmpeg の out_time_ms は歴史的な誤名で、値はマイクロ秒単位。
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				lastOutTimeMs = n / 1000
			}
		case "out_time_us":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				lastOutTimeMs = n / 1000
			}
		case "progress":
			if onProgress != nil {
				onProgress(time.Duration(lastOutTimeMs) * time.Millisecond)
			}
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

// EnqueueMissingEncodes は desired（recording_encode_policy.encode_profiles。issue #159）− observed
// （active encoded media_assets）の差分を埋める encode ジョブを投入する。
//
// レベルトリガー: 呼び出し側は「いつでも」呼んでよい。既に asset があるプロファイル
// や pending なジョブはスキップされる（UniqueOpts）。ingest 成功後のヒント投入と
// `POST /api/recordings/{id}/encode-profiles` のヒントジョブから使う。
//
// **プロファイル名が現在の設定に存在するかは見ない。** 一度きりのヒント経路では
// それでよい --- 設定から消えたプロファイルの投入は EncodeWorker が
// `unknown encode profile` で失敗させ、その失敗が運用者への通知になる。
// 15 分ごとに繰り返す定期パスが同じことをすると失敗を無限に作り続けるので、
// そちらは EnqueueMissingEncodesForKnownProfiles を使う。
func EnqueueMissingEncodes(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64) error {
	return enqueueMissingEncodes(ctx, inserter, pool, recordingID, nil)
}

// EnqueueMissingEncodesForKnownProfiles は EnqueueMissingEncodes と同じ判定を
// 行うが、投入対象を known（現在の encode.profiles の名前）に含まれる
// プロファイルだけに絞る。encode の定期 reconcile パス
// （EncodeReconcileWorker）が使う。
//
// known が空なら 1 件も投入しない（設定にプロファイルが 1 つも無い構成では、
// 投入しても EncodeWorker が全部弾く）。判定を 2 か所に分けないため、絞り込み
// 以外のロジック（原本の有無・ポリシー行の有無・observed の確認）は
// EnqueueMissingEncodes と同じ 1 つの実装を通る。
func EnqueueMissingEncodesForKnownProfiles(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64, known []string) error {
	set := make(map[string]struct{}, len(known))
	for _, name := range known {
		set[name] = struct{}{}
	}
	return enqueueMissingEncodes(ctx, inserter, pool, recordingID, set)
}

// enqueueMissingEncodes は上 2 つの実装本体。known が nil なら desired を絞らない
// （nil と空マップは意味が違う: 空マップは「投入してよいプロファイルが 1 つも
// 無い」）。
func enqueueMissingEncodes(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64, known map[string]struct{}) error {
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

	// desired（issue #159。recording_encode_policy 衛星表）。行が無い
	// （未凍結）は「エンコード対象のプロファイルが無い」と同じに扱う ---
	// ここに来る時点で原本は active（上のチェック）なので、通常は
	// resolveAndSnapshotEncodePolicy が同一 tx で行を作っているはずだが、
	// ingest 完了直後の競合（EncodeEnqueueHintArgs のヒントが原本コミットの
	// 直後に走る等）を黙って落とさないよう ErrNoRows も no-op で許容する。
	policy, err := q.GetRecordingEncodePolicy(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("loading recording encode policy %d: %w", recordingID, err)
	}
	if len(policy.EncodeProfiles) == 0 {
		return nil
	}

	for _, name := range policy.EncodeProfiles {
		if name == "" {
			continue
		}
		if known != nil {
			if _, ok := known[name]; !ok {
				continue
			}
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

// EncodeEnqueueHintArgs は事後追加されたエンコードプロファイルを反映するヒント
// ジョブの引数（issue #133、凍結の例外としての事後追加。docs/storage.md §6
// 「原本 TS の保持ポリシー」）。api の POST /api/recordings/{id}/encode-profiles
// が recording_encode_policy.encode_profiles の UPDATE と同一トランザクションで InsertTx する
// （internal/api/recordings.go の insertEncodeEnqueueHint。rules.go の
// insertRulerPassHint と同じヒント経路のパターン）。
//
// # ヒント経由にした理由（api → worker の結合パターンの一貫性）
//
// EnqueueMissingEncodes 自体は ffmpeg を exec しない（DB 読み取りと River Insert
// のみ）ため、api ハンドラから直接呼んでも不変条件 4（ffmpeg/ffprobe の exec は
// worker/streamer のみ）には反しない。それでも api → worker の既存の結合パターン
// （RulerPassArgs、rules.go の insertRulerPassHint）に揃えてヒントジョブ経由にした
// --- 「recording_encode_policy.encode_profiles の更新」と「不足分の encode ジョブ投入」を
// api ハンドラの 1 関数に同居させず、後者の実行を常に worker ロールの中で完結
// させるため（一貫性。api が worker の実行ロジックを直接呼ぶ経路を増やさない）。
type EncodeEnqueueHintArgs struct {
	RecordingID int64 `json:"recording_id"`
}

// Kind は River ジョブの種別名を返す。
func (EncodeEnqueueHintArgs) Kind() string { return "encode_enqueue_hint" }

// InsertOpts は encode キューと UniqueOpts を返す。recording_id 単位で pending 中の
// ヒントに合流させる（同じ録画への連続した追加依頼が重複ジョブを積まないように。
// RulerPassArgs / EncodeJobArgs と同じ ByArgs + ByState の形）。
func (EncodeEnqueueHintArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: encodeQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: pendingJobStates,
		},
	}
}

// EncodeEnqueueHintWorker は EncodeEnqueueHintArgs を受けて EnqueueMissingEncodes
// を呼ぶだけの薄いワーカー。ロジックは持たない（EnqueueMissingEncodes にそのまま
// 委譲する。レベルトリガーなので呼び出しが遅れても・重複しても収束する）。
type EncodeEnqueueHintWorker struct {
	river.WorkerDefaults[EncodeEnqueueHintArgs]
	Pool *pgxpool.Pool
}

// Work は EnqueueMissingEncodes を呼び、recording_encode_policy.encode_profiles（desired）と
// active encoded media_assets（observed）の差分を埋める encode ジョブを投入する。
//
// river.ClientFromContextSafely でジョブ実行中の Client を取り出す。取れない
// （単体テストで Work を直接呼んだ等）場合はエラーを返して失敗させる ---
// ruler_pass 完了時の reconcile_pass ヒント（ruler_pass.go）とは異なり、この
// ジョブ自体の主目的が「encode ジョブを実際に投入すること」であるため、client が
// 無いからと黙って何もしないとユーザーの事後追加依頼がサイレントに消える。
func (w *EncodeEnqueueHintWorker) Work(ctx context.Context, job *river.Job[EncodeEnqueueHintArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("encode enqueue hint: getting river client: %w", err)
	}
	return EnqueueMissingEncodes(ctx, client, w.Pool, job.Args.RecordingID)
}
