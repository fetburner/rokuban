package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/tsscan"
)

// errIngestRecordEndedAbnormally は、追従中の record が finished ではなく
// canceled / failed で終わったことを表す sentinel（RecordFollowReader の status hook が wrap し、
// Work が errors.Is で拾う）。
//
// **これを queue の再試行に戻してはならない。** cancel / fail は中身が不採用と
// 確定した終端なので、Work は temp を破棄して再試行を止める。Spec の MaxAttempts
// は既定のまま（25）で、エッジの record は commit 成功時にしか消えないため、
// 取り消した長時間録画が全量再取得を繰り返す形にしてはいけない。
//
// 部分ファイルは資産として commit せず、エッジの record も消さない（purge すると
// content path を共有する後継録画のファイルまで消える）。理由は
// docs/recording/ingest-retry.md §5.3 層 3。
var errIngestRecordEndedAbnormally = errors.New("mirakc record ended abnormally")

// errIngestSHA256Pending は、転送済みの temp を残して SHA-256 の再確認を snooze する内部制御エラー。
var errIngestSHA256Pending = errors.New("ingest content SHA-256 is pending")

// errIngestSliceElapsed は temp と SHA-256 の checkpoint を保存した試行の区切りを表す。
var errIngestSliceElapsed = errors.New("ingest transfer slice elapsed")

const (
	ingestSHA256SnoozeInterval = 30 * time.Second
)

// ingestTransferSlice は 1 回の Work で replay と転送に使う時間の上限。
// 4 分の slice と 1 分の後処理予算で Work Timeout を 5 分にし、6 分の rescue 既定より
// 短くする。--once では snooze ごとに Pod が入れ替わるため、運用クラスタで起動時間が
// slice より十分短いことを確認する。
var ingestTransferSlice = 4 * time.Minute

// ingestSHA256BytesPerSecond は finished 後に mirakc が content.sha256 を非同期計算する
// 速度（バイト/秒）の見積もり。待ちの上限は時間ではなくこの速度で固定する ---
// 計算量は録画サイズに比例するので、時間の固定値では大きい録画ほどほぼ必ず
// timeout_skipped になる。報告例の実測は 2.35 GB を約 4 分（約 9.8 MB/s）で、
// 25% の余裕を含めて 7.8 MB/s とする。計測点が 1 件のため設定キーにはしない。
// 地デジ 30 分（約 3.8 GB）で約 8 分、BS 2 時間（約 20 GB）で約 43 分になる
// （この除算の結果であり、実機での待ち時間の測定ではない）。
var ingestSHA256BytesPerSecond int64 = 7_800_000

// ingestFollowPace は録画追従（mirakc.RecordFollowOptions）の待ち時間の上書き。
// 本番では零値（mirakc 側の既定を使う）。実時間のポーリング間隔や再試行バックオフを
// 払いたくないテストだけが setFastIngestFollow で差し替える。
var ingestFollowPace struct {
	pollMin, pollMax time.Duration
	retryDelay       func(attempt int) time.Duration
}

// ingestSHA256MinWait は小さい録画でも mirakc がハッシュ計算を始める猶予として待つ下限。
const ingestSHA256MinWait = 10 * time.Second

// ingestSHA256WaitFor は転送済みバイト数 size に対する SHA-256 待ちの上限を返す。
func ingestSHA256WaitFor(size int64) time.Duration {
	if size < 0 {
		size = 0
	}
	seconds := (size + ingestSHA256BytesPerSecond - 1) / ingestSHA256BytesPerSecond
	return max(time.Duration(seconds)*time.Second, ingestSHA256MinWait)
}

// ingestSHA256WaitDeadline は recording.endTime が有効ならその値を、そうでなければ
// temp の mtime を起点に、size に応じた SHA-256 待ち期限を返す。
func ingestSHA256WaitDeadline(size int64, endTime *mirakc.Milliseconds, tempModTime, now time.Time) time.Time {
	startedAt := tempModTime
	if endTime != nil && !endTime.Time().IsZero() && !endTime.Time().After(now) {
		startedAt = endTime.Time()
	}
	return startedAt.Add(ingestSHA256WaitFor(size))
}

// openIngestFile は flock 済みの temp fd を dup して fsink.File として返す。
// 同じ record の別試行は既存のファイルを共有して続きから書くので、元 fd も複製
// fd も O_APPEND で開かれている。dup は同じ inode / open file description を
// 共有するため、writer の Close で flock を先に解放せず、tempLock が commit
// 終了まで排他を保持できる。path はテストのログやエラーメッセージに使えるよう
// 引数に残しているが、ここで別のパスを開いてはならない。
var openIngestFile = func(_ string, locked *os.File) (fsink.File, error) {
	return fsink.OpenFileFromLock(locked)
}

// lockIngestTempFile は temp を作成し、同時に 1 つの ingest だけが掴めるように
// 排他 flock を取る。戻り値の bool はこの呼び出しで temp を新規作成したかを示す。
// LOCK_NB にするのは、同じ record の別配送が live な転送を壊さず queue の再試行へ
// 戻るためである。
//
// 返した fd は writer の元 fd として使い、openIngestFile はこれを dup する。
// lock fd と writer fd を別々に path open すると、ロック取得後の unlink / rename
// と writer の open の順序次第で、ロックした古い inode と別名の新しい inode に
// 同時追記できるためである。
//
// open 直後に別プロセスが同名 temp を unlink / rename する競合もある。flock を
// 取得した後で fd とパスの inode が同じかを確認し、見えない inode を返さない。
func lockIngestTempFile(path string) (*os.File, bool, error) {
	lock, created, err := fsink.LockTempFile(path)
	if err != nil || !created {
		return lock, created, err
	}
	if err := afterIngestTempCreate(); err != nil {
		_ = lock.Close()
		return nil, false, fmt.Errorf("after creating ingest temporary file %q: %w", path, err)
	}
	return lock, created, nil
}

// lockExistingIngestTempFile は既存 temp だけをロックする。orphan 回収が
// O_CREATE で新しい空ファイルを作ると、別の ingest が同名 temp を作る直前の
// 瞬間に回収側が作った空ファイルを「新しい temp」と誤認してしまうため、回収側
// ではこちらを使う。
func lockExistingIngestTempFile(path string) (*os.File, error) {
	return fsink.LockExistingTempFile(path)
}

func ingestTempLockBusy(err error) bool {
	return fsink.TempLockBusy(err)
}

// ingestTempFilePath は同じ site / record_id の再配送が同じ temp を開くための
// パスを返す。record_id は通常 mirakc の hex ID だが、念のためファイル名に使えない
// 区切りを含む値が media_dir の外へ出ないようにする。site も map から直接渡るため
// 同じ防御を適用する。
func ingestTempFilePath(dir, site, recordID string) string {
	escape := strings.NewReplacer(
		"%", "%25",
		"/", "%2F",
		"\\", "%5C",
	)
	return filepath.Join(dir, mediapath.IngestTempFilePrefix+escape.Replace(site)+"-"+escape.Replace(recordID))
}

func ingestTempRelPath(mediaDir, tempPath string) (string, error) {
	relPath, err := filepath.Rel(mediaDir, tempPath)
	if err != nil {
		return "", fmt.Errorf("making ingest temp path relative to media root: %w", err)
	}
	if relPath == "." || filepath.IsAbs(relPath) || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("ingest temp path %q is outside media root %q", tempPath, mediaDir)
	}
	return filepath.ToSlash(relPath), nil
}

func (w *IngestWorker) removeUnstartedIngestTemp(tempPath string, log *slog.Logger) {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "removing unstarted ingest temp"); err != nil {
		log.Warn("ingest: skipping cleanup because media store marker did not match", "err", err)
		return
	}
	if err := fsink.RemoveTemp(tempPath); err != nil {
		log.Warn("ingest: removing unstarted temp failed", "path", tempPath, "err", err)
	}
}

func shouldAllocateIngestTempGeneration(created bool, state *store.IngestTempState) bool {
	return created || state == nil
}

// linkIngestFile / syncIngestParentDir は確定プロトコルの OS 操作をテストから
// 観測・失敗注入できるようにする。実装の順序（DB INSERT → link → 親 dir fsync
// → DB commit → temp の unlink）を、ファイルシステムの実体に依存せず検証するためのフックである。
var linkIngestFile = fsink.PublishLink
var syncIngestParentDir = fsink.SyncParentDir

// beforeIngestFilePublication / afterIngestFilePublication はファイル公開 callback を
// テストから止めるためのフック。tx は store の内側に置き、フックへ渡さない。
var beforeIngestFilePublication = func() error { return nil }
var afterIngestFilePublication = func() error { return nil }

// afterIngestTempCreate は temp の作成直後（進捗行を書く前）に止まった状態をテストで再現する。
var afterIngestTempCreate = func() error { return nil }

// IngestWorker は mirakc からの TS ファイル転送を行う queue worker。
type IngestWorker struct {
	Enqueuer jobq.Enqueuer

	// MirakcClients は site → mirakc クライアントの map（issue #532。1 プロセスが
	// N site を束縛できるため、この 1 インスタンスが複数 site の ingest_<site>
	// キューを同時に購読しうる）。Work は verifySite で args.Site に対応する
	// クライアントを取り出してから使う。
	MirakcClients    map[string]*mirakc.Client
	Publisher        store.MediaPublish
	Precheck         store.IngestPrecheck
	RecordTarget     store.IngestTarget
	CMDetectionStore store.CMDetection
	MediaDir         string
	MediaStoreName   string
	CMDetect         config.CMDetectConfig

	// probeMu は storageProbed を守り、ProbeIngestStorage の同時実行を 1 本にする。
	// storageProbed は ProbeIngestStorage が成功済みかを示す。起動時に目印を確認できず
	// probe を保留したときだけ false で始まる（NewWorkers が Deps から渡す）。
	// sync.Once は最初の失敗を固定してしまうので使わない。
	probeMu       sync.Mutex
	storageProbed bool

	// CutProfiles は cut: true のプロファイル名（config から注入）。凍結時の
	// クランプ（resolveAndSnapshotEncodePolicy）に使う。
	CutProfiles map[string]struct{}
	// RecordingHLSEnabled は config.recording_hls.enabled。録画 HLS が使えない場合だけ、
	// 凍結時に cut-only の選択を安全側へクランプする。
	RecordingHLSEnabled bool

	// StallTimeout は転送中の無進捗検知タイムアウト（config.ingest.stall_timeout。
	// config.defaults() が既定値 30 秒を埋めるので、ここでは常に config が
	// 渡した値をそのまま使う）。
	StallTimeout time.Duration

	// ProgressInterval は recording_ingest_progress を書き直す最短間隔
	// （issue #212）。0 は「未設定」で ingestProgressInterval に解決する
	// （resolveProgressInterval）。テストが転送の途中経過を観測するために
	// 短くできるようにしてあるだけで、運用上は既定のままでよい。
	ProgressInterval time.Duration
}

// ensureStorageProbed は起動時に保留した ProbeIngestStorage を、目印の照合が通った後に 1 回だけ走らせる。
// probe は media_dir/sites を作る書き込みなので、照合より前に呼んではならない。
// 失敗は error で返して次の ingest ジョブに再試行させる。
// 置き場の名前が空の構成は照合しないので、従来どおり起動時の probe だけに任せる。
func (w *IngestWorker) ensureStorageProbed() error {
	if w.MediaStoreName == "" {
		return nil
	}
	w.probeMu.Lock()
	defer w.probeMu.Unlock()
	if w.storageProbed {
		return nil
	}
	if err := ProbeIngestStorage(w.MediaDir); err != nil {
		return fmt.Errorf("validating ingest storage: %w", err)
	}
	w.storageProbed = true
	return nil
}

// resolveProgressInterval は設定された ProgressInterval があればそれを、
// なければ既定の ingestProgressInterval を返す（config キーが無いフィールドの
// 「0 は未設定」規約。ProgressInterval の doc コメント参照）。
func (w *IngestWorker) resolveProgressInterval() time.Duration {
	if w.ProgressInterval == 0 {
		return ingestProgressInterval
	}
	return w.ProgressInterval
}

// Work は ingest ジョブを実行する。ストリーム取得・DB コミット・エッジ削除を行う。
// jobs.IngestJobArgs.Spec().Timeout が 1 試行を 5 分に制限する。4 分の replay / 転送区切りで
// temp と SHA-256 状態を保存して snooze するため、録画全体の長さでは締切が決まらない。
//
// 戻り値を名前付きにするのは、defer が「この試行の結末」を分類して
// metrics.IngestJobs / IngestDuration へ記録するためである（記録の値域と
// 中断を数えない理由は下の defer のコメントと
// docs/operations/monitoring.md の rokuban_ingest_jobs_total）。
func (w *IngestWorker) Work(ctx context.Context, args jobs.IngestJobArgs) (err error) {
	log := slog.With("site", args.Site, "record_id", args.RecordID)

	started := time.Now()
	result := "failure"
	skipMetrics := false
	defer func() {
		if skipMetrics {
			// 区切りと SHA-256 待ちは同じ job を続ける中間状態。
			return
		}
		// result は success / failure / canceled の 3 値。**この 3 値の外に増やさない**
		// （低カーディナリティが前提。record id や理由は入れない）。
		//
		ctxErr := ctx.Err()
		// 停止による中断は数えず、締切超過は試行失敗として数える。adapter が
		// context.Cause に独自の error を設定した場合も、その cause を運んだ
		// 失敗なら中断として扱う。Work が nil を返した場合は成功として記録する。
		cause := context.Cause(ctx)
		if ctxErr == context.Canceled && (errors.Is(err, context.Canceled) || (cause != nil && errors.Is(err, cause))) {
			return
		}
		if ctxErr == context.DeadlineExceeded {
			result = "failure"
		}
		metrics.IngestDuration.Observe(time.Since(started).Seconds())
		metrics.IngestJobs.WithLabelValues(result).Inc()
	}()

	// mirakc の record id はインスタンススコープ。他サイトのジョブをこの
	// プロセスの mirakc に投げると、別番組をこの recording としてコミットしうる
	// （issue #139）。DB 参照（lookupRecordingID）や mirakc/FS への一切の
	// アクセスより前に照合する。
	client, err := verifySite(w.MirakcClients, args.Site, jobs.IngestQueue)
	if err != nil {
		return err
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "starting ingest"); err != nil {
		return err
	}
	if err := w.ensureStorageProbed(); err != nil {
		return err
	}

	recordingID, expectedBytes, err := w.lookupIngestTarget(ctx, args)
	if err != nil {
		return fmt.Errorf("looking up recording_id: %w", err)
	}

	// 冪等性チェック：この recording_id の original media_asset が既にコミット
	// 済みなら転送をやり直さない。エッジ record の削除
	// （handleAlreadyCommittedIngest / enqueueIngestFollowups の DeleteRecord）は
	// 失敗してもログのみで ingest 成功扱いにしている（意図的）ため、mirakc 側に
	// record が残ったまま 5 分後の record_sweep → watcher.processRecord
	// （status=finished）経由で同じ record の ingest ジョブが再投入されうる。
	// pendingJobStates の UniqueOpts は completed を除外するのでこの再投入は
	// 止まらない。ここで止めないと新しい試行がコミット済み canonical file を
	// 置き換えて全量を再ダウンロードし、streamer は不変条件 3
	// （コミット = DB 行）で既にコミット済みの録画に対して欠けたファイルを
	// 配ることになる。
	alreadyCommitted, err := w.hasOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("checking existing media_asset: %w", err)
	}
	if alreadyCommitted {
		result = "success"
		w.handleAlreadyCommittedIngest(ctx, client, args, recordingID, log)
		return nil
	}
	if err := w.ingestResolvedRecord(ctx, client, args, recordingID, expectedBytes, log, &result); err != nil {
		if errors.Is(err, errIngestSHA256Pending) {
			skipMetrics = true
			return jobq.Snooze(ingestSHA256SnoozeInterval)
		}
		if errors.Is(err, errIngestSliceElapsed) {
			skipMetrics = true
			return jobq.Snooze(0)
		}
		return err
	}
	return nil
}

//nolint:funlen,gocognit // temp lock, generation, transfer, hash verification, and publication share one cleanup boundary.
func (w *IngestWorker) ingestResolvedRecord(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, expectedBytes *int64, log *slog.Logger, result *string) error {
	relPath, fullPath, initialRecord, err := w.determineRelPath(ctx, args, client)
	if err != nil {
		return fmt.Errorf("determining rel_path: %w", err)
	}

	// 既に同じ rel_path を使う行がある場合は、無駄な転送を始める前に拒む。
	// これは安価なヒントであり、ingest 同士の競合を決着させるものではない。
	// 本当の採用順序は commit 内の未コミット INSERT（unique index の予約）で
	// 保証する。
	if conflictRecordingID, err := w.checkRelPathConflict(ctx, relPath); err != nil {
		return fmt.Errorf("checking rel_path conflict: %w", err)
	} else if conflictRecordingID != 0 {
		return fmt.Errorf("ingest: rel_path %q is already used by another media_asset that has not been deleted (recording_id=%d); refusing to overwrite its file (recording_id=%d)",
			relPath, conflictRecordingID, recordingID)
	}
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "creating ingest directory"); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", filepath.Dir(fullPath), err)
	}

	tempPath := ingestTempFilePath(filepath.Dir(fullPath), args.Site, args.RecordID)
	tempRelPath, err := ingestTempRelPath(w.MediaDir, tempPath)
	if err != nil {
		return err
	}
	tempLock, err := lockExistingIngestTempFile(tempPath)
	tempCreated := false
	if errors.Is(err, os.ErrNotExist) {
		// 古い進捗行を残したまま temp を作ると、進捗を書く前に停止した再試行が
		// 作り直した temp へ古い世代を引き継ぐ。先に進捗行を消してから作る。
		if err := w.Publisher.ClearIngestProgress(ctx, recordingID); err != nil {
			return fmt.Errorf("clearing stale ingest progress before creating temp: %w", err)
		}
		if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "removing stale ingest checkpoint"); err != nil {
			return err
		}
		if err := fsink.RemoveCheckpoint(tempPath); err != nil {
			return fmt.Errorf("removing stale ingest checkpoint before creating temp: %w", err)
		}
		tempLock, tempCreated, err = lockIngestTempFile(tempPath)
	}
	if err != nil {
		return fmt.Errorf("locking ingest temporary file %s: %w", tempPath, err)
	}
	tempState, err := w.Publisher.GetIngestTempState(ctx, recordingID)
	if err != nil {
		if tempCreated {
			w.removeUnstartedIngestTemp(tempPath, log)
		}
		_ = tempLock.Close()
		return fmt.Errorf("getting ingest temp state: %w", err)
	}
	newTempGeneration := shouldAllocateIngestTempGeneration(tempCreated, tempState)
	tempGeneration := int64(0)
	if newTempGeneration {
		tempGeneration, err = w.Publisher.NextIngestTempGeneration(ctx)
		if err != nil {
			if tempCreated {
				w.removeUnstartedIngestTemp(tempPath, log)
			}
			_ = tempLock.Close()
			return fmt.Errorf("allocating ingest temp generation: %w", err)
		}
	} else {
		tempGeneration = tempState.TempGeneration
	}
	f, err := openIngestFile(tempPath, tempLock)
	if err != nil {
		if tempCreated {
			w.removeUnstartedIngestTemp(tempPath, log)
		}
		_ = tempLock.Close()
		return fmt.Errorf("opening ingest temporary file %s: %w", tempPath, err)
	}
	// temp はプロセス死・ctx キャンセル・一時的な I/O / DB 失敗では残す。次の
	// job delivery が同じファイルを replay して続けるためである。中身が悪いと確定
	// した場合だけ下の removeTemp を立てる。link 後の DB commit 失敗でも temp は残す。
	// canonical と同じ inode なので、次の試行が転送せずに commit だけをやり直す。
	removeTemp := false
	defer func() {
		if removeTemp {
			// temp・checkpoint の unlink は 1 回の照合でまとめて守る。
			if markerErr := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "discarding ingest temp"); markerErr != nil {
				log.Warn("ingest: skipping temp cleanup because media store marker did not match", "err", markerErr)
			} else {
				// tempLock は f の dup 元 fd で、commit 終了まで flock を保持する。
				// 別の試行が同名パスを作ってから古い cleanup がそれを消すことが
				// ないよう、ロックを保持したまま unlink してから lock fd を閉じる。
				if err := fsink.RemoveTemp(tempPath); err != nil {
					log.Warn("ingest: removing discarded temp failed", "path", tempPath, "err", err)
				}
				if err := w.Publisher.ClearIngestProgress(ctx, recordingID); err != nil {
					// 捨てた temp を指す進捗行を残さない。消せなくても次の試行が temp 無しを
					// 見て消すので、転送の失敗にはしない。
					log.Warn("ingest: clearing progress after discarding temp failed", "recording_id", recordingID, "err", err)
				}
			}
		}
		_ = f.Close()
		// openIngestFile のテスト差し替えが underlying fd を閉じずに失敗
		// する場合にもロックを残さない。
		_ = tempLock.Close()
	}()
	// 前の試行が link した後に commit で失敗した場合、temp は canonical と同じ inode である。
	// temp は O_APPEND で開いているので、ここから 1 バイトでも書くと公開済みの canonical が変わる。
	// mirakc からは再転送せず、SHA checkpoint の続きだけ読んで commit をやり直す。
	if fsink.SameFile(tempPath, fullPath) {
		info, err := tempLock.Stat()
		if err != nil {
			return fmt.Errorf("stat ingest temp already linked to canonical path: %w", err)
		}
		sliceCtx, cancelSlice := context.WithTimeout(ctx, ingestTransferSlice)
		defer cancelSlice()
		hasher := sha256.New()
		offset, complete, err := fsink.ReplayTempFileWithCheckpoint(sliceCtx, tempPath, f, hasher,
			func() error {
				return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "saving linked ingest checkpoint")
			},
		)
		if err != nil {
			if isIngestSliceDeadline(err, sliceCtx, ctx) {
				return errIngestSliceElapsed
			}
			return fmt.Errorf("hashing ingest temp already linked to canonical path: %w", err)
		}
		if !complete || offset != info.Size() {
			return fmt.Errorf("replaying linked ingest hash: complete=%v offset=%d size=%d", complete, offset, info.Size())
		}
		if err := fsink.WriteCheckpoint(tempPath, offset, hasher,
			func() error {
				return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "writing linked ingest checkpoint")
			},
		); err != nil {
			return fmt.Errorf("writing linked ingest checkpoint: %w", err)
		}
		log.Info("ingest: temp is already linked to canonical path, retrying commit only", "bytes", info.Size())
		return w.commitAndFinish(ctx, client, args, recordingID, relPath, tempPath, fullPath, info.Size(), hasher.Sum(nil), log, result)
	}
	progress := &ingestProgressReporter{
		publisher:      w.Publisher,
		recordingID:    recordingID,
		expectedBytes:  expectedBytes,
		tempRelPath:    tempRelPath,
		tempGeneration: tempGeneration,
		interval:       w.resolveProgressInterval(),
		log:            log,
	}
	if newTempGeneration {
		info, err := tempLock.Stat()
		if err != nil {
			return fmt.Errorf("stat ingest temp for generation %d: %w", tempGeneration, err)
		}
		if err := w.Publisher.UpsertIngestProgress(ctx, store.IngestProgress{
			RecordingID: recordingID, WrittenBytes: info.Size(), ExpectedBytes: expectedBytes,
			TempRelPath: tempRelPath, TempGeneration: tempGeneration,
		}); err != nil {
			if tempCreated {
				removeTemp = true
			}
			return fmt.Errorf("recording ingest temp generation: %w", err)
		}
	}

	// finished record の temp が HEAD の長さまで転送済みなら、replay する前に
	// ハッシュを再確認する。snooze 中も大きな temp を毎回読み直さない。
	preflightSHA256, waiting, err := w.preflightIngestSHA256(ctx, client, args.RecordID, initialRecord, tempPath, f, log)
	if err != nil {
		return err
	}
	if waiting {
		return errIngestSHA256Pending
	}

	// replay と pull を合わせて 1 区切りにし、既存 temp が大きい場合も途中で再開できる。
	// Args Spec の Timeout はこの区切りより 1 分長く、checkpoint の fsync と commit に使う。
	sliceCtx, cancelSlice := context.WithTimeout(ctx, ingestTransferSlice)
	defer cancelSlice()
	hasher := sha256.New()
	offset, replayComplete, err := fsink.ReplayTempFileWithCheckpoint(
		sliceCtx, tempPath, f, hasher,
		func() error {
			return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "saving replay ingest checkpoint")
		},
	)
	if err != nil {
		if isIngestSliceDeadline(err, sliceCtx, ctx) {
			progress.start(ctx, offset)
			return errIngestSliceElapsed
		}
		return err
	}
	if !replayComplete {
		return fmt.Errorf("replaying ingest temp ended without completion")
	}
	// 既存 temp の SHA-256 を復元してから、新規転送の受理バイトを追記先と hasher に流す。
	sink := fsink.NewHashingWriter(f, hasher)

	// 転送の途中経過を recording_ingest_progress に写す（issue #212）。行の存在
	// そのものが「転送中」の主張なので（不変条件 10）、1 バイトも流れる前に
	// 1 行書いてから始める --- 遅い回線で最初の 1 バイトが来るまで数十秒かかる
	// ことがあり、そこが「何も起きていないように見える」時間帯そのものだから。
	progress.start(ctx, offset)
	// progressWriter は hashingWriter の外側に置き、temp に受理されたバイト数を報告する。
	dst := &progressWriter{
		w:       sink,
		written: offset,
		onWrite: func(written int64) { progress.report(ctx, written) },
	}

	var expectedSHA256 *string
	var observedEndTime *mirakc.Milliseconds
	offset, expectedSHA256, observedEndTime, err = w.transferIngestRecord(sliceCtx, client, args.RecordID, dst, progress, offset)
	if err != nil {
		if isIngestSliceDeadline(err, sliceCtx, ctx) {
			if err := fsink.PersistCheckpoint(tempPath, f, offset, hasher,
				func() error {
					return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "persisting ingest checkpoint")
				}); err != nil {
				return err
			}
			progress.flush(ctx, offset)
			return errIngestSliceElapsed
		}
		if errors.Is(err, errIngestRecordEndedAbnormally) {
			removeTemp = true
			// 再試行に戻さない（errIngestRecordEndedAbnormally の doc コメント
			// 参照）。進捗行は上の removeTemp の cleanup が消す。
			// 取り消し・失敗は「転送が壊れた」ではないので、失敗として数えない。
			// 同じ result="failure" に混ぜると、利用者が止めた録画が失敗率に
			// 積まれて本物の失敗が埋もれる。
			*result = "canceled"
			return errors.Join(jobq.ErrDiscard, err)
		}
		return err
	}
	expectedLen, err := client.HeadRecordStream(ctx, args.RecordID)
	if err != nil {
		return fmt.Errorf("HEAD record stream: %w", err)
	}
	if expectedLen >= 0 && offset != expectedLen {
		removeTemp = true
		return fmt.Errorf("size mismatch: written=%d expected=%d", offset, expectedLen)
	}
	if expectedSHA256 == nil && preflightSHA256 != nil {
		expectedSHA256 = preflightSHA256
	}
	if expectedSHA256 == nil {
		var waiting bool
		expectedSHA256, waiting, err = w.maybeWaitForSHA256(ctx, client, args.RecordID, tempPath, f, expectedLen, offset, observedEndTime, log)
		if err != nil {
			return err
		}
		if waiting {
			// maybeWaitForSHA256 が temp を Sync した後なので、hash 状態をその末尾へ合わせて保存する。
			if err := fsink.WriteCheckpoint(tempPath, offset, hasher,
				func() error {
					return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "writing ingest checkpoint")
				}); err != nil {
				return err
			}
			return errIngestSHA256Pending
		}
	}
	sha256Verification := "skipped"
	expectedHash, hashAvailable := normalizeContentSHA256(expectedSHA256)
	if expectedSHA256 == nil {
		sha256Verification = "timeout_skipped"
	} else if !hashAvailable {
		sha256Verification = "invalid_skipped"
		log.Warn("ingest: skipping invalid content sha256", "value", *expectedSHA256)
	}
	if hashAvailable {
		sha256Verification = "verified"
		actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
		if actualSHA256 != expectedHash {
			removeTemp = true
			metrics.IngestHashMismatches.Inc()
			log.Warn("ingest: content sha256 mismatch", "actual", actualSHA256, "expected", expectedHash, "bytes", offset)
			return fmt.Errorf("hash mismatch: actual=%s expected=%s bytes=%d", actualSHA256, expectedHash, offset)
		}
	}

	// Linux では遅延した書き込みエラー（ENOSPC / I/O エラー）は Close() では
	// 上がらず、fsync() でしか報告されない。offset はここまで転送できたバイト数を
	// メモリ上で数えた値であって実際にディスクへ落ちたことの確認ではないので、
	// 上の Content-Length 照合もこの種の失敗を素通りしてしまう。ここで fsync が
	// 失敗したら DB へ登録せず、mirakc の record を保持して再試行させる。途中の fsync は
	// 区切りでだけ行い、各バッファごとの実体化は増やさない。
	syncStarted := time.Now()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	// commit 直前に、受信しながら計算したハッシュを checkpoint に残す。mirakc が SHA-256 を返したときは、
	// 照合済みの値である（返さなかったとき、sha256_verification が timeout_skipped / invalid_skipped なら
	// 照合していないが、受信しながら計算した値であることは変わらない）。
	// link の後で commit が失敗して再試行するとき、論理行へ書く値はこのハッシュであるべきで、
	// ディスクを読み直した値ではない。読み直すと、その間に化けたファイルの値が作り手の値として
	// 残る（SHA-256 を作り手が書く理由そのものが崩れる）。
	// この checkpoint は link の前に書き、失敗すれば link せずに返る。そのため link 済みの temp に有効な
	// checkpoint が無いのは、次の場合に限られる（fsink.RestoreCheckpoint の判定と、この書き込みの位置から読み取った）。
	//   - この変更より前のバイナリが link した temp。commit 直前の checkpoint が無いか、途中の slice の
	//     checkpoint しか無い（後者は offset がファイル末尾より前なので、残りだけ読み直す）。
	//   - checkpoint が壊れていて RestoreCheckpoint が無効と判断した場合。
	// その経路（上の linked 経路）はディスクを読み直した値を作り手の値として書くことになる。
	// 例外として許容する。
	if err := fsink.WriteCheckpoint(tempPath, offset, hasher,
		func() error {
			return requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "writing ingest checkpoint before publication")
		},
	); err != nil {
		return fmt.Errorf("writing ingest checkpoint before publication: %w", err)
	}

	log.Info("ingest: transfer complete", "bytes", offset,
		"sha256_verification", sha256Verification,
		"fsync_duration", time.Since(syncStarted))

	recordIngestMetrics(offset)

	return w.commitAndFinish(ctx, client, args, recordingID, relPath, tempPath, fullPath, offset, hasher.Sum(nil), log, result)
}

// commitAndFinish は原本を commit し、成功したら temp を片付けてエッジ record の後続処理を投入する。
// 呼び出し元は temp の flock を保持している。
//
// link した後の temp は canonical と同じ inode なので、DB commit が失敗した間は消さない。
// 消してよいのは commit 成功後だけで、その unlink は canonical を残したままリンク数を 1 に戻す。
// unlink に失敗しても ingest は成功として扱う（行がコミットされており、残った temp は孤児回収の対象）。
func (w *IngestWorker) commitAndFinish(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, relPath, tempPath, fullPath string, size int64, contentSHA256 []byte, log *slog.Logger, result *string) error {
	if err := w.commit(ctx, recordingID, relPath, tempPath, fullPath, size, contentSHA256); err != nil {
		if errors.Is(err, fsink.ErrForeignDestination) {
			// temp・mirakc の record・media_assets のいずれにも触れずに止まる。運用者の調査対象
			// （docs/runbook/troubleshooting.md）なので、他の失敗と区別できる key を付ける。
			log.Error("ingest: canonical path is occupied by a file that is not this ingest's temp",
				"canonical_conflict", true, "rel_path", relPath, "err", err)
		}
		return fmt.Errorf("committing ingest: %w", err)
	}
	w.removePublishedIngestTemp(tempPath, log)

	// エッジ record の削除は失敗しても ingest は成功（コミット済み）。
	*result = "success"

	w.enqueueIngestFollowups(ctx, client, args.RecordID, recordingID, log)

	return nil
}

// removePublishedIngestTemp は commit 済みの temp と checkpoint を消す。
// 呼び出し元は temp の flock を保持している。置き場の目印を照合してから消し、失敗はログだけにする。
func (w *IngestWorker) removePublishedIngestTemp(tempPath string, log *slog.Logger) {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "removing committed ingest temp"); err != nil {
		log.Warn("ingest: skipping committed temp cleanup because media store marker did not match", "err", err)
		return
	}
	if err := fsink.RemoveTemp(tempPath); err != nil {
		log.Warn("ingest: removing committed temp failed", "path", tempPath, "err", err)
	}
}

// preflightIngestSHA256 は完成済み temp が既に揃っている場合、再生前に SHA-256 待ちを判定する。
func (w *IngestWorker) preflightIngestSHA256(ctx context.Context, client *mirakc.Client, recordID string, initialRecord *mirakc.Record, tempPath string, file fsink.File, log *slog.Logger) (*string, bool, error) {
	if initialRecord.Recording.Status != store.RecordingStatusFinished || initialRecord.Content.Sha256 != nil {
		return nil, false, nil
	}
	expectedLen, err := client.HeadRecordStream(ctx, recordID)
	if err != nil {
		return nil, false, fmt.Errorf("HEAD record stream before temp replay: %w", err)
	}
	if expectedLen < 0 {
		return nil, false, nil
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat ingest temporary file before replay: %w", err)
	}
	if info.Size() != expectedLen {
		return nil, false, nil
	}
	return w.maybeWaitForSHA256(ctx, client, recordID, tempPath, file, expectedLen, info.Size(), initialRecord.Recording.EndTime, log)
}

// maybeWaitForSHA256 は完成済み temp のハッシュ待ち期限を判定する。
// 待つ場合は temp を同期し、呼び出し元に同じ job の snooze を返させる。進捗行は commit まで残す（世代を変えないため）。
func (w *IngestWorker) maybeWaitForSHA256(ctx context.Context, client *mirakc.Client, recordID string, tempPath string, file fsink.File, expectedLen, size int64, observedEndTime *mirakc.Milliseconds, log *slog.Logger) (*string, bool, error) {
	// HEAD の長さが不明なら転送済みを判定できない。content.length は照合の根拠に
	// 使わず、待たずに timeout_skipped で commit する。
	if expectedLen < 0 || size != expectedLen {
		return nil, false, nil
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return nil, false, fmt.Errorf("stat ingest temporary file before SHA-256 wait: %w", err)
	}
	now := time.Now()
	if !now.Before(ingestSHA256WaitDeadline(size, observedEndTime, info.ModTime(), now)) {
		return nil, false, nil
	}
	// Sync の間に届いたハッシュを拾うため、同期の後で record を取り直し、期限も再判定する。
	if err := file.Sync(); err != nil {
		return nil, false, fmt.Errorf("syncing ingest temp before SHA-256 wait: %w", err)
	}
	record, err := client.GetRecord(ctx, recordID)
	if err != nil {
		return nil, false, fmt.Errorf("refreshing mirakc record after syncing temp: %w", err)
	}
	if record.Recording.Status != store.RecordingStatusFinished {
		return nil, false, nil
	}
	if record.Content.Sha256 != nil {
		return record.Content.Sha256, false, nil
	}
	now = time.Now()
	endTime := record.Recording.EndTime
	if endTime == nil {
		endTime = observedEndTime
	}
	deadline := ingestSHA256WaitDeadline(size, endTime, info.ModTime(), now)
	if !now.Before(deadline) {
		return nil, false, nil
	}
	log.Info("ingest: content sha256 is pending; snoozing until recheck",
		"sha256_verification", "pending",
		"wait", time.Until(deadline),
		"deadline", deadline)
	return nil, true, nil
}

// handleAlreadyCommittedIngest は原本が既にある ingest の残務を処理する。
// 進捗行とエッジ record の削除失敗はログだけにして ingest を成功扱いにする。
//
// 転送は行っていないが、DB からは成功と区別が付かない状態なのでメトリクスも
// 成功として数える（呼び出し元が result="success" を設定する）。ここでの
// 唯一の残り仕事はエッジ record の削除（下記）の再試行であり、それが完了
// すれば ingest の目的（コミット済み・record 削除済み）は満たされている。
func (w *IngestWorker) handleAlreadyCommittedIngest(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, recordingID int64, log *slog.Logger) {
	log.Info("ingest: media_asset already committed, skipping transfer", "recording_id", recordingID)
	// 前回の実行が転送の途中で死に、その後別経路（internal/inplace.Register
	// など）で原本がコミットされた場合、進捗行だけが取り残される。原本行が
	// ある録画の進捗表示は API 側が無視する（原本の有無が真実。不変条件 5）
	// ので害は無いが、掃除できる場所で掃除しておく。
	if err := w.Publisher.ClearIngestProgress(ctx, recordingID); err != nil {
		log.Warn("ingest: failed to clear stale transfer progress", "recording_id", recordingID, "err", err)
	}
	w.removeLinkedTempAfterCommit(ctx, client, args, log)
	// 原本があるなら対象を絞った reconcile をヒントとして投入する。
	enqueueEncodeReconcile(ctx, w.Enqueuer, recordingID)
	if _, err := client.DeleteRecord(ctx, args.RecordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (already committed)", "err", err)
	}
}

// removeLinkedTempAfterCommit は、commit は成功したが temp の unlink に失敗した（または unlink 前に
// 止まった）ingest の temp を片付ける。temp が canonical と同じ inode の場合だけ消す。
// 同名で別の inode の temp は別の試行のものなので触らない。flock を取れなければ何もしない。
// 失敗はログだけにする。
func (w *IngestWorker) removeLinkedTempAfterCommit(ctx context.Context, client *mirakc.Client, args jobs.IngestJobArgs, log *slog.Logger) {
	_, fullPath, _, err := w.determineRelPath(ctx, args, client)
	if err != nil {
		// 前の試行が DeleteRecord まで終えていれば record は既に無い。通常の経路なので警告しない。
		var apiErr *mirakc.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			log.Debug("ingest: mirakc record is already gone; skipping committed temp cleanup", "err", err)
			return
		}
		log.Warn("ingest: could not resolve canonical path to clean up a committed temp", "err", err)
		return
	}
	tempPath := ingestTempFilePath(filepath.Dir(fullPath), args.Site, args.RecordID)
	if !fsink.SameFile(tempPath, fullPath) {
		return
	}
	lock, err := lockExistingIngestTempFile(tempPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !ingestTempLockBusy(err) {
			log.Warn("ingest: locking committed temp for cleanup failed", "path", tempPath, "err", err)
		}
		return
	}
	defer func() { _ = lock.Close() }()
	// flock を取る間に差し替えられていないかを確かめ直す。
	if !fsink.SameFile(tempPath, fullPath) {
		return
	}
	w.removePublishedIngestTemp(tempPath, log)
}

// transferIngestRecord は mirakc の追従 reader から record 固有の一時ファイルへ転送する。
// initialOffset は既存 temp を replay したバイト数で、プロセス再試行時の最初の
// Range 開始点になる。省略時は新規 temp の 0 バイトから始める。
//
// 書き込み先のエラーは mirakc の障害ではないので reader の再試行に入れず、そのまま返す。
func (w *IngestWorker) transferIngestRecord(ctx context.Context, client *mirakc.Client, recordID string, dst io.Writer, progress *ingestProgressReporter, initialOffset ...int64) (int64, *string, *mirakc.Milliseconds, error) {
	offset := int64(0)
	if len(initialOffset) > 0 {
		offset = initialOffset[0]
	}
	var expectedSHA256 *string
	var observedEndTime *mirakc.Milliseconds

	onRecord := func(record *mirakc.Record, currentOffset int64) error {
		status := record.Recording.Status
		if status == store.RecordingStatusCanceled || status == store.RecordingStatusFailed {
			return fmt.Errorf("mirakc record ended with status %q: %w", status, errIngestRecordEndedAbnormally)
		}
		if status != store.RecordingStatusRecording && status != store.RecordingStatusFinished {
			// The shared reader retries unknown statuses. Do not refresh progress
			// from a status it has not accepted.
			return nil
		}
		var expected *int64
		if record.Content.Length != nil {
			length := int64(*record.Content.Length)
			expected = &length
		}
		progress.observeProgress(ctx, currentOffset, expected)
		if status == store.RecordingStatusFinished && record.Content.Sha256 != nil {
			sha256 := *record.Content.Sha256
			expectedSHA256 = &sha256
		}
		if status == store.RecordingStatusFinished && record.Recording.EndTime != nil {
			endTime := *record.Recording.EndTime
			observedEndTime = &endTime
		}
		return nil
	}
	reader := mirakc.NewRecordFollowReader(ctx, client, recordID, offset, nil, mirakc.RecordFollowOptions{
		StallTimeout: w.StallTimeout,
		OnRecord:     onRecord,
		PollMin:      ingestFollowPace.pollMin,
		PollMax:      ingestFollowPace.pollMax,
		RetryDelay:   ingestFollowPace.retryDelay,
		// 正常に終わる Range では flush しない（間引きを無視すると追従中に秒 2 行になる）。
		OnRangeInterrupted: func(currentOffset, _ int64) {
			progress.flush(ctx, currentOffset)
		},
	})
	defer func() { _ = reader.Close() }()

	written, err := io.Copy(dst, reader)
	currentOffset := offset + written
	if err != nil {
		// io.Copy can fail because dst failed. That is a local storage error,
		// not a reason to retry the mirakc request. Keep the accepted byte count so a
		// slice deadline can persist the matching temp offset and hash state.
		return currentOffset, expectedSHA256, observedEndTime, err
	}
	return currentOffset, expectedSHA256, observedEndTime, nil
}

// normalizeContentSHA256 は mirakc の optional な SHA-256 表記を比較用の
// 小文字 hex に正規化する。空文字や非 hex の値は「使えるハッシュなし」として
// 扱う。値が不正な proxy / 旧実装で ingest 全体を永久に塞がないためである。
func normalizeContentSHA256(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	normalized := strings.ToLower(strings.TrimSpace(*value))
	if len(normalized) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", false
	}
	return normalized, true
}

// recordIngestMetrics は転送結果のバイト数をメトリクスへ記録する。
func recordIngestMetrics(offset int64) {
	metrics.IngestBytes.Add(float64(offset))
}

// enqueueIngestFollowups はコミット済み ingest の encode / thumbnail 対象 reconcile と
// mirakc record の削除を行う。補助処理の失敗はログに記録して本処理を成功扱いにする。
//
// encode 投入は対象録画を絞った reconcile。判定は定期パスと同じ query を使う。
//
// thumbnail 投入も対象録画を絞った reconcile で、poster・再選択・seek_tiles を
// 定期パスと同じ query で判定する。
// TS scan もヒント。候補の真実は periodic reconcile が DB から取り直す。
// すべて best-effort で、投入失敗はログに残して ingest の成否には影響させない。
func (w *IngestWorker) enqueueIngestFollowups(ctx context.Context, client *mirakc.Client, recordID string, recordingID int64, log *slog.Logger) {
	enqueueEncodeReconcile(ctx, w.Enqueuer, recordingID)
	if enqueueErr := EnqueueCMDetectionIfNeeded(ctx, w.CMDetectionStore, w.Enqueuer, recordingID, w.MediaStoreName); enqueueErr != nil {
		log.Error("ingest: failed to enqueue CM detection job", "recording_id", recordingID, "err", enqueueErr)
	}
	if enqueueErr := enqueueJob(ctx, w.Enqueuer, jobs.ThumbnailReconcileArgs{RecordingID: recordingID}); enqueueErr != nil {
		log.Error("ingest: failed to enqueue thumbnail reconcile", "recording_id", recordingID, "err", enqueueErr)
	}
	if enqueueErr := tsscan.EnqueueScan(ctx, w.Enqueuer, recordingID, w.MediaStoreName); enqueueErr != nil {
		log.Error("ingest: failed to enqueue TS scan", "recording_id", recordingID, "err", enqueueErr)
	}
	if _, err := client.DeleteRecord(ctx, recordID, true); err != nil {
		log.Error("ingest: failed to delete edge record (committed OK)", "err", err)
	}
}

// hasOriginalMediaAsset は recordingID に対する kind='original' の media_asset
// 行が既に存在するかを返す。ingest の冪等性チェックに使う（Work 参照）。
func (w *IngestWorker) hasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	has, err := w.Precheck.HasOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		return false, fmt.Errorf("querying media_assets: %w", err)
	}
	return has, nil
}

// checkRelPathConflict は relPath を既に使っている、まだ削除されていない
// （state <> 'deleted'。'active' に限らず、削除処理中の 'deleting' も含む）
// media_asset があれば、その recording_id を返す（無ければ 0, nil）。これは
// 転送前の安価なヒントであり、同時 ingest の決着ではない。採用の根拠は commit
// 内の media_asset_copies INSERT と部分一意索引である。
//
// 通常削除も同じ rel_path の filesystem lock の下で状態更新と unlink を行うため、
// 公開・削除の確定区間はこの SELECT と独立に直列化される。canonical 孤児は自動で
// unlink しない。ただしこの関数自体は転送前の安価なヒントであり、ingest 同士の
// 決着は commit 内の lock と media_asset_copies の一意索引に任せる。ここを一意性の
// 最終判定に使わない。
//
// WHERE state <> 'deleted' はその一意索引の述語と同じにする。削除済み
// （state='deleted'）の行が使っていた rel_path は正当に再利用できるので、
// ここで引っかけて誤って失敗させてはいけない。'deleting'（delete_reconcile の
// unlink 前後の中間状態）はまだ 'deleted' ではない --- unlink 前・unlink
// 失敗中は実ファイルが残っており、かつ resolveUnqualifiedDeletingAsset が
// ファイルの現存を確認した上でその行を active に戻しうる
// （delete_execute.go）ため、'deleting' の rel_path を ingest が上書きすると
// 「DB は active、実体は別番組」が再生産される。したがって 'deleting' も
// 'active' と同じく衝突として扱う。
func (w *IngestWorker) checkRelPathConflict(ctx context.Context, relPath string) (int64, error) {
	conflictRecordingID, err := w.Precheck.GetLiveMediaAssetRecordingID(ctx, relPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("querying media_assets: %w", err)
	}
	return conflictRecordingID, nil
}

// lookupIngestTarget は record_sync から転送先の recording_id と、進捗の分母に
// 使う content_length を 1 回のクエリで読む。
//
// content_length は watcher が mirakc record から観測した値（record.content.length）
// で、mirakc が返さなければ nil。分母をここから取るのは、転送中に使える唯一の
// 材料だから --- HEAD の Content-Length は転送完了後の照合（層 3）にしか取って
// おらず、ファイル stat は api ロールが読めない（不変条件 1）。nil のときは
// 進捗をバイト数だけで出し、% は出さない（issue #212）。
func (w *IngestWorker) lookupIngestTarget(ctx context.Context, args jobs.IngestJobArgs) (recordingID int64, expectedBytes *int64, err error) {
	row, err := w.RecordTarget.GetRecordSyncIngestTarget(ctx, args.Site, args.RecordID)
	if err != nil {
		return 0, nil, fmt.Errorf("querying record_sync: %w", err)
	}
	if row.RecordingID == nil {
		return 0, nil, fmt.Errorf("record_sync (%s, %s) has no recording_id", args.Site, args.RecordID)
	}
	return *row.RecordingID, row.ContentLength, nil
}

// determineRelPath は保存先の相対パスと、それを解決した絶対パスを返す。
// relPath は mirakc の contentPath 由来なので、メディアディレクトリの外を
// 指していないことを検証する。`sites/{site}/` を前置する判断（前置する理由・
// 固定の 1 段目を挟む理由・前置前の既存行を移行しない判断）は
// docs/storage/contract.md §rel_path の名前空間にある。
//
// 前置に使うのは args.Site。Work は determineRelPath を呼ぶ前に verifySite
// （internal/worker/worker.go）で args.Site が w.MirakcClients のいずれかの
// キーと一致することを検査済みである。w.MirakcClients のキーは常に実際の
// （空でない）site 名なので（config のバリデーションが site 名の非空を要求する。
// verifySite は jobSite を正規化しない）、verifySite を通過した args.Site は
// 常に非空である。
//
// client は verifySite が返したクライアント（Work が呼び出し元で解決済み）を
// そのまま受け取る --- ここで再度 w.MirakcClients を引くと、verifySite が通した
// site と実際に使うクライアントがズレる経路を作ってしまう。
//
// contentPath / Content.Path がどちらも空だと relPath が "."（カレント
// ディレクトリ）になる。前置後は "sites/{site}/." が Join/Clean で "." が
// 消えて "sites/{site}" という一見正当なパスになり mediapath.Resolve を
// 通ってしまい、一時ファイル作成が "{media_dir}/sites/{site}" を通常ファイルとして
// 作ってしまう（以後その site 配下の ingest が全て MkdirAll で
// "not a directory" になる。docs/storage/contract.md §rel_path の名前空間
// 参照）。前置前に弾く（下記）。
func (w *IngestWorker) determineRelPath(ctx context.Context, args jobs.IngestJobArgs, client *mirakc.Client) (relPath, fullPath string, record *mirakc.Record, err error) {
	record, err = client.GetRecord(ctx, args.RecordID)
	if err != nil {
		return "", "", nil, fmt.Errorf("getting mirakc record: %w", err)
	}
	if cp := record.Recording.Options.ContentPath; cp != nil && *cp != "" {
		relPath = *cp
	} else {
		relPath = filepath.Base(record.Content.Path)
	}
	// 空の rel_path と予約名は、mirakc 由来か移行ライブラリ由来かによらず
	// catalog.SiteRelPath で同じように拒否する。
	relPath, err = catalog.SiteRelPath(args.Site, relPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("building site rel_path for mirakc record %s: %w", args.RecordID, err)
	}
	fullPath, err = mediapath.Resolve(w.MediaDir, relPath)
	if err != nil {
		return "", "", nil, err
	}
	return relPath, fullPath, record, nil
}

// commit は原本の公開プロトコルを 1 回実行する。
//
// rel_path lock を保持し、論理資産とコピーの行予約・link・親ディレクトリ fsync・commit を
// store の 1 transaction に渡す。store は DB transaction を先に開始し、2 つの行を作った後に
// callback を呼ぶ。DB 接続が失われても goroutine のファイル操作は続きうるため、この関数が返るまで
// rel_path lock を保持する。
// 進捗行は原本の行と同じ tx で消す。コミット = DB 行なので、原本の行が生まれる瞬間に進捗行が消え、
// 「原本があるのに取り込み中」という中間状態が読み手から見えない。
//
// 公開は fsink.PublishLink で、宛先に別のファイルがあれば fsink.ErrForeignDestination で止まる。
// そのとき tx は rollback され、temp も canonical も変わらない。link 後の fsync / DB commit が
// 失敗した場合も canonical file は消さず、temp も残す（呼び出し側が commit 成功後にだけ unlink する）。
// 次の試行は temp と canonical が同じ inode であることを見て、転送せずにこの関数だけをやり直す。
// link 前の失敗では、中身の不一致や record の cancel / fail と確定した場合だけ呼び出し側が
// tempPath を消し、それ以外は次の試行へ残す。
func (w *IngestWorker) commit(ctx context.Context, recordingID int64, relPath, tempPath, fullPath string, size int64, contentSHA256 []byte) error {
	if err := requireMediaStoreMarker(w.MediaDir, w.MediaStoreName, "locking original for publication"); err != nil {
		return err
	}
	fileLock, err := lockMediaRelPathFile(ctx, w.MediaDir, relPath)
	if err != nil {
		return fmt.Errorf("locking canonical file protocol: %w", err)
	}
	defer func() { _ = fileLock.Close() }()
	return w.Publisher.CommitOriginal(ctx, store.CommitOriginalInput{
		RecordingID: recordingID, RelPath: relPath, SizeBytes: size, SHA256: contentSHA256,
	}, w.resolveEncodePolicy, func() error {
		if err := beforeIngestFilePublication(); err != nil {
			return fmt.Errorf("preparing canonical file publication: %w", err)
		}
		if err := linkIngestFile(tempPath, fullPath); err != nil {
			return fmt.Errorf("linking ingest temporary file into canonical path: %w", err)
		}
		if err := syncIngestParentDir(fullPath); err != nil {
			return fmt.Errorf("syncing canonical parent directory: %w", err)
		}
		if err := afterIngestFilePublication(); err != nil {
			return fmt.Errorf("finishing canonical file publication: %w", err)
		}
		return nil
	})
}

// resolveEncodePolicy は store が読み取った予約・上書き・意図から、この録画の凍結方針を導出する純粋関数である。
// DB の読み取りと凍結の保存は CommitOriginal の tx が担い、この関数は DB に触れない。
// 方針の行は原本の行と同じ tx で書かれるので、ここで返す値が CHECK に反すると原本ごとロールバックして録画が消える。
// 書く前に、反する組み合わせを安全側へ倒す。
// 凍結する瞬間・予約を放送イベントキーで引く理由・解決に失敗しても凍結する理由・
// source 別のログレベル・クランプの判断は docs/storage/retention.md §6 にある。
func (w *IngestWorker) resolveEncodePolicy(in store.EncodePolicyInputs) (store.EncodePolicySnapshot, error) {
	recordingID := in.RecordingID
	keepOriginal := "always"
	encodeProfiles := []string{}
	if in.Reservation == nil {
		// 予約が引けなくても凍結自体はスキップせず、既定値（'always' / 空）で凍結する。
		// 行の存在が「凍結済み」を意味するので、何も書かないと後続の reconcile が「未凍結」と誤認し、
		// 事後追加も「行が既にある」ことを前提にできなくなる。
		// source='rule' は予約があったのに引けなくなった場合、source='manual' は意図があったことを示す
		// snapshot なので Warn にする。source='unattributed' は予約も意図も特定できない録画で、
		// 予約が最初から無い日常的なケースなので Info に落とす。どの source でも黙って戻らない。
		// 判別できないことを、ログの欠落で埋め合わせない。
		logArgs := []any{
			"recording_id", recordingID,
			"site", in.Site,
			"network_id", in.NetworkID,
			"service_id", in.ServiceID,
			"event_id", in.EventID,
		}
		if in.Source == reservation.SourceUnattributed {
			slog.Info("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		} else {
			slog.Warn("encode policy: reservation not found via broadcast event key; freezing defaults", logArgs...)
		}
	} else {
		eff, err := reservation.EffectiveOptions(in.Reservation.Base, in.Reservation.Overrides, in.Reservation.IntentAction)
		if err != nil {
			return store.EncodePolicySnapshot{}, fmt.Errorf("computing effective options for program %d: %w", in.Reservation.ProgramID, err)
		}
		if eff.KeepOriginal != nil {
			keepOriginal = *eff.KeepOriginal
		}
		if eff.EncodeProfiles != nil {
			encodeProfiles = *eff.EncodeProfiles
		}
	}

	// cut だけになったときのクランプ。ルール単独・上書き単独ではそれぞれ録画 HLS 無効時の規則を満たしていても、
	// マージ結果として cut だけが生成されうる。録画 HLS を使えない構成では、確認に再生が要り、
	// 再生に encode が要り、encode に確認が要る循環になるので、cut のプロファイルを落とす。
	// 意図は上書きに残るので、利用者が cut でないプロファイルを足せば戻る。
	//
	// このクランプを下の until_encoded クランプより先に置く。落とした結果 desired が空になった場合
	// （cut だけのルール）は、下のクランプが keepOriginal を安全側へ倒す。
	// ここで別の分岐を書くと、「cut を落とした後だけ効く規則」がもう 1 つ増える。
	if err := config.ValidateCutSelection(encodeProfiles, w.CutProfiles, w.RecordingHLSEnabled); err != nil {
		slog.Warn("encode policy: frozen profile selection is cut-only; dropping cut profiles",
			"recording_id", recordingID, "encode_profiles", encodeProfiles)
		kept := make([]string, 0, len(encodeProfiles))
		for _, name := range encodeProfiles {
			if _, isCut := w.CutProfiles[name]; !isCut {
				kept = append(kept, name)
			}
		}
		encodeProfiles = kept
	}
	// until_encoded は desired が 1 つ以上あることを CHECK が要求する。
	// ルール単独・上書き単独では禁則を満たしていても、マージ結果としてこの組み合わせが生成されうる。
	if keepOriginal == "until_encoded" && len(encodeProfiles) == 0 {
		keepOriginal = "always"
	}
	return store.EncodePolicySnapshot{
		KeepOriginal:   keepOriginal,
		EncodeProfiles: encodeProfiles,
		CMDetect:       w.CMDetect.Enabled,
	}, nil
}
