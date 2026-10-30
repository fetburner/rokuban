package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
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

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/webhook"
)

// EncodeWorker は原本 media_asset から構造化プロファイルで派生物を作る。
// 原本は mediapath.Resolve で読み、ffmpeg 出力はローカル scratch に書く。完成した
// 派生物は objstore.Store.Put で永続化し、その後 DB transaction が pending copy を
// active にする。ファイルシステム固有の staging と原子的な公開は fsstore が担う。
// 進捗は ffmpeg -progress pipe:1 から読み、stderr のスクレイピングはしない。
//
// # 並走する encode の世代と公開を守る
//
// job timeout 後、queue adapter が同じ Args を再配送する前に以前の Work が
// 終わる保証はなく、同じ (recording, profile) の encode が 2 本並走しうる。
//
//   - **scratch は recording / profile / domain attempt ごと**に作る。queue job ID や
//     配送回数は使わず、再配送後もドメインキーで追える。各試行は自分の scratch dir に
//     flock を持ち続け、cleanup は lock が取れた（死んだ）dir だけを消す。encode 同士は
//     別 dir なので直列化しない（TestEncodeWorker_ConcurrentScratchCleanupSkipsActiveAttempts）。公開 transaction では
//     `recording_encode_attempts.attempt_count` を読み直し、古い試行を拒否する。代償として、
//     cancel を無視する古い試行が新しい試行とともに ffmpeg を完走することはある。
//   - **派生物の公開はキーを再利用しない**（publishEncoded）。attempt token、desired・要求行・通常のカット版ジョブの章の状態・
//     encoded 行を読み直し、エンコード前の判定（observeEncodePlan）と同じ純関数
//     （store.PlanEncodedPublish）で判定する。次のどれかなら active commit を飛ばす。
//     Put 済みの pending object は配信されず、削除 reconcile が回収する:
//     (a) rel_path が冒頭で観測した値と違う（state は問わない。行なしも値の 1 つ）。
//     encoded 行の rel_path を書くのは UpsertDerivedMediaAsset だけで、行は DELETE
//     されず tombstone で残るので、rel_path は世代順に前へしか進まない。古い計画の
//     実行が行を巻き戻すのをこれが止める。(a) は「誰かが済ませた」ではなく
//     「自分の計画が古い」を意味する。(a) だけが立ち行が active なら（区間が違う
//     カット版のときだけ）成功で飛ばさず、公開せずに jobq の snooze で戻して計画を
//     やり直す。snooze 後はエンコード前の判定からやり直し、ジョブの種類で結果が分かれる。
//     作り直し要求に由来するジョブは、要求行の keep と照合して新しい世代で作り直す
//     （成功で飛ばすと押した作り直しが黙って消える）。要求に由来しないジョブは、
//     active なカット版を置き換えてはならないので active_cut_protected で飛ばす。
//     行が active でない（ごみ箱など）ときは成功で飛ばす。snooze は attempt を消費せず、
//     encode.failed も試行の failed 行も出さない。
//     (b) 行が active で（カット版なら）凍結区間がこの試行の keep と一致する（誰かが
//     既に commit した）。各試行は異なる世代キーへ Put するので、遅い試行が active
//     copy のバイト列を上書きできない。
//     (c) profile が desired（recording_encode_policy.encode_profiles）に無い。
//     ユーザーが外した版を、外す前に積まれたジョブが公開して復活させるのを止める。
//     (d) 作り直し要求に由来するジョブで、要求行が無いか keep が上書き済み。または
//     要求に由来しないカット版ジョブで、章を所有していないか現在の keep と違う。
//     要求に由来しないジョブは active なカット版を置き換えない（置き換えてよいのは
//     利用者が押した作り直しだけ）。
//
// DB の公開・削除 transaction は同じ論理資産の状態を直列化する。Store のキーは
// 使い回さず、DB の active copy だけを streamer に公開する。Put 後に DB commit
// されなかったキーは pending reconcile が回収する。
//
// # site 照合ガード（issue #139）は不要と判断
//
// EncodeJobArgs は recording_id + profile のみで site を持たない。エンコードは
// 原本 media_asset（mediapath.Resolve で解決する単一の MediaDir 配下）を読み、
// 派生物は object store に公開するだけで mirakc には一切触れない（不変条件 4「ffmpeg/ffprobe の exec は
// worker / streamer パッケージのみ」であって mirakc 呼び出しではない）。
// アーカイブは複数サイト構成でも単一（site に従属しない。docs/storage.md）ため、
// 他サイトの worker が拾っても mediapath.Resolve が解決する先は変わらず、
// 「別インスタンスの id を投げる」形の壊れ方が起きない。
type EncodeWorker struct {
	// Media はエンコード計画と原本の読み取り契約。nil は該当経路を呼ばない部分テストでのみ許容する。
	Media     store.MediaReconcile
	Attempts  store.EncodeAttempts
	Publisher store.MediaPublish
	// Objects は派生物の置き場。実装差は objstore に閉じ、公開は ObjectPublisher の pending → active で行う。
	Objects         objstore.Store
	ObjectPublisher store.DerivedMediaPublisher
	Progress        store.EncodeProgressReporter
	MediaDir        string
	MediaStoreName  string
	ScratchDir      string
	FFmpeg          string
	FFprobe         string
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

// encodeMinTimeout は実尺から計算する締切の下限。
const encodeMinTimeout = time.Hour

// Work は encode ジョブを実行する。
//
// 停止による ctx キャンセルと snooze は試行に数えず、締切超過と通常の失敗は
// recording_encode_attempts の回数に反映する。ドメイン上限に達したら
// jobq.ErrDiscard を返して、adapter に再配信不要と伝える。
// POST /api/recordings/{id}/encode-profiles と cut 版の /reencode は failed 行を消して予算を戻す。
func (w *EncodeWorker) Work(ctx context.Context, args jobs.EncodeJobArgs) error {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "starting encode"); err != nil {
		return err
	}
	start, err := w.beginEncodeAttempt(ctx, args.RecordingID, args.Profile)
	if err != nil {
		return err
	}
	if start.DeadAttempt > 0 {
		w.notifyEncodeFailure(ctx, args, start.DeadAttempt)
	}
	if start.Terminal {
		return errors.Join(jobq.ErrDiscard, errEncodeAttemptLimit)
	}

	err = w.runEncode(ctx, args, start.Count)
	if err == nil {
		if clearErr := w.clearEncodeAttempt(ctx, args.RecordingID, args.Profile, start.Count); clearErr != nil {
			logEncodeAttemptWriteFailure("clear success", args.RecordingID, args.Profile, clearErr)
		}
		return nil
	}
	if shouldCountEncodeFailure(err, ctx.Err()) {
		updated, updateErr := w.markEncodeAttemptFailed(ctx, args.RecordingID, args.Profile, start.Count, err)
		if updateErr != nil {
			return errors.Join(err, updateErr)
		}
		if updated {
			w.notifyEncodeFailure(ctx, args, start.Count)
			if start.Count >= encodeAttemptLimit {
				return errors.Join(jobq.ErrDiscard, errEncodeAttemptLimit, err)
			}
		}
		return err
	}
	if restoreErr := w.restoreEncodeAttempt(ctx, args.RecordingID, args.Profile, start); restoreErr != nil {
		return errors.Join(err, restoreErr)
	}
	return err
}

// runEncode は encode ジョブの本体。
func (w *EncodeWorker) runEncode(ctx context.Context, args jobs.EncodeJobArgs, attempt int32) (err error) {
	log := slog.With("recording_id", args.RecordingID, "profile", args.Profile)

	started := time.Now()
	result := "failure"
	defer func() {
		metrics.EncodeDuration.Observe(time.Since(started).Seconds())
		metrics.EncodeJobs.WithLabelValues(result).Inc()
	}()

	// 冪等: カット版でないプロファイルは、active な encoded があれば飛ばす。
	// カット版は、active な版の凍結区間がこのジョブの keep と一致するときだけ飛ばす。
	// 区間が違う active なカット版を置き換えてよいのは、作り直し要求に由来するジョブだけである。
	// 要求に由来しないジョブ（不足の補完）は、利用者が押していないので置き換えず、飛ばす。
	observed, cut, expectedCutKeep, skip, err := w.observeEncodePlan(ctx, args, log)
	if err != nil {
		return err
	}
	if skip {
		result = "success"
		return nil
	}

	profile, originalRelPath, inputPath, duration, err := w.prepareEncodeInput(ctx, args, log)
	if err != nil {
		return err
	}
	// reporter.start は prepareEncodeInput ではなく、ここで呼ぶ（理由は prepareEncodeInput の doc コメント参照）。
	var reportProgress func(time.Duration)
	if duration > 0 {
		reporter := encodeProgressReporter{
			recordingID: args.RecordingID,
			profile:     args.Profile,
			duration:    duration,
			interval:    encodeProgressInterval,
			reporter:    w.Progress,
			log:         log,
		}
		var stopProgress context.CancelFunc
		reportProgress, stopProgress = reporter.start(ctx)
		defer stopProgress()
	}

	// 置き換えでは新しい世代のパスに置いてから旧パスを消す（世代と旧パスは
	// planEncodePublish が冒頭の観測から導く）。
	generation := observed.generation
	relPath, err := EncodedRelPath(originalRelPath, profile.Name, profile.Container, int(generation), cut != nil)
	if err != nil {
		return fmt.Errorf("building encoded rel_path: %w", err)
	}
	profileName := profile.Name
	if w.ObjectPublisher == nil {
		return errors.New("derived media publisher is required")
	}
	if err := w.ObjectPublisher.PreparePendingMediaAssetCopy(ctx, store.PendingMediaAssetCopy{
		RecordingID: args.RecordingID, Kind: store.AssetKindEncoded, Profile: &profileName,
		RelPath: relPath, DerivedGeneration: generation, ReserveNewGeneration: true,
	}); err != nil {
		if errors.Is(err, store.ErrGenerationAdvanced) {
			return jobq.Snooze(encodeReplanDelay)
		}
		return fmt.Errorf("reserving encoded object generation: %w", err)
	}

	// プロセス死の残骸を回収し、domain attempt ごとに scratch を分ける。
	// cleanup は同じ (recording_id, profile) の、dir lock を誰も持たない dir だけを消す。
	removeStaleEncodeScratch(w.ScratchDir, args.RecordingID, args.Profile, log)
	scratchDir, scratchLock, err := newEncodeScratchDir(w.ScratchDir, args.RecordingID, args.Profile, attempt)
	if err != nil {
		return fmt.Errorf("creating encode scratch directory: %w", err)
	}
	scratchOut := filepath.Join(scratchDir, "out."+profile.Container)
	defer func() {
		// 成功・失敗を問わず scratch を best-effort で掃除（途中成果物は cleanup が
		// 回収する想定だが、正常系では残さない）。
		if rmErr := os.RemoveAll(scratchDir); rmErr != nil {
			log.Warn("encode: scratch cleanup failed", "dir", scratchDir, "err", rmErr)
		}
		if lockErr := closeEncodeScratchLock(scratchLock); lockErr != nil {
			log.Warn("encode: scratch lock release failed", "err", lockErr)
		}
	}()

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
	size, published, err := w.publishEncoded(ctx, encodePublishInput{
		attempt:               attempt,
		observed:              observed,
		recordingID:           args.RecordingID,
		profile:               profile.Name,
		relPath:               relPath,
		scratchOut:            scratchOut,
		subtitleOut:           subtitleOut,
		withSubtitles:         withSubtitles,
		pendingObjectPrepared: true,
		cut:                   cut,
		expectedKeep:          expectedCutKeep,
	})
	if err != nil {
		if _, snoozed := jobq.SnoozeDelay(err); snoozed {
			// 計画が古い。成功で skip すると新しい chapters の編集が消えるので、
			// snooze のまま返して現在の keep から計画をやり直させる（attempt は消費しない）。
			log.Info("encode: publish plan is stale, snoozing to replan")
			result = "replan"
		}
		return err
	}
	if !published {
		// 別の実行が先に公開した（または行が先へ進んだ）か、profile が desired から
		// 外れた。自分の temp は消してあり、旧ファイルの unlink も完了通知も出さない。
		log.Info("encode: skipped publishing, another attempt already advanced the row or the profile is no longer desired")
		result = "success"
		return nil
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

// observeEncodePlan は、ffmpeg を起動する前に、公開 tx と同じ判定を読み取りだけで当てる。
// 必要な DB 状態はロックを取らない 1 つのスナップショットで読む。作り直し要求の行は
// 消さない（満たされたかは unsatisfied_encode_rebuilds が導出する）。
//
// この結果は古くなりうるが、エンコードが無駄になるか公開 tx に弾かれるかのどちらかにしかならない。
// 公開 tx は同じ判定をロック下で評価し直す。
func (w *EncodeWorker) observeEncodePlan(
	ctx context.Context,
	args jobs.EncodeJobArgs,
	log *slog.Logger,
) (observed encodePlan, cut *cutContext, expectedKeep []chapters.Range, skip bool, err error) {
	expectedKeep, hasExpectedKeep, err := decodeRequestedCutKeep(args.RequestedCutKeep)
	if err != nil {
		return encodePlan{}, nil, nil, false, fmt.Errorf("decoding requested cut keep for recording %d profile %q: %w", args.RecordingID, args.Profile, err)
	}

	profile, profileKnown := w.Profiles.Profile(args.Profile)
	if !profileKnown && !hasExpectedKeep {
		return encodePlan{}, nil, nil, false, fmt.Errorf("unknown encode profile %q", args.Profile)
	}
	profileIsCut := profileKnown && profile.Cut
	// 作り直し要求に由来するのにカット版でないプロファイルは、ジョブの仕様と描画器の設定の
	// 食い違い（ローリング更新の間に起きうる）である。DB を読まずに飛ばす。
	if hasExpectedKeep && !profileIsCut {
		log.Info("encode: skipped before encoding", "reason", "rebuild_request_on_non_cut_profile")
		return encodePlan{}, nil, expectedKeep, true, nil
	}
	state, err := w.Media.GetEncodedPublishState(ctx, args.RecordingID, args.Profile, profileIsCut, profileIsCut && !hasExpectedKeep)
	if err != nil {
		return encodePlan{}, nil, nil, false, fmt.Errorf("checking encoded publish state: %w", err)
	}

	input := store.EncodedPublishInput{
		RecordingID:       args.RecordingID,
		Profile:           args.Profile,
		Cut:               profileIsCut,
		HasRebuildRequest: hasExpectedKeep,
		RequestedKeep:     expectedKeep,
	}
	if hasExpectedKeep {
		input.KeepRanges = expectedKeep
	} else if profileIsCut {
		// **所有の行があることを前提にする。** 確認前にカット版をコミットすると、誤検出の
		// まま本編が削られ、原本がごみ箱を経由せずに消えて取り返せなくなる。要求に由来しない
		// カット版ジョブを投入するのは、encode reconcile が ListMissingEncodeProfiles の
		// 不足側の行から作るものだけで、そこは所有していない録画を除外する。ここに来る
		// ジョブは所有済みのはずなので、来なければジョブの失敗として現れる（黙って切らない）。
		// desired から外れていれば状態を読んでいないので、下の判定が飛ばす。
		if state.Desired {
			if !state.ChapterOwned {
				return encodePlan{}, nil, nil, false, fmt.Errorf("cut profile %q requires adopted chapters for recording %d", args.Profile, args.RecordingID)
			}
			// keep が空（全部カット）も失敗させる。keep_ranges の CHECK が空を拒否するので、
			// 先に落とさないと tx ごとロールバックする。
			if len(state.CurrentCutKeep) == 0 {
				return encodePlan{}, nil, nil, false, fmt.Errorf("recording %d has no keep ranges (every span is cut)", args.RecordingID)
			}
		}
		input.KeepRanges = state.CurrentCutKeep
	}

	observed = planEncodePublishFromState(input, state)
	if observed.skip {
		log.Info("encode: skipped before encoding", "reason", string(observed.skipReason))
		return observed, nil, expectedKeep, true, nil
	}
	if state.MaxCopyGeneration != nil && *state.MaxCopyGeneration == int64(^uint64(0)>>1) {
		return encodePlan{}, nil, nil, false, fmt.Errorf("encoded generation exhausted for recording %d profile %q", args.RecordingID, args.Profile)
	}
	if profileIsCut {
		cut = &cutContext{keep: input.KeepRanges}
	}
	return observed, cut, expectedKeep, false, nil
}

// cutContext は cut プロファイルの encode に要る、録画側の事実から導出した
// keep 区間。cut でないプロファイルでは nil。
type cutContext struct {
	keep []chapters.Range
}

func decodeRequestedCutKeep(raw json.RawMessage) ([]chapters.Range, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var keep []chapters.Range
	if err := json.Unmarshal(raw, &keep); err != nil {
		return nil, false, err
	}
	if len(keep) == 0 {
		return nil, false, errors.New("requested keep ranges are empty")
	}
	return keep, true, nil
}

// buildCutFilter は入力のストリームを選び、keep 区間で切る filtergraph を組む。
func (w *EncodeWorker) buildCutFilter(ctx context.Context, profile config.EncodeProfile, inputPath string, keep []chapters.Range) (*ffargs.CutFilterResult, error) {
	video, audio, err := w.selectStreams(ctx, inputPath)
	if err != nil {
		return nil, err
	}
	hwUpload := profile.HWAccel != nil && profile.HWAccel.Kind == "vaapi" && profile.HWAccel.OutputFormat == ""
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
	ffprobe := ffargs.FFprobePath(w.FFprobe)
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
//
// ffmpeg の stdout は、io.PipeWriter を Cmd.Stdout に渡して os/exec にコピーさせる。
// これで Wait はコピーの完了まで戻らない。writer は Wait の後に閉じて parser に EOF を
// 送るので、終了直前の進捗行も取りこぼさない
// （TestEncodeWorker_RunEncodeCommand_ReadsFinalProgressBeforeReturning）。StdoutPipe では
// 読み終わる前に Wait を呼ぶことになり、os/exec の契約外になる。Wait が pipe を閉じるので
// 末尾の行を落とす。
//
// <-progressDone を Wait の前に置いてはならない。孫プロセスが stdout を握ると EOF が来ず、
// WaitDelay で待ちを切る唯一の場所である Wait に届かないまま塞がる
// （TestEncodeWorker_WaitDelayExpiredOnSuccess_TreatedAsSuccess）。
//
// os/exec は「WaitDelay に関わらず、Wait は Stdout への Write の完了まで塞がりうる」と
// 定めている。io.PipeWriter は読み手がいなければ塞がるので、parser が早期終了しても
// 残りを読み捨て、writer が閉じるまで読み続ける
// （TestEncodeWorker_RunEncodeCommand_DiscardsProgressAfterLongLine）。reportProgress も
// 塞がない前提である。塞ぐと WaitDelay でも ctx のキャンセルでも Wait は戻らない
// （上の契約からの帰結で、未測定）。
func (w *EncodeWorker) runEncodeCommand(ctx context.Context, in encodeCommandInput, reportProgress func(time.Duration), log *slog.Logger) error {
	profile, inputPath, scratchOut, subtitleOut, withSubtitles := in.profile, in.inputPath, in.scratchOut, in.subtitleOut, in.withSubtitles
	ffmpeg := ffargs.FFmpegPath(w.FFmpeg)
	cmd := exec.CommandContext(ctx, ffmpeg, BuildFFmpegArgs(profile, inputPath, scratchOut, withSubtitles, in.filter)...)
	setWorkerExecWaitDelay(cmd)
	// 進捗は stdout（-progress pipe:1）。stderr はエラー診断のみ（進捗に使わない）。
	stdout, stdoutWriter := io.Pipe()
	cmd.Stdout = stdoutWriter
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
		// parser が早期終了しても読み続ける（塞がった Write が Wait を止めないように）。
		_, _ = io.Copy(io.Discard, stdout)
	}()
	waitErr := cmd.Wait()
	_ = stdoutWriter.Close()
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
// コンテキストキャンセルは queue worker の停止やタイムアウト。
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

// encodeReplanDelay は、publishEncoded が tx 内の判定で計画が古いと分かったとき
// （pending object は配信されず、DB reconcile が回収する）に返す snooze の待ち。
const encodeReplanDelay = 10 * time.Second

// beforeEncodeObjectActivation / beforeEncodeCommit はテストが公開手順の途中で実行を止め、
// あるいは commit の失敗を注入するためのフックである（本番では何もしない）。
// 引数は試行の scratch 出力パス（どの試行かをテストが見分けるため）。
var (
	// beforeEncodeObjectPut は hash 後、派生物を置き場へ送る直前のマウント目印検査前に呼ぶ。
	// テストでその間に目印が変わる状況を再現する。
	beforeEncodeObjectPut = func(string) {}
	// beforeEncodeObjectActivation は DB の fencing と計画判定の後、active commit の前に呼ぶ。
	beforeEncodeObjectActivation = func(string) error { return nil }
	// beforeEncodeCommit は active row を更新する tx.Commit の直前に呼ぶ。
	// エラーを返すと commit せずに失敗させる。
	beforeEncodeCommit = func(scratchOut string) error { return nil }
)

// encodePublishInput は publishEncoded の入力。
type encodePublishInput struct {
	recordingID           int64
	profile               string
	attempt               int32
	relPath               string
	scratchOut            string
	subtitleOut           string
	withSubtitles         bool
	pendingObjectPrepared bool
	// observed は runEncode 冒頭の観測（tx 内の判定 (a) の比較元）。
	observed encodePlan
	// cut はカット版のときだけ非 nil。commit で凍結した keep 区間を同じ tx で
	// 差し替える。
	cut *cutContext
	// expectedKeep は作り直し要求に由来するジョブの、押した時点の keep（nil なら
	// 要求に由来しないジョブ）。
	expectedKeep []chapters.Range
}

// publishEncoded は検証済みの scratch 出力を派生物の置き場へ書き、media_assets を
// commit する。pending copy を予約し、置き場の Store.Put が完成・永続化した後に
// pending を active にする。ファイルシステム固有の staging、rename、親 directory
// fsync、上書き防止は fsstore.Put が担う。
func (w *EncodeWorker) publishEncoded(ctx context.Context, in encodePublishInput) (size int64, published bool, err error) {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "publishing encoded object"); err != nil {
		return 0, false, err
	}
	return w.publishEncodedObject(ctx, in)
}

// publishEncodedObject は encoded を派生物の置き場へ公開する。pending は ffmpeg の前に予約済みで、
// ここでは Put してから、公開の tx で pending を active にし SHA-256 を論理行へ書く。
func (w *EncodeWorker) publishEncodedObject(ctx context.Context, in encodePublishInput) (size int64, published bool, err error) {
	info, err := os.Stat(in.scratchOut)
	if err != nil {
		return 0, false, fmt.Errorf("stating encoded scratch output: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, false, fmt.Errorf("encoded scratch output is not a non-empty regular file")
	}
	cut := in.cut != nil
	keep := []chapters.Range(nil)
	if cut {
		keep = in.cut.keep
	}
	input := store.EncodedPublishInput{
		RecordingID: in.recordingID, Profile: in.profile, Attempt: in.attempt,
		RelPath: in.relPath, SizeBytes: info.Size(), DerivedGeneration: in.observed.generation,
		ObservedGeneration: in.observed.observedGeneration, Cut: cut, KeepRanges: keep,
		HasRebuildRequest: in.expectedKeep != nil, RequestedKeep: in.expectedKeep,
		PendingCopy: true,
	}
	pending := store.PendingMediaAssetCopy{
		RecordingID: in.recordingID, Kind: store.AssetKindEncoded, Profile: &in.profile,
		RelPath: in.relPath, SizeBytes: info.Size(), DerivedGeneration: in.observed.generation,
		ReserveNewGeneration: !in.pendingObjectPrepared,
	}
	var companions []objectCompanion
	if in.withSubtitles {
		subtitleKey, err := mediapath.SubtitleSibling(in.relPath)
		if err != nil {
			return 0, false, fmt.Errorf("building subtitle object key: %w", err)
		}
		companions = append(companions, objectCompanion{Key: subtitleKey, Path: in.subtitleOut, ContentType: objectContentType(subtitleKey)})
	}
	size, published, err = publishGeneratedObject(
		ctx, w.Objects, w.ObjectPublisher, pending, in.scratchOut, objectContentType(in.relPath),
		in.pendingObjectPrepared,
		func(operation string) error {
			beforeEncodeObjectPut(in.scratchOut)
			return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, operation)
		},
		func(size int64, sum [sha256.Size]byte) (bool, error) {
			input.SizeBytes = size
			input.SHA256 = sum[:]
			return w.ObjectPublisher.PublishEncoded(ctx, input, func() error {
				if err := beforeEncodeObjectActivation(in.scratchOut); err != nil {
					return fmt.Errorf("preparing encoded object publication: %w", err)
				}
				return beforeEncodeCommit(in.scratchOut)
			})
		}, companions...,
	)
	if errors.Is(err, store.ErrStalePublishPlan) {
		return 0, false, jobq.Snooze(encodeReplanDelay)
	}
	if err != nil {
		return 0, false, err
	}
	return size, published, nil
}

// notify は ev に録画のスナップショット（site / title）を足して webhook を送る。
// 失敗はログのみ（本処理を止めない。M3-11）。
func (w *EncodeWorker) notify(ctx context.Context, ev webhook.Event) {
	if w.Webhook == nil {
		return
	}
	rec, err := w.Media.GetRecordingNotifySubject(ctx, ev.RecordingID)
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

// attemptWriteContext は試行状態の確定に使う、job の ctx から切り離した
// （ただし無期限には待たない）ctx を返す。DeadlineExceeded 後にも失敗を記録し、
// Canceled / snooze では直前の状態へ戻すために使う。
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

// encodePlan は encoded 行の 1 回の観測から導いた、公開の判断材料。
type encodePlan struct {
	// observedGeneration は行の derived_generation（active かは問わない）。nil は行が無いことを表す。
	observedGeneration *int64
	// generation はこの試行が使う派生世代。初回の cut は 1、他の初回出力は 0。
	generation int64
	// skip は公開を飛ばして成功扱いにする判定。
	skip bool
	// skipReason は飛ばす理由。skip でないときは空。
	skipReason store.EncodedPublishSkipReason
	// stale は「自分の計画が古い」ことを表す: 行が active のまま derived_generation だけが
	// 観測時から進み、区間が自分の keep と違う ((a) だけが立つ。cut でだけ起きる)。
	// 成功扱いで飛ばさず、公開せずに計画をやり直す（結果はジョブの種類で分かれる。冒頭の (a)）。
	stale bool
}

// planEncodePublishFromState は store.PlanEncodedPublish の判定に、カット版の世代と置き換え先を足す。
func planEncodePublishFromState(
	in store.EncodedPublishInput,
	state store.EncodedPublishState,
) encodePlan {
	plan := store.PlanEncodedPublish(in, state, false)
	p := encodePlan{
		observedGeneration: plan.ObservedGeneration,
		skip:               plan.Skip,
		skipReason:         plan.SkipReason,
		stale:              plan.Stale,
	}
	if state.EncodedAsset != nil {
		p.generation = state.EncodedAsset.DerivedGeneration
		if plan.Replaces || !state.EncodedAsset.Active {
			p.generation++
		}
	} else if in.Cut {
		p.generation = 1
	}
	// 公開前に止まった試行が予約した世代と、回収済みの世代は使わない。
	// 同じ key を別の出力で再利用しないため、全ての置き場でコピー行を先に予約する。
	if !p.skip && state.MaxCopyGeneration != nil && *state.MaxCopyGeneration >= p.generation &&
		*state.MaxCopyGeneration < int64(^uint64(0)>>1) {
		p.generation = *state.MaxCopyGeneration + 1
	}
	return p
}

func (w *EncodeWorker) loadOriginal(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := w.Media.GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return row, fmt.Errorf("no active original media_asset for recording %d: %w", recordingID, err)
		}
		return row, fmt.Errorf("loading original media_asset: %w", err)
	}
	return row, nil
}

func probeHasSubtitles(ctx context.Context, ffprobe, input string, run func(context.Context, string, ...string) ([]byte, error)) (bool, error) {
	out, err := run(ctx, ffargs.FFprobePath(ffprobe), ffargs.SubtitleProbeArgs([]string{input}, "", "")...)
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
// cut で hwaccel.output_format があれば通常の `-hwaccel` 前置ブロックを出す。
// 省略時は CPU decode の救済経路なので `-vaapi_device` を使う
// （ffargs.VAAPIDeviceArgs）。
func BuildFFmpegArgs(profile config.EncodeProfile, input, output string, withSubtitles bool, cut *ffargs.CutFilterResult) []string {
	args := []string{
		"-hide_banner",
		"-nostats",
		"-y",
	}
	if profile.Cut {
		if profile.HWAccel != nil && profile.HWAccel.OutputFormat != "" {
			args = append(args, ffargs.PreInput(profile.HWAccel, profile.InputExtraArgs)...)
		} else {
			args = append(args, ffargs.VAAPIDeviceArgs(profile.HWAccel)...)
			args = append(args, profile.InputExtraArgs...)
		}
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
// generation が正なら cut は `_{profile}.g{n}.{container}`、uncut は
// `_{profile}.u{n}.{container}` になる。異なる印は、rescue 後に media_asset_cuts が
// 無くても cut の意味を保つ。世代の採番自体は DB の derived_generation から行い、
// cut の初回は 1、uncut の初回は 0。置き換えと tombstone からの再生成は DB の世代を進める。
// 置き換えは「新しいパスに置いてから旧パスを消す」
// ので、同じパスへ上書きしてはならない（ストレージ契約の「置くのは一回」。
// 生きている行の rel_path 部分一意索引とも衝突する）。
//
// profile 名は、そのまま 1 つのパス成分として入れる。入らない名前は、設定と同じ規則
// （config.ValidateEncodeProfileName）でエラーにする。サニタイズすると別の名前と同じ
// パスになりうる。
// 階層は原本の dir のみ。
func EncodedRelPath(originalRel, profileName, container string, generation int, cut bool) (string, error) {
	if originalRel == "" {
		return "", fmt.Errorf("empty original rel_path")
	}
	if container != "mp4" && container != "mkv" {
		return "", fmt.Errorf("unsupported container %q", container)
	}
	if err := config.ValidateEncodeProfileName(profileName); err != nil {
		return "", fmt.Errorf("encode profile %w", err)
	}
	suffix := ""
	if generation > 0 {
		marker := "u"
		if cut {
			marker = "g"
		}
		suffix = fmt.Sprintf(".%s%d", marker, generation)
	}

	// パス区切りは DB 上で '/' 規約。filepath は OS 依存なので ToSlash で揃える。
	originalRel = filepath.ToSlash(originalRel)
	dir := pathDirSlash(originalRel)
	base := pathBaseSlash(originalRel)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if stem == "" {
		stem = "encoded"
	}
	name := stem + "_" + profileName + suffix + "." + container
	if dir == "" || dir == "." {
		return contentpath.SanitizeContentPath(name), nil
	}
	return contentpath.SanitizeContentPath(dir + "/" + name), nil
}

// cutGeneration は rel_path が指定 profile の cut 世代形式かを返す。
// 数値を次の世代の採番には使わず、cut の過去の意味を rescue 後も読むためだけに使う。
func cutGeneration(relPath, profileName string) (int, bool) {
	base := pathBaseSlash(filepath.ToSlash(relPath))
	head, n, ok := config.SplitCutGeneration(strings.TrimSuffix(base, filepath.Ext(base)))
	if !ok || !strings.HasSuffix(head, "_"+profileName) {
		return 0, false
	}
	return n, true
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
	if err := sc.Err(); err != nil {
		log.Warn("encode: progress parser stopped", "err", err)
	}
}

func newEncodeJobArgs(ctx context.Context, media store.MediaReconcile, recordingID int64, profile string, profiles config.EncodeConfig) (jobs.EncodeJobArgs, error) {
	times, err := media.GetRecordingEncodeTimes(ctx, recordingID)
	if err != nil {
		return jobs.EncodeJobArgs{}, fmt.Errorf("loading recording times: %w", err)
	}
	// 実尺は started_at / ended_at を優先し、取れなければ番組長で代用する
	// （ライブラリ取り込みは時刻が NULL、rescue 再スキャンは started_at == ended_at）。
	var duration time.Duration
	if times.StartedAt != nil && times.EndedAt != nil {
		duration = times.EndedAt.Sub(*times.StartedAt)
	}
	if duration <= 0 {
		duration = time.Duration(times.ProgramDurationMs) * time.Millisecond
	}
	rate := config.DefaultEncodeProfileRate
	if p, ok := profiles.Profile(profile); ok && p.Rate > 0 {
		rate = p.Rate
	}
	return jobs.EncodeJobArgs{
		RecordingID: recordingID,
		Profile:     profile,
		Timeout:     encodeJobTimeout(duration, rate),
	}, nil
}

// encodeJobTimeout は max(1h, 実尺 × rate)。実尺が不明（0 以下）なら Spec の
// 未知尺用締切を返す。
func encodeJobTimeout(recordingDuration time.Duration, rate float64) time.Duration {
	if recordingDuration <= 0 {
		return jobs.EncodeUnknownDurationTimeout
	}
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		rate = config.DefaultEncodeProfileRate
	}
	calculated := float64(recordingDuration) * rate
	if calculated >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return max(encodeMinTimeout, time.Duration(math.Ceil(calculated)))
}
