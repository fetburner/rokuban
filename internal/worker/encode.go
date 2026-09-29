package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/webhook"
)

// EncodeWorker は原本 media_asset から構造化プロファイルで派生物を作る。
//
// ストレージ契約（docs/storage.md §3）:
//  1. 原本は mediapath.Resolve で読む
//  2. ffmpeg 出力は scratch に書く（media_dir へ直接書かない）
//  3. 進捗は ffmpeg -progress pipe:1（stderr スクレイピング禁止）
//  4. 成功時のみ scratch → media へストリームコピー + fsync → media_assets INSERT
//  5. 失敗時はコミット行を残さず、scratch は best-effort で掃除
//
// # 並走する 2 本の encode から canonical を守る
//
// job lock（advisory lock + heartbeat）は ffmpeg の排他ではなく、lock を失っても
// 実行中の encode を cancel しない（[ingest_job_lock.go] の jobLockIdleSessionTimeout
// 参照）。そのため同じ (recording, profile) の encode が 2 本並走しうる。排他は
// 2 段で持つ:
//
//   - **scratch は (recording, profile) ごとの flock で直列化する**（lockEncodeScratch）。
//     試行ごとに scratch を分ける案は採らない --- プロセス死で残った scratch を
//     回収する仕組みが無く、試行ごとの一意名にすると途中死のたびにディスクを食う。
//     固定パスなら次の試行が同じディレクトリを作り直して最後に消す。取れなかった
//     実行は待たずに失敗させて River に戻す（保持側は数十分動きうるので blocking は
//     枠を無駄に捨てる。戻したジョブは冪等判定で短絡するか、保持側の終了後に走る）。
//   - **canonical は同じディレクトリの temp へ書いて rename で公開する**（publishEncoded）。
//     `O_TRUNC` で canonical を直接開くと、並走側がコピー中の canonical を切り詰め、
//     `size_bytes` と中身が食い違うファイルを commit しうる。rename は同じ FS 内で
//     原子的なので、読者は常に完全なファイルを見る。rel_path filesystem lock を
//     公開から DB commit まで保持し、孤児回収の unlink と直列化する。
//
// **前提は ingest と同じ**: この排他は対象 FS 上の協調的な POSIX flock に依存する
// （RWX のメディア越しに効くかは未検証。docs/storage/contract.md §3 ルール 4）。
// flock が効かない構成でも rename 自体は原子的なので、途中の内容が観測されることは
// 無い（残るのは「最後に commit した行が最後に置いたファイルを指すか」の順序だけ）。
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
	river.WorkerDefaults[jobs.EncodeJobArgs]
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
// プロセス終了を待つ）。プロセス死で running のまま残ったジョブは、
// encode_reconcile が job-id advisory lock の解放を確認して回収する。
func (w *EncodeWorker) Timeout(*river.Job[jobs.EncodeJobArgs]) time.Duration {
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
func (w *EncodeWorker) Work(ctx context.Context, job *river.Job[jobs.EncodeJobArgs]) error {
	log := slog.With("recording_id", job.Args.RecordingID, "profile", job.Args.Profile)

	// Work の開始から終了まで、ジョブ ID 固有の advisory lock を保持する。
	// encode_reconcile の回収側が同じキーを pg_try できた場合だけ、元プロセスが
	// 死んでセッションが解放されたと確定できる。lock は ffmpeg の出力を排他する
	// ものではなく、recovery が live job を時刻だけで殺さないための生存確認である。
	jobLock, acquired, err := acquireEncodeJobLock(ctx, w.Pool, job.ID, defaultJobLockTimeout)
	if err != nil {
		return fmt.Errorf("acquiring encode job lock: %w", err)
	}
	if !acquired {
		// 断定はしない: この分岐には、別プロセスが本当に実行中の場合だけでなく、
		// encode_reconcile の回収側が同じキーを一瞬 try して保持している場合も落ちる。
		log.Warn("encode: job advisory lock is held by another session, deferring", "job_id", job.ID)
		return fmt.Errorf("encode: job %d advisory lock is held by another session; deferring", job.ID)
	}
	defer jobLock.release()

	err = w.runEncode(ctx, job)
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
func (w *EncodeWorker) runEncode(ctx context.Context, job *river.Job[jobs.EncodeJobArgs]) (err error) {
	args := job.Args
	log := slog.With("recording_id", args.RecordingID, "profile", args.Profile)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.EncodeDuration.Observe(time.Since(started).Seconds())
		metrics.EncodeJobs.WithLabelValues(result).Inc()
	}()

	// 冪等: 既に active な encoded があれば何もしない。リークした古い試行行
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

	// 冪等: 既に active な encoded があれば何もしない。リークした古い試行行
	// （不変条件 10: 完了しているのに failed/running を名乗る行を残さない）が
	// あれば掃除する。
	//
	// cut プロファイルの冪等は「active で、かつ凍結した区間が現在の量子化 keep と
	// 一致する」である。**区間がずれていれば active でも作り直す** ---
	// ユーザーがチャプターを直した後に再エンコードのジョブが来たときに、
	// 古いカット版を「完了済み」と読むと編集が反映されない。
	//
	// 試行の観測（上の markEncodeAttemptRunning）より後に置く: ここの失敗も
	// ジョブの失敗として recording_encode_attempts に残す（未定義プロファイルを
	// ここで先に弾くと、観測が始まる前に return して failed 行が残らない）。
	cut, err := w.loadCutContext(ctx, args.RecordingID, args.Profile)
	if err != nil {
		return err
	}
	already, err := w.hasActiveEncoded(ctx, args.RecordingID, args.Profile, cut)
	if err != nil {
		return fmt.Errorf("checking existing encoded asset: %w", err)
	}
	if already {
		log.Info("encode: encoded asset already committed, skipping")
		result = "success"
		w.clearEncodeAttempt(ctx, args.RecordingID, args.Profile)
		return nil
	}

	profile, originalRelPath, inputPath, duration, err := w.prepareEncodeInput(ctx, args, log)
	if err != nil {
		return err
	}
	// reporter.start をここで呼ぶ（prepareEncodeInput 側ではない）理由は
	// prepareEncodeInput の doc コメント参照。
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

	// 置き換えでは新しい世代のパスに置いてから旧パスを消す。旧パス（生きている行）
	// は同じ tx より前の観測なので、ここで読んでおく。
	var prevRelPath string
	generation := 0
	if cut != nil {
		var prevState string
		prevRelPath, prevState, err = w.loadEncodedRelPath(ctx, args.RecordingID, profile.Name)
		if err != nil {
			return err
		}
		generation = nextCutGeneration(prevRelPath, profile.Name)
		if prevState != "active" {
			// tombstone / deleting のファイルは自分のものではない（消えているか、
			// 削除 reconcile が処理中）。unlink の対象から外す。
			prevRelPath = ""
		}
	}
	relPath, err := EncodedRelPath(originalRelPath, profile.Name, profile.Container, generation)
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
	releaseScratch, err := reserveEncodeScratch(scratchDir, log)
	if err != nil {
		return err
	}
	defer releaseScratch()

	withSubtitles, subtitleOut, err := w.prepareEncodeSubtitles(ctx, profile, inputPath, scratchOut, log)
	if err != nil {
		return err
	}
	// cut プロファイルはストリームとフィルタグラフをアプリが握る（出力側に -map を
	// 書けないので ffmpeg の既定の選択を再現する。ffargs.SelectDefaultStreams）。
	var filterArgs *ffargs.CutFilterResult
	if cut != nil {
		filterArgs, err = w.buildCutFilter(ctx, profile, inputPath, cut.keep)
		if err != nil {
			return err
		}
	}
	argsIn := encodeCommandInput{
		profile:       profile,
		inputPath:     inputPath,
		scratchOut:    scratchOut,
		subtitleOut:   subtitleOut,
		withSubtitles: withSubtitles,
		filter:        filterArgs,
	}
	if err := w.runEncodeCommand(ctx, argsIn, reportProgress, log); err != nil {
		return err
	}
	// 字幕は同じ ffmpeg 起動の別出力なので filtergraph の trim が効かない。
	// 書き出した後に同じ keep 区間の写像で時刻を付け替える。
	if withSubtitles && cut != nil {
		if err := retimeSubtitleSidecar(subtitleOut, cut.keep); err != nil {
			return err
		}
	}
	size, err := w.publishEncoded(ctx, encodePublishInput{
		recordingID:   args.RecordingID,
		profile:       profile.Name,
		relPath:       relPath,
		finalPath:     finalPath,
		scratchOut:    scratchOut,
		subtitleOut:   subtitleOut,
		withSubtitles: withSubtitles,
		cut:           cut,
	})
	if err != nil {
		return err
	}

	// 旧パスの unlink は commit の後。**失敗しても孤児回収に任せる**（旧行はもう
	// 存在しないので、cleanup が「メディア上にあるが DB に載っていないファイル」
	// として拾う）。commit の前に消すと、commit が失敗したときに生きている行が
	// 指すファイルを失う。
	if prevRelPath != "" && prevRelPath != relPath {
		w.removeReplacedEncoded(prevRelPath, log)
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

// cutContext は cut プロファイルの encode に要る、録画側の事実から導出した
// keep 区間。cut でないプロファイルでは nil。
type cutContext struct {
	keep []chapters.Range
}

// loadCutContext は profile が cut のとき、有効なタイムラインから keep 区間を
// 導出する。cut でなければ (nil, nil)。
//
// **所有の行があることを前提にする。** 確認前にカット版をコミットすると、誤検出の
// まま本編が削られ、原本がごみ箱を経由せずに消えて取り返せなくなる。ジョブの投入側
// （enqueueMissingEncodes / ListMissingEncodeProfiles）が所有していない録画を除外
// しているので、ここに来る cut ジョブは所有済みのはずである。来なければジョブの
// 失敗として現れる（黙って切らない）。
//
// keep が空（全部カット）も同じく失敗させる。keep_ranges の CHECK が空を拒否する
// ので、先に落とさないと tx ごとロールバックする。
func (w *EncodeWorker) loadCutContext(ctx context.Context, recordingID int64, profileName string) (*cutContext, error) {
	profile, ok := w.Profiles.Profile(profileName)
	if !ok {
		// 未知のプロファイルは prepareEncodeInput が同じ文言で落とす（ここは
		// cut かどうかだけを決められればよい）。
		return nil, fmt.Errorf("unknown encode profile %q", profileName)
	}
	if !profile.Cut {
		return nil, nil
	}
	q := sqlcgen.New(w.Pool)
	keep, owned, err := currentCutKeep(ctx, q, recordingID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, fmt.Errorf("cut profile %q requires adopted chapters for recording %d", profileName, recordingID)
	}
	if len(keep) == 0 {
		return nil, fmt.Errorf("recording %d has no keep ranges (every span is cut)", recordingID)
	}
	return &cutContext{keep: keep}, nil
}

// currentCutKeep は録画の有効なタイムラインから keep 区間を導出する。owned は
// ユーザーが確認済みか（所有の行の有無）。
//
// **導出は chapters.Derive 1 か所を通る**（api の GET chapters・カット版の
// encode・「編集前の内容です」の判定が同じ関数を使う）。所有済みなので自動層は
// 読まない。
func currentCutKeep(ctx context.Context, q *sqlcgen.Queries, recordingID int64) (keep []chapters.Range, owned bool, err error) {
	state, err := q.GetRecordingChapterState(ctx, recordingID)
	if err != nil {
		return nil, false, fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
	}
	if !state.Owned {
		return nil, false, nil
	}
	raw, err := q.GetRecordingChapterSpansJSON(ctx, recordingID)
	if err != nil {
		return nil, false, fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
	}
	var spans []chapters.Span
	if err := json.Unmarshal(raw, &spans); err != nil {
		return nil, false, fmt.Errorf("decoding chapter spans for recording %d: %w", recordingID, err)
	}
	return chapters.KeepRanges(chapters.Derive(true, spans, nil, state.ProgramDurationMs)), true, nil
}

// assetKeepRanges は media_asset に凍結された keep 区間を読む。凍結していない
// （cut でない版）なら空。
func assetKeepRanges(ctx context.Context, q *sqlcgen.Queries, assetID int64) ([]chapters.Range, error) {
	raw, err := q.GetMediaAssetKeepRangesJSON(ctx, assetID)
	if err != nil {
		return nil, fmt.Errorf("loading frozen cut ranges: %w", err)
	}
	var ranges []chapters.Range
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return nil, fmt.Errorf("decoding frozen cut ranges: %w", err)
	}
	return ranges, nil
}

// buildCutFilter は入力のストリームを選び、keep 区間で切る filtergraph を組む。
func (w *EncodeWorker) buildCutFilter(ctx context.Context, profile config.EncodeProfile, inputPath string, keep []chapters.Range) (*ffargs.CutFilterResult, error) {
	video, audio, err := w.selectStreams(ctx, inputPath)
	if err != nil {
		return nil, err
	}
	hwUpload := profile.HWAccel != nil && profile.HWAccel.Kind == "vaapi"
	result, err := ffargs.CutFilterComplex(keep, video, audio, profile.Scaler, profile.Height, profile.Deinterlace, hwUpload)
	if err != nil {
		return nil, fmt.Errorf("building cut filtergraph: %w", err)
	}
	return &result, nil
}

// selectStreams は ffprobe で入力のストリームを列挙し、ffmpeg の既定の選択と
// 同じ規則で映像 1 本・音声 1 本を選ぶ（絶対ストリーム番号を返す）。
//
// **既定を再現する理由**: cut でない版は出力側に -map を指定せず ffmpeg の既定に
// 任せている。cut 版だけ別のストリームが選ばれると、同じ録画の 2 つの版で
// 音声が食い違う。
func (w *EncodeWorker) selectStreams(ctx context.Context, inputPath string) (video, audio int, err error) {
	ffprobe := w.FFprobe
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	probeCtx, cancel := context.WithTimeout(ctx, streamProbeTimeout)
	defer cancel()
	out, err := commandOutput(probeCtx, ffprobe,
		"-v", "error",
		"-show_entries", "stream=index,codec_type,width,height,channels",
		"-of", "csv=p=0", inputPath,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("probing streams of %s: %w", inputPath, err)
	}
	streams, err := parseStreamCSV(string(out))
	if err != nil {
		return 0, 0, err
	}
	video, audio, ok := ffargs.SelectDefaultStreams(streams)
	if !ok {
		return 0, 0, fmt.Errorf("input %s has no video or no audio stream", inputPath)
	}
	return video, audio, nil
}

// parseStreamCSV は `-of csv=p=0` の `index,codec_type,width,height,channels`
// 行を読む。値が無い列は空文字（映像に channels は無い）。
func parseStreamCSV(out string) ([]ffargs.StreamInfo, error) {
	var streams []ffargs.StreamInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			return nil, fmt.Errorf("unexpected ffprobe stream line %q", line)
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("unexpected ffprobe stream index %q: %w", fields[0], err)
		}
		s := ffargs.StreamInfo{Index: index, CodecType: fields[1]}
		if len(fields) > 2 {
			s.Width, _ = strconv.Atoi(fields[2])
		}
		if len(fields) > 3 {
			s.Height, _ = strconv.Atoi(fields[3])
		}
		if len(fields) > 4 {
			s.Channels, _ = strconv.Atoi(fields[4])
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// retimeSubtitleSidecar は書き出した WebVTT の時刻をカット後の時間軸へ付け替える。
// 空になった結果（全キューが CM の中）はそのまま置く --- 元のファイルを消すと
// copyEncodeOutputs のサイズ検査が落ちる。
func retimeSubtitleSidecar(path string, keep []chapters.Range) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading subtitle sidecar: %w", err)
	}
	out, err := chapters.RetimeVTT(data, keep)
	if err != nil {
		return fmt.Errorf("retiming subtitle sidecar: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing retimed subtitle sidecar: %w", err)
	}
	return nil
}

// prepareEncodeInput はプロファイル・原本・入力パスを解決し、進捗報告に使う
// 入力長（duration）を返す。duration <= 0 は「取得できなかった」ことを表し、
// 呼び出し元はこれを進捗報告なし（reporter.start を呼ばない）の合図に使う。
//
// reporter.start（goroutine を起動する）はこの関数の外、呼び出し元 runEncode が
// duration を受け取った後で呼ぶ。ここで呼ばないのは、この関数が複数の
// return 文を持つため、start より後に error を返す段が増えた瞬間に呼び出し元が
// defer し損ねて goroutine が漏れる経路を構造的に作らないため。
func (w *EncodeWorker) prepareEncodeInput(ctx context.Context, args jobs.EncodeJobArgs, log *slog.Logger) (config.EncodeProfile, string, string, time.Duration, error) {
	profile, ok := w.Profiles.Profile(args.Profile)
	if !ok {
		// 設定から消えたプロファイルは再試行しても直らない。
		return config.EncodeProfile{}, "", "", 0, fmt.Errorf("unknown encode profile %q", args.Profile)
	}
	orig, err := w.loadOriginal(ctx, args.RecordingID)
	if err != nil {
		return config.EncodeProfile{}, "", "", 0, err
	}
	inputPath, err := mediapath.Resolve(w.MediaDir, orig.RelPath)
	if err != nil {
		return config.EncodeProfile{}, "", "", 0, fmt.Errorf("resolving original path: %w", err)
	}
	if _, err := os.Stat(inputPath); err != nil {
		return config.EncodeProfile{}, "", "", 0, fmt.Errorf("original file %s: %w", inputPath, err)
	}
	duration, probeErr := probeEncodeDuration(ctx, w.FFprobe, inputPath, encodeDurationProbeTimeout, commandOutput)
	if probeErr != nil {
		log.Warn("encode: probing input duration failed; progress percentage disabled", "err", probeErr)
	}
	return profile, orig.RelPath, inputPath, duration, nil
}

// prepareEncodeSubtitles は字幕ストリームの有無を調べ、サイドカー出力先を返す。
//
// ffmpeg は字幕ストリームが無い状態で WebVTT 出力を要求すると終了する。
// 先に ffprobe で存在を確認し、字幕がある録画だけサイドカー出力を有効に
// することで、局や番組によって字幕 PID が無い録画でもエンコード本体を
// 落とさない（issue #430 の optional map の罠）。
func (w *EncodeWorker) prepareEncodeSubtitles(ctx context.Context, profile config.EncodeProfile, inputPath, scratchOut string, log *slog.Logger) (bool, string, error) {
	withSubtitles := false
	var err error
	if profile.Subtitles == "webvtt" {
		withSubtitles, err = probeHasSubtitlesWithTimeout(ctx, w.FFprobe, inputPath, commandOutput)
		if err != nil {
			log.Warn("encode: probing subtitle streams failed; continuing without subtitle sidecar", "err", err)
		}
	}
	// サイドカーの出力パスは scratchOut と同じディレクトリ・basename に .vtt を
	// 付けたもの（BuildFFmpegArgs が内部で導出するのと同じ規則を
	// mediapath.SubtitleSibling で共有する）。withSubtitles=false のときは
	// BuildFFmpegArgs がこの引数を使わないので、空文字などの特別扱いは要らない。
	subtitleOut, err := mediapath.SubtitleSibling(scratchOut)
	if err != nil {
		return false, "", fmt.Errorf("deriving subtitle sidecar path: %w", err)
	}
	return withSubtitles, subtitleOut, nil
}

// encodeCommandInput は ffmpeg 起動 1 回ぶんの入力（runEncodeCommand の引数）。
// 引数が 6 個を超えたのでまとめた（順序を間違えても型が同じで気付けない
// 組み合わせが 3 つ以上ある）。
type encodeCommandInput struct {
	profile       config.EncodeProfile
	inputPath     string
	scratchOut    string
	subtitleOut   string
	withSubtitles bool

	// filter は cut プロファイルの filtergraph と -map（cut でなければ nil）。
	// ストリームの選択は ffprobe の結果に依存するので、BuildFFmpegArgs の外で
	// 決めて渡す。
	filter *ffargs.CutFilterResult
}

// runEncodeCommand は ffmpeg を実行し、進捗を読み取り、scratch 出力を検証する。
func (w *EncodeWorker) runEncodeCommand(ctx context.Context, in encodeCommandInput, reportProgress func(time.Duration), log *slog.Logger) error {
	profile, inputPath, scratchOut, subtitleOut, withSubtitles := in.profile, in.inputPath, in.scratchOut, in.subtitleOut, in.withSubtitles
	ffmpeg := w.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, ffmpeg, BuildFFmpegArgs(profile, inputPath, scratchOut, withSubtitles, in.filter)...)
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
	if err := encodeCommandError(ctx, cmd, waitErr, stderrBuf.String(), log); err != nil {
		return err
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
	return nil
}

// encodeCommandError は ffmpeg の終了結果をジョブエラーへ変換する。
//
// コンテキストキャンセルは River の停止やタイムアウト。
func encodeCommandError(ctx context.Context, cmd *exec.Cmd, waitErr error, stderr string, log *slog.Logger) error {
	if waitErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// ffmpeg 自体は exit 0 で完走したが、孫プロセスが stdout/stderr の
		// fd を握ったままで WaitDelay が先に切れた（この PR が扱う
		// ハングの exit 0 版）。以降の os.Stat / サイズ検査が出力を守るので
		// 失敗にはしないが、再試行ループから見分けられるよう記録は残す。その
		// 検査は呼び出し元 runEncodeCommand が cmd.Wait() の直後（この関数を
		// 呼んだすぐ後）に行う --- この関数はエラー変換だけを担い、出力の
		// 検証はしない。
		log.Warn("encode: ffmpeg exited successfully but WaitDelay expired before I/O completed", "wait_delay", workerExecWaitDelay)
		return nil
	}
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("ffmpeg failed: %w (stderr: %s)", waitErr, stderr)
	}
	return fmt.Errorf("ffmpeg failed: %w", waitErr)
}

// renameEncodedFile は公開プロトコルの rename をテストから観測・失敗注入できるように
// するフックである。stage → rename → 親 dir fsync → DB コミット の順序と、
// 「rename まで canonical に触らない」ことを、ファイルシステムの実体に依存せず
// 検証するために置いている（ingest の renameIngestFile と同じ理由）。
var renameEncodedFile = os.Rename

// stagedEncodedFile は media ディレクトリへ置く前の一時ファイル。finalPath と同じ
// ディレクトリに置くので rename は同一 FS 内で完結する（scratch からの rename は
// `EXDEV` になる。docs/storage/contract.md §3 ルール 2）。
type stagedEncodedFile struct {
	tempPath  string
	finalPath string
	size      int64
}

// stageEncodedFile は src（scratch）を finalPath と同じディレクトリの一時ファイルへ
// ストリームコピー + fsync する。名前は公開前の一時ファイルと分かる予約接頭辞を
// 付け、後発の孤児回収が「途中死で残った置き忘れ」として mtime 猶予つきで拾える
// ようにする（公開前なので canonical の名前は使わない）。
func stageEncodedFile(src, finalPath string) (stagedEncodedFile, error) {
	dir := filepath.Dir(finalPath)
	// CreateTemp は 0600 で作る。公開後のファイルは canonical なので、コピー元の
	// streamCopyFile と同じ 0644 に揃える（別 UID の streamer が読む構成がある）。
	temp, err := os.CreateTemp(dir, mediapath.EncodeTempFilePrefix+"*")
	if err != nil {
		return stagedEncodedFile{}, fmt.Errorf("creating staged output in media dir: %w", err)
	}
	tempPath := temp.Name()
	_ = temp.Close()
	if err := os.Chmod(tempPath, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return stagedEncodedFile{}, fmt.Errorf("chmod staged output: %w", err)
	}
	size, err := streamCopyFile(src, tempPath)
	if err != nil {
		// コピーに失敗した一時ファイルは公開しない。残ると mtime 猶予の後に孤児
		// 回収が拾うだけなので、ここで消す。
		_ = os.Remove(tempPath)
		return stagedEncodedFile{}, fmt.Errorf("staging output in media dir: %w", err)
	}
	return stagedEncodedFile{tempPath: tempPath, finalPath: finalPath, size: size}, nil
}

// publish は staged 出力を canonical へ rename で公開し、親ディレクトリを fsync する。
func (s stagedEncodedFile) publish() error {
	if err := renameEncodedFile(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("publishing staged output to canonical path: %w", err)
	}
	if err := syncIngestDirectory(s.finalPath); err != nil {
		return fmt.Errorf("syncing canonical parent directory: %w", err)
	}
	return nil
}

func (s stagedEncodedFile) discard() {
	if s.tempPath == "" {
		return
	}
	_ = os.Remove(s.tempPath)
}

// encodePublishInput は publishEncoded の入力。引数が 4 つを超え、順序を間違えても
// 型が同じで気付けない組み合わせがあるのでまとめた。
type encodePublishInput struct {
	recordingID   int64
	profile       string
	relPath       string
	finalPath     string
	scratchOut    string
	subtitleOut   string
	withSubtitles bool
	// cut はカット版のときだけ非 nil。commit で凍結した keep 区間を同じ tx で
	// 差し替える（commitEncoded 参照）。
	cut *cutContext
}

// publishEncoded は検証済みの scratch 出力を canonical へ公開し、media_assets を
// commit する。**stage → rename → 親 dir fsync → DB commit** を、canonical の
// rel_path filesystem lock の下で 1 続きに行う（ingest の commit と同じ確定
// プロトコル。順序を逆にしない --- DB commit を先にすると、行が指す実体の欠落を
// 作る）。
//
// lock の範囲を rename だけにしない理由は、孤児回収が同じ lock を非 blocking で
// 取ってから canonical を unlink するためである。公開と commit の間で切ると、
// コミット前の行（まだ誰からも見えない）と消えた実体の組み合わせが残りうる。
//
// 戻り値の size は置いたファイルのバイト数（media_assets.size_bytes）。
func (w *EncodeWorker) publishEncoded(ctx context.Context, in encodePublishInput) (int64, error) {
	// 親ディレクトリが無いと rel_path lock file を開けない（lock file は canonical と
	// 同じディレクトリに置く）。ingest はストレージ層が commit 前に作るが、encode は
	// ここが最初の書き込みなので、lock の前に作る。
	if err := os.MkdirAll(filepath.Dir(in.finalPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", filepath.Dir(in.finalPath), err)
	}
	fileLock, err := lockMediaRelPathFile(ctx, in.finalPath, in.relPath)
	if err != nil {
		return 0, fmt.Errorf("locking canonical file protocol: %w", err)
	}
	defer func() { _ = fileLock.Close() }()

	staged, err := stageEncodedFile(in.scratchOut, in.finalPath)
	if err != nil {
		return 0, err
	}

	var stagedSubtitle stagedEncodedFile
	if in.withSubtitles {
		subtitleRelPath, err := mediapath.SubtitleSibling(in.relPath)
		if err != nil {
			staged.discard()
			return 0, fmt.Errorf("building subtitle rel_path: %w", err)
		}
		subtitleFinalPath, err := mediapath.Resolve(w.MediaDir, subtitleRelPath)
		if err != nil {
			staged.discard()
			return 0, fmt.Errorf("resolving subtitle path: %w", err)
		}
		// サイドカーは本体と同じ lock の下で置く（1 つの行 = 1 つの組として
		// 公開する。別 lock を取るとロック順序が増えるだけで得るものが無い）。
		stagedSubtitle, err = stageEncodedFile(in.subtitleOut, subtitleFinalPath)
		if err != nil {
			staged.discard()
			return 0, fmt.Errorf("staging subtitle sidecar: %w", err)
		}
	}

	if err := staged.publish(); err != nil {
		staged.discard()
		stagedSubtitle.discard()
		return 0, err
	}
	if in.withSubtitles {
		if err := stagedSubtitle.publish(); err != nil {
			stagedSubtitle.discard()
			return 0, err
		}
	}

	if err := w.commitEncoded(ctx, in.recordingID, in.profile, in.relPath, staged.size, in.cut); err != nil {
		// DB コミット失敗: media 上のファイルは DB に載らないので孤児。cleanup が
		// 回収する（rename 済みのファイルをここで消すと、commit が実は成功していた
		// 場合に生きている行が指す実体を失う）。
		return 0, fmt.Errorf("committing encoded asset: %w", err)
	}
	return staged.size, nil
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

// streamProbeTimeout は cut プロファイルで選ぶストリームを調べる ffprobe の上限。
// 入力ファイルは既にローカルにある（原本）ので、字幕 probe と同じ 30 秒で足りる。
const streamProbeTimeout = 30 * time.Second

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
// プロファイルは active な encoded を持つので ListMissingEncodeProfiles の
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

// hasActiveEncoded は冪等スキップの判定。cut が nil（cut でないプロファイル）なら
// 「active な encoded があるか」だけを見る。cut が非 nil なら、それに加えて
// **凍結した区間が現在の量子化 keep と一致すること**を要求する（一致しなければ
// 作り直す。encode の doc コメント参照）。
//
// 比較は量子化後の値どうしで行う。keep は chapters.Derive を通った時点で量子化
// 済み、凍結側は書き込み時に量子化して入れてある（media_asset_cuts の
// コメント参照）。
func (w *EncodeWorker) hasActiveEncoded(ctx context.Context, recordingID int64, profile string, cut *cutContext) (bool, error) {
	q := sqlcgen.New(w.Pool)
	if cut == nil {
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
	row, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.State != "active" {
		return false, nil
	}
	frozen, err := assetKeepRanges(ctx, q, row.ID)
	if err != nil {
		return false, err
	}
	return chapters.SameRanges(frozen, cut.keep), nil
}

// loadEncodedRelPath は (recording_id, profile) の encoded 行の rel_path と state
// を返す。**state を問わない**: 世代番号は tombstone からも導出する（次のカット版が
// 同じパスを再利用しないため）。行が無ければ空文字。
//
// state も返すのは、**unlink してよいのは active な行だけ**だからである。
// tombstone のファイルは既に消えており、`deleting` のものは削除 reconcile が
// 処理中で、先回りして消すと相手に偽のエラーログを出させる。
func (w *EncodeWorker) loadEncodedRelPath(ctx context.Context, recordingID int64, profile string) (relPath, state string, err error) {
	q := sqlcgen.New(w.Pool)
	row, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if errors.Is(err, pgx5.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("loading existing encoded asset: %w", err)
	}
	return row.RelPath, row.State, nil
}

// nextCutGeneration は旧 rel_path の世代番号 +1 を返す（無ければ 1）。
//
// **1 世代目から `.g1` を付ける。** 「世代番号の無いパス」という例外を作ると、
// 置き換えのたびに「旧パスに世代が付いているか」を分岐で扱うことになる。
// 番号が読めないパス（手で置かれた行など）は 1 に戻す --- 衝突したら部分一意索引が
// 弾くので、黙って他人のファイルを上書きすることはない。
func nextCutGeneration(prevRelPath, profileName string) int {
	if prevRelPath == "" {
		return 1
	}
	stem := strings.TrimSuffix(prevRelPath, filepath.Ext(prevRelPath))
	// `..._{profile}.g{n}` の n を読む。プロファイル名に `.g<数字>` が含まれても
	// 末尾だけを見るので取り違えない。
	safeProfile, err := sanitizeProfileForPath(profileName)
	if err != nil {
		return 1
	}
	prefix := safeProfile + ".g"
	i := strings.LastIndex(stem, prefix)
	if i < 0 {
		return 1
	}
	n, err := strconv.Atoi(stem[i+len(prefix):])
	if err != nil || n < 1 {
		return 1
	}
	return n + 1
}

// removeReplacedEncoded は置き換えで不要になった旧ファイルを消す。
//
// **失敗はログのみ。** 旧パスはもう media_assets のどの行からも指されていないので、
// 既存の孤児回収（cleanup）が「メディア上にあるが DB に載っていないファイル」と
// して拾う。ここでエラーを返すと、置き換え自体は成功しているのにジョブが失敗し、
// 再試行が新しい世代をさらに作る。
func (w *EncodeWorker) removeReplacedEncoded(relPath string, log *slog.Logger) {
	path, err := mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		log.Warn("encode: could not resolve replaced path", "rel_path", relPath, "err", err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("encode: removing replaced encoded file failed; orphan collection will pick it up",
			"rel_path", relPath, "err", err)
	}
	// サイドカーも同じ世代で置き換わる。
	if sidecarRel, err := mediapath.SubtitleSibling(relPath); err == nil {
		if sidecar, err := mediapath.Resolve(w.MediaDir, sidecarRel); err == nil {
			_ = os.Remove(sidecar)
		}
	}
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

// encodeScratchLockPath は scratch ディレクトリの隣に置く lock file のパスを返す。
// **scratch の中に置かない**: 終了時の RemoveAll が lock file ごと消すと、次の実行が
// 新しい inode を作って、まだ生きている実行の lock と別物になる。
func encodeScratchLockPath(scratchDir string) string {
	return scratchDir + ".lock"
}

// lockEncodeScratch は (recording, profile) の encode を直列化する filesystem lock を
// 取る。取れなければ (nil, false, nil) を返し、呼び出し元は待たずにジョブを戻す。
func lockEncodeScratch(scratchDir string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(scratchDir), 0o755); err != nil {
		return nil, false, fmt.Errorf("creating scratch root: %w", err)
	}
	lock, err := os.OpenFile(encodeScratchLockPath(scratchDir), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("opening encode scratch lock: %w", err)
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, true, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("locking encode scratch: %w", err)
	}
}

// reserveEncodeScratch は (recording, profile) の scratch を占有して作り、解放関数を
// 返す。
//
// 同じ (recording, profile) の別実行が掴んでいたら待たずにエラーを返す --- 保持側は
// 数十分動きうるので、待つと River の実行枠を捨てるだけである（戻したジョブは冪等
// 判定で短絡するか、保持側の終了後に走る）。解放は RemoveAll の後で lock を離す ---
// 掃除のあいだに次の実行が同じディレクトリを掴むと、消し合う。
func reserveEncodeScratch(scratchDir string, log *slog.Logger) (release func(), err error) {
	lock, acquired, err := lockEncodeScratch(scratchDir)
	if err != nil {
		return nil, fmt.Errorf("locking encode scratch: %w", err)
	}
	if !acquired {
		log.Warn("encode: scratch is held by another attempt of the same (recording, profile)", "dir", scratchDir)
		return nil, fmt.Errorf("encode: scratch %q is in use by another attempt of the same (recording, profile); deferring", scratchDir)
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("creating scratch dir: %w", err)
	}
	return func() {
		// 成功・失敗を問わず scratch を best-effort で掃除（途中成果物は cleanup が
		// 回収する想定だが、正常系では残さない）。
		if rmErr := os.RemoveAll(scratchDir); rmErr != nil {
			log.Warn("encode: scratch cleanup failed", "dir", scratchDir, "err", rmErr)
		}
		_ = lock.Close()
	}, nil
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

// commitEncoded は置いた canonical file を media_assets の行として公開する。
// cut が非 nil（カット版）なら、**行の差し替えと凍結した区間の差し替えを 1 つの tx
// で行う** --- 別々に commit すると、間に落ちたときに「新しいパスを指す行 + 古い
// 区間」という、実在しないファイルの説明が残る。
//
// 行は消さずに UpsertEncodedMediaAsset で rel_path / size_bytes を書き換える
// （消して作り直すと、rel_path の部分一意索引から一瞬外れてその隙間に別の行が
// 同じパスを取れる）。カット版は世代番号で必ず新しいパスになるので衝突しない。
//
// **transaction-level advisory lock を ingest commit と同じキーで取る**。同じ
// rel_path の公開と孤児回収の unlink を DB 側でも直列化するためで、呼び出し元が
// 保持する filesystem lock が効かない構成（RWX 越しの flock は未検証）でも、
// cleanup が公開直後の canonical を消せない。
//
// 旧パスの unlink は**呼び出し元が commit の後で**行う（ここで消すと、commit が
// 失敗したときに生きている行が指すファイルを失う）。
func (w *EncodeWorker) commitEncoded(ctx context.Context, recordingID int64, profile, relPath string, size int64, cut *cutContext) error {
	var ranges pgtype.Multirange[pgtype.Range[pgtype.Int8]]
	if cut != nil {
		var err error
		if ranges, err = keepRangesParam(cut.keep); err != nil {
			return err
		}
	}

	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning encoded asset commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMediaRelPathInTransaction(ctx, tx, relPath); err != nil {
		return err
	}
	q := sqlcgen.New(tx)

	profileName := profile
	assetID, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: recordingID,
		Profile:     &profileName,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		return fmt.Errorf("upserting media_asset: %w", err)
	}
	if cut != nil {
		if err := replaceMediaAssetCuts(ctx, q, assetID, ranges); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing encoded asset: %w", err)
	}
	return nil
}

// replaceMediaAssetCuts は 1 つの media_asset の凍結区間を差し替える
// （DELETE → INSERT）。呼び出し側の tx の中で使う。
func replaceMediaAssetCuts(ctx context.Context, q *sqlcgen.Queries, assetID int64, ranges pgtype.Multirange[pgtype.Range[pgtype.Int8]]) error {
	if err := q.DeleteMediaAssetCuts(ctx, assetID); err != nil {
		return fmt.Errorf("clearing media_asset_cuts: %w", err)
	}
	if err := q.InsertMediaAssetCuts(ctx, sqlcgen.InsertMediaAssetCutsParams{
		MediaAssetID: assetID,
		KeepRanges:   ranges,
	}); err != nil {
		return fmt.Errorf("writing media_asset_cuts: %w", err)
	}
	return nil
}

// keepRangesParam は ms の半開区間列を int8multirange の param へ写す。
// 昇順・非交差へ正規化してから渡す（値どうしの一致比較をするため）。
func keepRangesParam(keep []chapters.Range) (pgtype.Multirange[pgtype.Range[pgtype.Int8]], error) {
	out := make(pgtype.Multirange[pgtype.Range[pgtype.Int8]], 0, len(keep))
	for _, r := range keep {
		if r.EndMs <= r.StartMs {
			continue
		}
		out = append(out, pgtype.Range[pgtype.Int8]{
			Lower:     pgtype.Int8{Int64: r.StartMs, Valid: true},
			Upper:     pgtype.Int8{Int64: r.EndMs, Valid: true},
			LowerType: pgtype.Inclusive,
			UpperType: pgtype.Exclusive,
			// Valid を立てないと pgx が「NULL の要素」として符号化を拒否する
			// （multirange cannot contain NULL element）。
			Valid: true,
		})
	}
	if len(out) == 0 {
		// CHECK (NOT isempty(keep_ranges)) が拒否する。先に落とす。
		return nil, errors.New("keep ranges are empty")
	}
	return out, nil
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
//	[-vf <deinterlace[, scaler が決めた scale]>]   # deinterlace=true または height>0 のときだけ、常に 1 個
//	[-crf N | -qp N] [-preset P]
//	[extra_args…]                                  # ユーザー（出力側）
//	-f CONTAINER -progress pipe:1 -loglevel error OUTPUT  # アプリ所有の末尾
//	[-map 0:s? -c:s webvtt -f webvtt SUBTITLE_OUTPUT]   # subtitles=webvtt かつ withSubtitles のとき
//
// **extra_args は -f の前に置く。** 以前は -f の後ろだった（旧位置に依存する
// config は無い前提 --- -f は許可済みオプションに含まれないので、ユーザーが
// 相対順序に依存する余地は無い）。VOD と live で「ユーザーのオプション
// はコーデック/品質/スケール指定の後・アプリ所有の末尾の前」という 1 つの規則に
// するための移動（BuildLiveFFmpegArgs と同じ形にする）。
//
// withSubtitles は起動前の ffprobe 判定結果（呼び出し側が probeHasSubtitles で
// 得る）。profile.Subtitles == "webvtt" と両方 true のときだけ WebVTT サイドカーの
// 出力を追加する --- 字幕ストリームが無い状態で ffmpeg に WebVTT 出力を要求すると
// 終了するため、局や番組によって字幕 PID が無い録画でもエンコード本体を落とさない
// （issue #430 の optional map の罠）。サイドカーの出力パスは output と同じ
// ディレクトリ・basename に .vtt を付けたもの（mediapath.SubtitleSibling）で
// 固定する --- 呼び出し側が任意のパスを選べる余地は無い。
//
// cut は cut プロファイルの filtergraph と -map（cut でなければ nil）。**cut では
// `-vf` を出さない** --- filtergraph はアプリが 1 本だけ組み、deinterlace / scale /
// hwupload を連結の後ろに置く（ffargs.CutFilterComplex）。`-vf` と
// `-filter_complex` の併用は、同じ入力を 2 回フィルタする意図の無い形になる。
// cut の入力側は `-hwaccel` の代わりに `-vaapi_device` を出す（HW デコードした
// フレームは trim に通せない。ffargs.VAAPIDeviceArgs）。
func BuildFFmpegArgs(profile config.EncodeProfile, input, output string, withSubtitles bool, cut *ffargs.CutFilterResult) []string {
	args := []string{
		"-hide_banner",
		"-nostats",
		"-y",
	}
	if profile.Cut {
		args = append(args, ffargs.VAAPIDeviceArgs(profile.HWAccel)...)
		args = append(args, profile.InputExtraArgs...)
	} else {
		args = append(args, ffargs.PreInput(profile.HWAccel, profile.InputExtraArgs)...)
	}
	if profile.Subtitles == "webvtt" && withSubtitles {
		// **ARIB 字幕は duration を持たない。** これが無いと WebVTT の終了時刻が
		// 全 cue で約 1193 時間になり、字幕が一度出たら消えず積み重なる（実測:
		// NHK Eテレの実 TS 30 秒で 11/11 cue が壊れ、付けると 11/11 正常）。
		// -fix_sub_duration は入力側オプションなので -i より前に置く。
		args = append(args, "-fix_sub_duration")
	}
	args = append(args, "-i", input)
	if cut != nil {
		args = append(args, "-filter_complex", cut.FilterComplex, "-map", cut.VideoMap, "-map", cut.AudioMap)
	}
	args = append(args,
		"-c:v", profile.VideoCodec,
		"-c:a", profile.AudioCodec,
	)
	if cut == nil {
		if filter, ok := ffargs.VideoFilterArgs(profile.Scaler, profile.Height, profile.Deinterlace); ok {
			args = append(args, "-vf", filter)
		}
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
	if profile.Subtitles == "webvtt" && withSubtitles {
		// output は container（mp4 / mkv）の拡張子を必ず持つ（config 検証済み）。
		// 万一導出できなければサイドカーの出力を足さない --- 呼び出し側が
		// 同じ規則で導出した subtitleOut を os.Stat して失敗するので黙って
		// 字幕だけ消えることはない。
		if sidecar, err := mediapath.SubtitleSibling(output); err == nil {
			args = append(args,
				"-map", "0:s?",
				"-c:s", "webvtt",
				"-f", "webvtt",
				sidecar,
			)
		}
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
// generation が正なら `_{profile}.g{n}.{container}` になる（cut プロファイルの
// 世代。1 世代目から必ず付く）。置き換えは「新しいパスに置いてから旧パスを消す」
// ので、同じパスへ上書きしてはならない（ストレージ契約の「置くのは一回」。
// 生きている行の rel_path 部分一意索引とも衝突する）。
//
// profile 名はパス成分としてサニタイズする（contentpath）。階層は原本の dir のみ。
func EncodedRelPath(originalRel, profileName, container string, generation int) (string, error) {
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
	suffix := ""
	if generation > 0 {
		suffix = fmt.Sprintf(".g%d", generation)
	}

	// パス区切りは DB 上で '/' 規約。filepath は OS 依存なので ToSlash で揃える。
	originalRel = filepath.ToSlash(originalRel)
	dir := pathDirSlash(originalRel)
	base := pathBaseSlash(originalRel)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		stem = "encoded"
	}
	name := stem + "_" + safeProfile + suffix + "." + container
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
func EnqueueMissingEncodes(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64, cutProfiles map[string]struct{}) error {
	return enqueueMissingEncodes(ctx, inserter, pool, recordingID, nil, cutProfiles)
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
func EnqueueMissingEncodesForKnownProfiles(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64, known []string, cutProfiles map[string]struct{}) error {
	set := make(map[string]struct{}, len(known))
	for _, name := range known {
		set[name] = struct{}{}
	}
	return enqueueMissingEncodes(ctx, inserter, pool, recordingID, set, cutProfiles)
}

// enqueueMissingEncodes は上 2 つの実装本体。known が nil なら desired を絞らない
// （nil と空マップは意味が違う: 空マップは「投入してよいプロファイルが 1 つも
// 無い」）。
//
// cutProfiles は cut: true のプロファイル名（config.EncodeConfig.CutProfileSet）。
// **cut プロファイルは所有（= ユーザーが確認済み）の行がある録画にしか投入しない。**
// 確認前にカット版がコミットされると、誤検出のまま本編が削られ、原本がごみ箱を
// 経由せずに消えて取り返せなくなる。所有していない録画の cut プロファイルは
// 「投入しない」ではなく「まだ投入しない」で、ユーザーが確認した次のパスが拾う
// （api 側はそれを awaiting_review として見せる）。
func enqueueMissingEncodes(ctx context.Context, inserter JobInserter, pool *pgxpool.Pool, recordingID int64, known, cutProfiles map[string]struct{}) error {
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

	// 所有の行の有無は 1 回だけ引く（cut プロファイルが 1 つも無ければ引かない）。
	var currentKeep []chapters.Range
	if len(cutProfiles) > 0 {
		keep, owned, err := currentCutKeep(ctx, q, recordingID)
		if err != nil {
			if errors.Is(err, pgx5.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
		}
		// 未確認（owned=false）も全区間カット（keep 空）も投入しない。後者は
		// loadCutContext が "has no keep ranges" で必ず失敗するので、投入すると
		// reconcile のたびに失敗ジョブが積まれる。
		if owned {
			currentKeep = keep
		}
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
		_, isCut := cutProfiles[name]
		if isCut && len(currentKeep) == 0 {
			continue // 未確認 or 全区間カット。確認するまで / keep が出来るまで投入しない
		}
		assetID, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{
			RecordingID: recordingID,
			Profile:     &name,
		})
		if err == nil {
			if !isCut {
				continue // 既に active encoded あり
			}
			// cut は「active で、かつ凍結した区間が現在の keep と一致する」が
			// 完了。**一致しなければ作り直す** --- チャプターを直した後の
			// 再エンコード（POST …/reencode）がこの経路でジョブになる。
			fresh, err := cutIsCurrent(ctx, q, assetID, currentKeep)
			if err != nil {
				return err
			}
			if fresh {
				continue
			}
		} else if !errors.Is(err, pgx5.ErrNoRows) {
			return fmt.Errorf("checking encoded asset %q: %w", name, err)
		}

		if _, err := inserter.Insert(ctx, jobs.EncodeJobArgs{
			RecordingID: recordingID,
			Profile:     name,
		}, nil); err != nil {
			return fmt.Errorf("inserting encode job profile=%s: %w", name, err)
		}
	}
	return nil
}

// cutIsCurrent は active な cut 版の凍結区間が現在の量子化 keep と一致するかを
// 返す。keep が空（全部カット）なら false（作り直しても作れないので、呼び出し側は
// 投入しない判断に使える）。
func cutIsCurrent(ctx context.Context, q *sqlcgen.Queries, assetID int64, keep []chapters.Range) (bool, error) {
	if len(keep) == 0 {
		return false, nil
	}
	frozen, err := assetKeepRanges(ctx, q, assetID)
	if err != nil {
		return false, err
	}
	return chapters.SameRanges(frozen, keep), nil
}

// enqueueMissingEncodesFromContext は River ワーカーの ctx から client を取り、
// 欠けている encode ジョブを投入する。client が無い（単体で Work を呼んだ）場合は
// 何もしない。失敗はログのみ（ingest 本体の成功を巻き戻さない）。
func enqueueMissingEncodesFromContext(ctx context.Context, pool *pgxpool.Pool, recordingID int64, cutProfiles map[string]struct{}) {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return
	}
	if err := EnqueueMissingEncodes(ctx, client, pool, recordingID, cutProfiles); err != nil {
		slog.Error("encode: failed to enqueue missing encodes",
			"recording_id", recordingID, "err", err)
	}
}

// EncodeEnqueueHintWorker は EncodeEnqueueHintArgs を受けて EnqueueMissingEncodes
// を呼ぶだけの薄いワーカー。ロジックは持たない（EnqueueMissingEncodes にそのまま
// 委譲する。レベルトリガーなので呼び出しが遅れても・重複しても収束する）。
type EncodeEnqueueHintWorker struct {
	river.WorkerDefaults[jobs.EncodeEnqueueHintArgs]
	Pool *pgxpool.Pool

	// CutProfiles は cut: true のプロファイル名（config から注入）。所有して
	// いない録画への投入を止めるのに使う（enqueueMissingEncodes 参照）。
	CutProfiles map[string]struct{}
}

// Work は EnqueueMissingEncodes を呼び、recording_encode_policy.encode_profiles（desired）と
// active encoded media_assets（observed）の差分を埋める encode ジョブを投入する。
//
// river.ClientFromContextSafely でジョブ実行中の Client を取り出す。取れない
// （単体テストで Work を直接呼んだ等）場合はエラーを返して失敗させる ---
// ruler_pass 完了時の reconcile_pass ヒント（ruler_pass.go）とは異なり、この
// ジョブ自体の主目的が「encode ジョブを実際に投入すること」であるため、client が
// 無いからと黙って何もしないとユーザーの事後追加依頼がサイレントに消える。
func (w *EncodeEnqueueHintWorker) Work(ctx context.Context, job *river.Job[jobs.EncodeEnqueueHintArgs]) error {
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		return fmt.Errorf("encode enqueue hint: getting river client: %w", err)
	}
	return EnqueueMissingEncodes(ctx, client, w.Pool, job.Args.RecordingID, w.CutProfiles)
}
